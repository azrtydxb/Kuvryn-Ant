// Command ac6 runs the same artifact integrity + lifecycle scenario against one storage backend (AC6).
//
//	ac6 -name kw-minio -base https://ant.kw.watteel.lab -token-file ~/.kuvryn-ant/kw-automation-token.txt \
//	    -s3 kw -s3poke ./s3poke -out evidence/2026-10-18/ac6/kw-minio.json
//	ac6 -name local-disk -base http://127.0.0.1:PORT -token-file STATE/token -disk-root STATE/storage -out ...
//
// Steps are identical for every backend: publish (including a multipart-sized object), read + SHA-256, range
// reads, immutable conflict, OCI push/pull/validate, management delete/unlist/relist, delete-policy denial, GC
// dry run, real integrity scan, injected bit-rot detection, repair and rescan. The feeds it creates are deleted.
package main

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/bench/antlib"
)

type obj = antlib.Obj

var (
	name     = flag.String("name", "", "backend label")
	base     = flag.String("base", "", "instance base URL")
	tokFile  = flag.String("token-file", "", "credential file")
	s3       = flag.String("s3", "none", "s3poke target (oke|kw|none)")
	diskRoot = flag.String("disk-root", "", "disk blob root (local backend)")
	s3poke   = flag.String("s3poke", "", "directory (or binary) of the helper named exactly s3poke; put on PATH")
	out      = flag.String("out", "", "result JSON")
	bigMiB   = flag.Int("big-mib", 66, "size of the multipart-sized object in MiB")
	maxScans = flag.Int("max-scan-runs", 15, "max integrity passes")
)

var steps []obj

func step(n string, ok bool, detail string) bool {
	r := "pass"
	if !ok {
		r = "FAIL"
	}
	steps = append(steps, obj{"name": n, "result": r, "detail": cut(detail, 600)})
	fmt.Printf("%s %s %s\n", map[bool]string{true: "PASS", false: "FAIL"}[ok], n, cut(detail, 200))
	return ok
}

func info(n string, detail any) {
	d := fmt.Sprint(detail)
	if b, err := json.Marshal(detail); err == nil {
		if _, isStr := detail.(string); !isStr {
			d = string(b)
		}
	}
	steps = append(steps, obj{"name": n, "result": "info", "detail": cut(d, 900)})
	fmt.Println("INFO", n, cut(d, 200))
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func must(o obj, err error) obj { return antlib.Must(o, err) }

func main() {
	flag.Parse()
	if d := *s3poke; d != "" {
		if fi, err := os.Stat(d); err == nil && !fi.IsDir() {
			d = filepath.Dir(d)
		}
		if err := os.Setenv("PATH", d+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
			panic(err)
		}
	}
	token, err := antlib.TokenFor(*tokFile)
	if err != nil {
		panic(err)
	}
	api := antlib.New(*base, token)
	runID := fmt.Sprintf("%x", time.Now().Unix())
	slug := "ac6-" + runID
	info("environment", obj{"backend": *name, "base": *base, "started": antlib.Now()})
	st := must(api.OK("GET", "/api/v1/status", nil))
	info("blob_storage readiness", st.Obj("readiness").Obj("blob_storage"))
	if su, err := api.OK("GET", "/api/v1/storage/usage", nil); err == nil {
		info("storage.usage", fmt.Sprintf("backend=%s blobs=%d bytes=%d", su.Str("backend"), su.Int("blob_count"), su.Int("blob_bytes")))
	}
	feed, _ := api.MakeFeed(slug, "raw")
	dp := must(api.OK("GET", "/api/v1/feeds/"+feed.Str("id")+"/delete-policy", nil))
	must(api.OK("PUT", "/api/v1/feeds/"+feed.Str("id")+"/delete-policy", obj{"mode": "allowed", "immutable_releases": false, "revision": dp.Int("revision")}))

	// ---- publish and read back
	sizes := []struct {
		k string
		n int
	}{{"empty", 0}, {"one-byte", 1}, {"4k", 4096}, {"1m", 1<<20 + 17}, {"8m", 8<<20 + 3}, {"big", *bigMiB<<20 + 5}}
	data := map[string][]byte{}
	for _, s := range sizes {
		data[s.k] = antlib.Det("ac6:"+s.k, s.n)
		t0 := time.Now()
		code := api.RawPut(slug, "obj/"+s.k+".bin", data[s.k])
		step(fmt.Sprintf("publish %s (%d B)", s.k, s.n), code == 201 || code == 200, fmt.Sprintf("http %d in %.1fs", code, time.Since(t0).Seconds()))
	}
	for _, s := range sizes {
		t0 := time.Now()
		code, _, b := api.RawGet(slug, "obj/"+s.k+".bin", "")
		step("read+sha256 "+s.k, code == 200 && antlib.SHA(b) == antlib.SHA(data[s.k]) && len(b) == s.n, fmt.Sprintf("http %d len=%d in %.1fs", code, len(b), time.Since(t0).Seconds()))
	}
	// ---- ranges on the big object
	big := data["big"]
	bn := len(big)
	part := 64 << 20
	for _, r := range []struct {
		label string
		hdr   string
		lo    int
		hi    int
	}{
		{"first 100 B", "bytes=0-99", 0, 99},
		{"middle 1 KiB", fmt.Sprintf("bytes=%d-%d", bn/2, bn/2+1023), bn / 2, bn/2 + 1023},
		{"across 64 MiB part boundary", fmt.Sprintf("bytes=%d-%d", part-4096, part+4095), part - 4096, part + 4095},
		{"open-ended last 10 B", fmt.Sprintf("bytes=%d-", bn-10), bn - 10, bn - 1},
		{"suffix 7 B", "bytes=-7", bn - 7, bn - 1},
	} {
		if r.hi >= bn {
			continue
		}
		code, h, b := api.RawGet(slug, "obj/big.bin", r.hdr)
		cr := h.Get("Content-Range")
		step("range "+r.label, code == 206 && bytes.Equal(b, big[r.lo:r.hi+1]) && strings.HasPrefix(cr, fmt.Sprintf("bytes %d-%d/%d", r.lo, r.hi, bn)), fmt.Sprintf("http %d content-range=%s", code, cr))
	}
	code, _, _ := api.RawGet(slug, "obj/big.bin", fmt.Sprintf("bytes=%d-%d", bn+10, bn+20))
	step("range unsatisfiable -> 416", code == 416, fmt.Sprintf("http %d", code))
	// ---- immutable conflict
	c1 := api.RawPut(slug, "obj/4k.bin", antlib.Det("ac6:other", 4096))
	c2 := api.RawPut(slug, "obj/4k.bin", data["4k"])
	_, _, b := api.RawGet(slug, "obj/4k.bin", "")
	step("immutable: different content rejected, original intact", (c1 == 409 || c1 == 412 || c1 == 400 || c1 == 403) && antlib.SHA(b) == antlib.SHA(data["4k"]), fmt.Sprintf("conflict http %d; identical re-put http %d", c1, c2))

	// ---- OCI image with crane
	host := strings.SplitN(*base, "://", 2)[1]
	insecure := strings.HasPrefix(*base, "http://")
	ociSlug := "ac6o-" + runID
	ofeed, _ := api.MakeFeed(ociSlug, "oci")
	dcfg, _ := os.MkdirTemp("", "ac6-docker-")
	cfg, _ := json.Marshal(obj{"auths": obj{host: obj{"auth": base64.StdEncoding.EncodeToString([]byte("service:" + token))}}})
	_ = os.WriteFile(filepath.Join(dcfg, "config.json"), cfg, 0o600)
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	for i, n := range []int{3 << 20, 5 << 20} {
		_ = tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("f%d.bin", i), Size: int64(n), Mode: 0o644})
		_, _ = tw.Write(antlib.Det(fmt.Sprintf("ac6:oci%d", i), n))
	}
	_ = tw.Close()
	layer := filepath.Join(dcfg, "layer.tar")
	_ = os.WriteFile(layer, tb.Bytes(), 0o600)
	ref := fmt.Sprintf("%s/%s/demo:v1", host, ociSlug)
	crane := func(args ...string) (int, string) {
		if insecure {
			args = append(args, "--insecure")
		}
		c := exec.Command("crane", args...)
		c.Env = append(os.Environ(), "DOCKER_CONFIG="+dcfg)
		o, err := c.CombinedOutput()
		rc := 0
		if err != nil {
			rc = 1
		}
		return rc, strings.ReplaceAll(strings.TrimSpace(string(o)), token, "***")
	}
	rc, o := crane("append", "--oci-empty-base", "-f", layer, "-t", ref)
	step("oci: push (crane append, 8 MiB layer)", rc == 0, o)
	rc, dig := crane("digest", ref)
	step("oci: digest resolves", rc == 0 && strings.HasPrefix(dig, "sha256:"), dig)
	pulled := filepath.Join(dcfg, "pull.tar")
	rc, o = crane("pull", ref, pulled)
	fi, _ := os.Stat(pulled)
	step("oci: pull + client-side digest verification", rc == 0 && fi != nil && fi.Size() > 8<<20, o)
	rc, o = crane("validate", "--remote", ref)
	step("oci: crane validate --remote (re-reads every blob and verifies digests)", rc == 0, o)

	// ---- management lifecycle
	pk := must(api.OK("GET", "/api/v1/feeds/"+feed.Str("id")+"/packages?limit=100", nil))
	find := func(key string) obj {
		for _, i := range pk.Items() {
			if i.Str("coordinate") == key {
				return i
			}
		}
		return nil
	}
	version := func(comp obj) string {
		v := must(api.OK("GET", "/api/v1/feeds/"+feed.Str("id")+"/packages/"+comp.Str("id")+"/versions", nil)).Items()
		if len(v) == 0 {
			return "first"
		}
		return v[0].Str("version")
	}
	mut := func(comp obj, v, action string) (int, string) {
		p := "/api/v1/feeds/" + feed.Str("id") + "/packages/" + comp.Str("id") + "/versions/" + v
		if action != "" {
			p += "?action=" + action
		}
		s, _, d, _ := api.Req("DELETE", p, nil)
		return s, string(d)
	}
	if p := find("obj/4k.bin"); p != nil {
		v := version(p)
		s, d := mut(p, v, "unlist")
		step("lifecycle: unlist version accepted", s == 200, fmt.Sprintf("http %d %s", s, cut(d, 150)))
		rs, _, _ := api.RawGet(slug, "obj/4k.bin", "")
		info("read after unlist", fmt.Sprintf("http %d (unlisted content: direct read behaviour)", rs))
		s, d = mut(p, v, "relist")
		step("lifecycle: relist version", s == 200, fmt.Sprintf("http %d %s", s, cut(d, 150)))
		rs, _, rb := api.RawGet(slug, "obj/4k.bin", "")
		step("read after relist intact", rs == 200 && antlib.SHA(rb) == antlib.SHA(data["4k"]), fmt.Sprintf("http %d", rs))
	} else {
		step("lifecycle: 4k package listed", false, "missing")
	}
	if p := find("obj/8m.bin"); p != nil {
		s, d := mut(p, version(p), "")
		step("lifecycle: delete version (8m)", s == 200, fmt.Sprintf("http %d %s", s, cut(d, 150)))
		rs, _, _ := api.RawGet(slug, "obj/8m.bin", "")
		step("deleted object -> 404", rs == 404, fmt.Sprintf("http %d", rs))
		rs, _, rb := api.RawGet(slug, "obj/1m.bin", "")
		step("neighbouring object unaffected", rs == 200 && antlib.SHA(rb) == antlib.SHA(data["1m"]), fmt.Sprintf("http %d", rs))
	} else {
		step("lifecycle: 8m package listed", false, "missing")
	}
	dp = must(api.OK("GET", "/api/v1/feeds/"+feed.Str("id")+"/delete-policy", nil))
	must(api.OK("PUT", "/api/v1/feeds/"+feed.Str("id")+"/delete-policy", obj{"mode": "denied", "immutable_releases": false, "revision": dp.Int("revision")}))
	if p := find("obj/1m.bin"); p != nil {
		s, d := mut(p, version(p), "")
		step("delete policy=denied blocks delete", s == 403 || s == 409, fmt.Sprintf("http %d %s", s, cut(d, 150)))
	}

	// ---- GC dry run + integrity scan
	run := func(dry bool) (obj, string) {
		r, err := api.OK("POST", "/api/v1/retention/runs", obj{"dry_run": dry})
		if err != nil {
			return nil, err.Error()
		}
		for i := 0; i < 300; i++ {
			g := must(api.OK("GET", "/api/v1/retention/runs/"+r.Str("id"), nil))
			if g.Str("state") != "running" {
				return g, ""
			}
			time.Sleep(time.Second)
		}
		return nil, "timeout"
	}
	r, e := run(true)
	rep := obj{}
	if r != nil {
		rep = r.Obj("report")
	}
	rj, _ := json.Marshal(rep)
	step("gc dry-run completes, sweeps nothing", r != nil && r.Str("state") == "succeeded" && rep.Int("blobs_swept") == 0, string(rj)+e)
	dig1m := antlib.SHA(data["1m"])
	runs := 0
	var rp obj
	for i := 0; i < *maxScans; i++ {
		r, _ = run(false)
		runs++
		if r != nil {
			rp = r.Obj("report")
			if rp.Bool("integrity_wrapped") {
				break
			}
		}
	}
	rj, _ = json.Marshal(rp)
	step("integrity scan completes a full pass without findings on our fresh blobs", r != nil && rp.Int("integrity_mismatches") == 0, fmt.Sprintf("runs=%d last=%s", runs, rj))
	fx := must(api.OK("GET", "/api/v1/retention/integrity-findings?limit=100", nil))
	info("open integrity findings before injection (whole instance)", len(fx.Items()))

	locate := func(d string) (string, []string) {
		if *s3 != "none" {
			o, _ := exec.Command("s3poke", *s3, "ls", "").Output()
			var keys []string
			for _, l := range strings.Split(string(o), "\n") {
				if k := strings.Split(l, "\t")[0]; strings.HasSuffix(k, "/objects/"+d) {
					keys = append(keys, k)
				}
			}
			return "s3", keys
		}
		o, _ := exec.Command("find", *diskRoot, "-name", d, "-type", "f").Output()
		return "disk", strings.Fields(string(o))
	}
	finding := func() bool {
		f := must(api.OK("GET", "/api/v1/retention/integrity-findings?limit=100", nil))
		for _, i := range f.Items() {
			if strings.HasSuffix(i.Str("digest"), dig1m) {
				return true
			}
		}
		return false
	}
	kind, locs := locate(dig1m)
	info("blob location", fmt.Sprintf("%s %v", kind, locs))
	if len(locs) > 0 {
		tmp := filepath.Join(dcfg, "blob.orig")
		write := func(p string, d []byte) {
			if kind == "s3" {
				f := filepath.Join(dcfg, "blob.tmp")
				_ = os.WriteFile(f, d, 0o600)
				if err := exec.Command("s3poke", *s3, "put", locs[0], f).Run(); err != nil {
					panic(err)
				}
			} else {
				_ = os.Chmod(locs[0], 0o600)
				_ = os.WriteFile(locs[0], d, 0o600)
			}
		}
		var orig []byte
		if kind == "s3" {
			_ = exec.Command("s3poke", *s3, "get", locs[0], tmp).Run()
			orig, _ = os.ReadFile(tmp)
		} else {
			orig, _ = os.ReadFile(locs[0])
		}
		bad := append([]byte(nil), orig...)
		bad[len(bad)/2] ^= 0xFF
		write(locs[0], bad)
		step("fault injected: one byte flipped in stored blob (same size)", true, kind)
		rs, _, rb := api.RawGet(slug, "obj/1m.bin", "")
		info("read of corrupted blob", fmt.Sprintf("http %d len=%d sha256_matches_original=%v", rs, len(rb), antlib.SHA(rb) == dig1m))
		detected := false
		for i := 0; i < *maxScans && !detected; i++ {
			r, _ = run(false)
			if r != nil && r.Obj("report").Int("integrity_mismatches") > 0 {
				detected = true
				j, _ := json.Marshal(r.Obj("report"))
				info("scan run that detected it", string(j))
			}
		}
		step("integrity scan detects the corrupted blob and records a finding", detected && finding(), "")
		write(locs[0], orig)
		rs, _, rb = api.RawGet(slug, "obj/1m.bin", "")
		step("after repair the object reads correctly again", rs == 200 && antlib.SHA(rb) == dig1m, fmt.Sprintf("http %d", rs))
		cleared, passes := false, 0
		for i := 0; i < *maxScans+3 && !cleared; i++ {
			run(false)
			passes++
			cleared = !finding()
		}
		step("finding resolves automatically after repair + clean pass", cleared, fmt.Sprintf("passes=%d", passes))
	} else {
		step("locate stored blob for fault injection", false, "not found")
	}
	for _, f := range []obj{feed, ofeed} {
		s, _, _, _ := api.Req("DELETE", "/api/v1/feeds/"+f.Str("id"), nil, antlib.Opt{Rev: api.Rev("feeds", f.Str("id"))})
		info("cleanup feed "+f.Str("id"), fmt.Sprintf("http %d", s))
	}
	res := obj{"backend": *name, "base": *base, "run_id": runID, "finished": antlib.Now(), "steps": steps}
	_ = os.MkdirAll(filepath.Dir(*out), 0o755)
	b2, _ := json.MarshalIndent(res, "", " ")
	_ = os.WriteFile(*out, b2, 0o644)
	fails := 0
	for _, s := range steps {
		if s.Str("result") == "FAIL" {
			fails++
		}
	}
	fmt.Printf("DONE: %d steps, %d FAIL\n", len(steps), fails)
}
