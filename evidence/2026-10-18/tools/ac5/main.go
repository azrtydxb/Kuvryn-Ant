// Command ac5 drives the AC5 two-instance replication evidence run: OKE is the authority, kw replicates.
//
//	ac5 setup  <state.json> <docker-config-dir> [sfx]      create feeds/token on OKE and seed content
//	ac5 seed   <state.json> <timeline.jsonl> <sfx> <run> [oci-only]
//	ac5 ctl    disable|delete <job-id>... | delfeed <slug>... | status
//	ac5 verify <state.json> <kw-ca.pem> <out.json> [names]  digest-verify every replicated artifact
//	ac5 incr   <state.json> <out.json> <s3poke-binary>      incremental sync, lag, corruption, outage, deletion dry-run
//
// The state file holds the replicate-scoped token, so it must live outside the repository.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/bench/antlib"
)

type obj = antlib.Obj

const nsAnt = "kuvryn-ant"

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ac5:", err)
		os.Exit(1)
	}
}

func readJSON(path string, v any) {
	b, err := os.ReadFile(path)
	die(err)
	die(json.Unmarshal(b, v))
}

func writeJSON(path string, v any, mode os.FileMode) {
	b, _ := json.MarshalIndent(v, "", " ")
	die(os.WriteFile(path, b, mode))
}

func main() {
	if len(os.Args) < 2 {
		die(fmt.Errorf("usage: ac5 setup, seed, ctl, verify or incr"))
	}
	a := os.Args[2:]
	switch os.Args[1] {
	case "setup":
		setup(a)
	case "seed":
		seed(a)
	case "ctl":
		ctl(a)
	case "verify":
		verify(a)
	case "incr":
		incr(a)
	default:
		die(fmt.Errorf("unknown subcommand"))
	}
}

// ---------------------------------------------------------------- setup
func getOrMake(api *antlib.Api, slug, format string) obj {
	if f := api.FeedBySlug(slug); f != nil {
		return f
	}
	f, _ := api.MakeFeed(slug, format)
	return f
}

func setup(a []string) {
	statePath, dcfg := a[0], a[1]
	sfx := ""
	if len(a) > 2 {
		sfx = a[2]
	}
	oke := antlib.Named("oke")
	feedRaw := getOrMake(oke, "ev5"+sfx+"-raw", "raw")
	feedOci := getOrMake(oke, "ev5"+sfx+"-oci", "oci")
	fmt.Println("feeds", feedRaw.Str("id"), feedOci.Str("id"))
	var role obj
	for _, r := range antlib.Must(oke.OK("GET", "/api/v1/roles", nil)).Items() {
		if r.Str("name") == "ev5"+sfx+"-replicate" {
			role = r
		}
	}
	if role == nil {
		role = antlib.Must(oke.OK("POST", "/api/v1/roles", obj{"name": "ev5" + sfx + "-replicate"}))
	}
	var grants []obj
	for _, f := range []obj{feedRaw, feedOci} {
		grants = append(grants, obj{"kind": "feed", "effect": "allow", "feed_id": f.Str("id"), "actions": []string{"read", "replicate"}})
	}
	antlib.Must(oke.OK("PUT", "/api/v1/roles/"+role.Str("id")+"/grants", obj{"grants": grants}, max64(role.Int("revision"), 1)))
	var pr obj
	for _, p := range antlib.Must(oke.OK("GET", "/api/v1/principals", nil)).Items() {
		if p.Str("subject") == "ev5"+sfx+"-replicator" {
			pr = p
		}
	}
	if pr == nil {
		pr = antlib.Must(oke.OK("POST", "/api/v1/principals", obj{"subject": "ev5" + sfx + "-replicator", "kind": "service"}))
	}
	antlib.Must(oke.OK("PUT", "/api/v1/principals/"+pr.Str("id")+"/roles", obj{"role_ids": []string{role.Str("id")}}, max64(pr.Int("revision"), 1)))
	var scopes []obj
	for _, f := range []obj{feedRaw, feedOci} {
		scopes = append(scopes, obj{"feed_id": f.Str("id"), "actions": []string{"read", "replicate"}})
	}
	tk := antlib.Must(oke.OK("POST", "/api/v1/principals/"+pr.Str("id")+"/tokens", obj{"scopes": scopes, "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}))
	state := obj{"created_at": antlib.Now(), "feed_raw": feedRaw, "feed_oci": feedOci, "role_id": role.Str("id"), "principal_id": pr.Str("id"), "token_id": tk.Str("id"), "replicate_token": tk.Str("secret")}
	writeJSON(statePath, state, 0o600)

	type item struct {
		path string
		n    int
	}
	var plan []item
	for i := 0; i < 250; i++ {
		plan = append(plan, item{fmt.Sprintf("small/f%04d.bin", i), 1024 + (i*397)%63000})
	}
	if sfx != "" { // small-blob profile: no blob above ~1.3 MiB (OKE edge LB is shaped to 10 Mbit/s; see bug 1)
		for i := 0; i < 100; i++ {
			plan = append(plan, item{fmt.Sprintf("medium/m%03d.bin", i), 262144 + (i*40961)%1000000})
		}
	} else {
		for i := 0; i < 40; i++ {
			plan = append(plan, item{fmt.Sprintf("medium/m%03d.bin", i), 262144 + (i*40961)%1800000})
		}
		for i := 0; i < 10; i++ {
			plan = append(plan, item{fmt.Sprintf("large/L%02d.bin", i), 16 << 20})
		}
	}
	manifest := map[string]obj{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	t0 := time.Now()
	for _, it := range plan {
		wg.Add(1)
		sem <- struct{}{}
		go func(it item) {
			defer wg.Done()
			defer func() { <-sem }()
			data := antlib.Det("ev5:"+it.path, it.n)
			var s int
			for try := 0; try < 3; try++ {
				if s = oke.RawPut("ev5"+sfx+"-raw", it.path, data); s == 201 || s == 200 {
					mu.Lock()
					manifest[it.path] = obj{"size": it.n, "sha256": antlib.SHA(data)}
					mu.Unlock()
					return
				}
				time.Sleep(time.Second)
			}
			die(fmt.Errorf("put %s -> %d", it.path, s))
		}(it)
	}
	wg.Wait()
	total := 0
	for _, m := range manifest {
		total += int(m.Int("size"))
	}
	fmt.Printf("raw published %d assets, %d bytes in %.1fs\n", len(manifest), total, time.Since(t0).Seconds())
	writeJSON(statePath+".raw-manifest.json", manifest, 0o644)

	die(os.MkdirAll(dcfg, 0o700))
	auth := base64.StdEncoding.EncodeToString([]byte("service:" + oke.Token))
	writeJSON(filepath.Join(dcfg, "config.json"), obj{"auths": obj{"ant.azrty.com": obj{"auth": auth}}}, 0o600)
	images := map[string]string{"busybox": "mirror.gcr.io/library/busybox:1.36", "static": "gcr.io/distroless/static:nonroot"}
	if sfx == "" {
		images = map[string]string{"alpine": "mirror.gcr.io/library/alpine:3.20", "busybox": "mirror.gcr.io/library/busybox:1.36", "nginx": "mirror.gcr.io/library/nginx:1.27-alpine"}
	}
	// NOTE: do not copy hello-world: its index carries Windows images with 125-200 MB layers.
	for name, src := range images {
		cmd := exec.Command("crane", "copy", src, "ant.azrty.com/ev5"+sfx+"-oci/"+name+":v1")
		cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+dcfg)
		out, err := cmd.CombinedOutput()
		fmt.Println("crane copy", name, "err", err, trunc(string(out), 160))
	}
	fmt.Println("setup done", antlib.Now())
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// ---------------------------------------------------------------- seed
var statusKeys = []string{"phase", "running", "seed_done", "seed_total", "checkpoint", "remote_head", "changes_behind", "lag_seconds", "assets_applied", "blobs_transferred", "blobs_deduplicated", "bytes_transferred", "resumed_bytes", "consecutive_failures", "failures_total", "last_error", "error"}

type tl struct {
	f  *os.File
	mu sync.Mutex
}

func (t *tl) log(ev string, kv obj) {
	rec := obj{"t": antlib.Now(), "ev": ev}
	for k, v := range kv {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	t.mu.Lock()
	_, _ = t.f.Write(append(b, 10))
	t.mu.Unlock()
	fmt.Println(trunc(string(b), 240))
}

func jobStatus(kw *antlib.Api, id string) obj {
	s, err := kw.OK("GET", "/api/v1/replication/jobs/"+id+"/status", nil)
	if err != nil {
		return obj{"error": trunc(err.Error(), 80)}
	}
	return s
}

func pick(s obj, keys []string) obj {
	o := obj{}
	for _, k := range keys {
		if v, ok := s[k]; ok {
			o[k] = v
		}
	}
	return o
}

func kubectlKW(args ...string) { _, _ = antlib.Kubectl("kw", args...) }

func seed(a []string) {
	statePath, tlPath, sfx, run := a[0], a[1], a[2], a[3]
	only := len(a) > 4 && a[4] == "oci-only"
	var state obj
	readJSON(statePath, &state)
	var man map[string]obj
	readJSON(statePath+".raw-manifest.json", &man)
	rawTotal := int64(0)
	for _, m := range man {
		rawTotal += m.Int("size")
	}
	f, err := os.OpenFile(tlPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	die(err)
	T := &tl{f: f}
	kw := antlib.Named("kw")
	destRaw := getOrMakeNoSrc(kw, "ev5"+sfx+"-raw-replica", "raw")
	destOci := getOrMakeNoSrc(kw, "ev5"+sfx+"-oci-replica", "oci")
	var peer obj
	for _, p := range antlib.Must(kw.OK("GET", "/api/v1/replication/peers", nil)).Items() {
		if p.Str("name") == "oke-ev5"+sfx {
			peer = p
		}
	}
	if peer == nil {
		peer = antlib.Must(kw.OK("POST", "/api/v1/replication/peers", obj{"name": "oke-ev5" + sfx, "url": "https://ant.azrty.com", "token": state.Str("replicate_token")}))
	}
	T.log("peer", obj{"peer_id": peer.Str("id"), "url": peer.Str("url")})
	m, _ := antlib.Kubectl("kw", "-n", nsAnt, "exec", "deploy/ant-ant", "--", "wget", "-q", "-O", "-", "http://127.0.0.1:8080/metrics")
	_ = os.WriteFile(tlPath+".metrics-before.txt", []byte(m), 0o644)
	type jd struct {
		name       string
		rf, df     obj
		interrupts bool
	}
	jobs := obj{}
	for _, j := range []jd{{"ev5" + sfx + "-raw-" + run, antlib.Obj(state.Obj("feed_raw")), destRaw, true}, {"ev5" + sfx + "-oci-" + run, antlib.Obj(state.Obj("feed_oci")), destOci, false}} {
		if only && strings.Contains(j.name, "-raw-") {
			continue
		}
		job := antlib.Must(kw.OK("POST", "/api/v1/replication/jobs", obj{"name": j.name, "peer_id": peer.Str("id"), "remote_feed_id": j.rf.Str("id"), "dest_feed_id": j.df.Str("id"), "mode": "continuous", "interval_seconds": 10, "include_cached": true, "deletion_policy": "ignore"}))
		T.log("job-created", obj{"name": j.name, "job_id": job.Str("id"), "dest": j.df.Str("slug")})
		jobs[j.name] = job.Str("id")
		writeJSON(statePath+".jobs.json", jobs, 0o644)
		interrupted := !j.interrupts
		for t0 := time.Now(); time.Since(t0) < 30*time.Minute; time.Sleep(time.Second) {
			s := jobStatus(kw, job.Str("id"))
			T.log("status", obj{"tag": "seed", j.name: pick(s, statusKeys)})
			if !interrupted && float64(s.Int("bytes_transferred")) >= 0.35*float64(rawTotal) && (s.Str("phase") == "seed_assets" || s.Str("phase") == "seed_tags") {
				T.log("interrupt", obj{"action": "scale kw ant-ant to 0", "job": j.name, "bytes_transferred": s.Int("bytes_transferred"), "unique_bytes": rawTotal, "seed_done": s.Int("seed_done")})
				interruptKW(T, kw, 45*time.Second)
				interrupted = true
				continue
			}
			if s.Str("phase") == "incremental" && interrupted && !s.Bool("running") {
				T.log("seed-complete", obj{"job": j.name})
				break
			}
		}
	}
}

func getOrMakeNoSrc(api *antlib.Api, slug, format string) obj {
	if f := api.FeedBySlug(slug); f != nil {
		return f
	}
	return antlib.Must(api.OK("POST", "/api/v1/feeds", obj{"slug": slug, "format": format}))
}

// interruptKW scales the kw deployment to zero, holds, restores and waits for readiness.
func interruptKW(T *tl, kw *antlib.Api, hold time.Duration) {
	kubectlKW("-n", nsAnt, "scale", "deploy/ant-ant", "--replicas=0")
	kubectlKW("-n", nsAnt, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/name=kuvryn-ant", "--timeout=120s")
	T.log("down", obj{"note": fmt.Sprintf("holding down %s", hold)})
	time.Sleep(hold)
	kubectlKW("-n", nsAnt, "scale", "deploy/ant-ant", "--replicas=1")
	T.log("scaled-up", nil)
	t0 := time.Now()
	for time.Since(t0) < 5*time.Minute {
		if s, _, _, err := kw.Req("GET", "/api/v1/status", nil); err == nil && s == 200 {
			break
		}
		time.Sleep(time.Second)
	}
	T.log("ready-again", obj{"seconds": time.Since(t0).Seconds()})
}

// ---------------------------------------------------------------- ctl
func ctl(a []string) {
	kw := antlib.Named("kw")
	switch a[0] {
	case "disable", "delete":
		for _, j := range a[1:] {
			rev := kw.Rev("replication/jobs", j)
			if a[0] == "disable" {
				antlib.Must(kw.OK("PATCH", "/api/v1/replication/jobs/"+j, obj{"revision": rev, "enabled": false}))
			} else {
				antlib.Must(kw.OK("DELETE", "/api/v1/replication/jobs/"+j, nil, rev))
			}
			fmt.Println(a[0], j)
		}
	case "delfeed":
		for _, slug := range a[1:] {
			if f := kw.FeedBySlug(slug); f != nil {
				s, _, _, _ := kw.Req("DELETE", "/api/v1/feeds/"+f.Str("id"), nil, antlib.Opt{Rev: f.Int("revision")})
				fmt.Println(slug, s)
			}
		}
	case "delpeer": // delpeer <name>...: delete replication peers on kw by name
		for _, p := range antlib.Must(kw.OK("GET", "/api/v1/replication/peers", nil)).Items() {
			for _, n := range a[1:] {
				if p.Str("name") == n {
					s, _, _, _ := kw.Req("DELETE", "/api/v1/replication/peers/"+p.Str("id"), nil, antlib.Opt{Rev: p.Int("revision")})
					fmt.Println("peer", n, s)
				}
			}
		}
	case "revoke": // revoke <oke|kw> <token-id>...
		api := antlib.Named(a[1])
		for _, id := range a[2:] {
			s, _, _, _ := api.Req("DELETE", "/api/v1/tokens/"+id, nil)
			fmt.Println("token", id, s)
		}
	case "cleanup": // cleanup <oke|kw> <slug-prefix>: delete leftover test feeds
		api := antlib.Named(a[1])
		for _, f := range antlib.Must(api.OK("GET", "/api/v1/feeds", nil)).Items() {
			if strings.HasPrefix(f.Str("slug"), a[2]) {
				s, _, _, _ := api.Req("DELETE", "/api/v1/feeds/"+f.Str("id"), nil, antlib.Opt{Rev: f.Int("revision")})
				fmt.Println(f.Str("slug"), s)
			}
		}
	case "status":
		for _, e := range antlib.Must(kw.OK("GET", "/api/v1/replication/status", nil)).Items() {
			b, _ := json.Marshal(e.Obj("status"))
			fmt.Println(e.Str("name"), e.Str("id"), e.Bool("enabled"), string(b))
		}
	}
}

// ---------------------------------------------------------------- verify
func craneEnv(host, token string, extra ...string) []string {
	d, _ := os.MkdirTemp("", "ac5v-")
	writeJSON(filepath.Join(d, "config.json"), obj{"auths": obj{host: obj{"auth": base64.StdEncoding.EncodeToString([]byte("service:" + token))}}}, 0o600)
	return append(append(os.Environ(), "DOCKER_CONFIG="+d), extra...)
}

func crane(env []string, args ...string) (string, error) {
	c := exec.Command("crane", args...)
	c.Env = env
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func verify(a []string) {
	statePath, ca, out := a[0], a[1], a[2]
	names := []string{"busybox", "static"}
	if len(a) > 3 {
		names = strings.Split(a[3], ",")
	}
	var state obj
	readJSON(statePath, &state)
	sfx := ""
	if strings.Contains(state.Obj("feed_raw").Str("slug"), "ev5s") {
		sfx = "s"
	}
	var man map[string]obj
	readJSON(statePath+".raw-manifest.json", &man)
	kw := antlib.Named("kw")
	res := obj{"started": antlib.Now()}
	slug := "ev5" + sfx + "-raw-replica"
	var bad []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	t0 := time.Now()
	for path, m := range man {
		wg.Add(1)
		sem <- struct{}{}
		go func(path string, m obj) {
			defer wg.Done()
			defer func() { <-sem }()
			s, _, d := kw.RawGet(slug, path, "")
			if s != 200 || antlib.SHA(d) != m.Str("sha256") || int64(len(d)) != m.Int("size") {
				mu.Lock()
				bad = append(bad, fmt.Sprintf("%s(%d)", path, s))
				mu.Unlock()
			}
		}(path, m)
	}
	wg.Wait()
	res["raw"] = obj{"artifacts": len(man), "mismatch_or_missing": len(bad), "examples": bad, "seconds": time.Since(t0).Seconds()}
	fmt.Println("raw", res["raw"])
	oci := obj{}
	okeTok := antlib.Named("oke").Token
	for _, n := range names {
		e := obj{}
		d1, _ := crane(craneEnv("ant.azrty.com", okeTok), "digest", "ant.azrty.com/ev5"+sfx+"-oci/"+n+":v1")
		kenv := craneEnv("ant.kw.watteel.lab", kw.Token, "SSL_CERT_FILE="+ca)
		d2, _ := crane(kenv, "digest", "ant.kw.watteel.lab/ev5"+sfx+"-oci-replica/"+n+":v1")
		v, err := crane(kenv, "validate", "--remote", "ant.kw.watteel.lab/ev5"+sfx+"-oci-replica/"+n+":v1")
		e["oke_digest"], e["kw_digest"], e["digest_equal"] = d1, d2, d1 == d2 && strings.HasPrefix(d1, "sha256:")
		if err == nil {
			e["kw_validate_remote"] = "PASS"
		} else {
			e["kw_validate_remote"] = trunc(v, 200)
		}
		oci[n] = e
		fmt.Println(n, e)
	}
	res["oci"], res["finished"] = oci, antlib.Now()
	writeJSON(out, res, 0o644)
}

// ---------------------------------------------------------------- incr
func ociReq(api *antlib.Api, method, path string, body []byte, ct string) (int, http.Header, []byte) {
	req, _ := http.NewRequest(method, api.Base+path, bytes.NewReader(body))
	req.SetBasicAuth("service", api.Token)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := api.C.Do(req)
	if err != nil {
		return 0, nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	d, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, d
}

func findJob(kw *antlib.Api, prefix string) obj {
	for _, j := range antlib.Must(kw.OK("GET", "/api/v1/replication/jobs", nil)).Items() {
		if strings.HasPrefix(j.Str("name"), prefix) && j.Bool("enabled") {
			return j
		}
	}
	die(fmt.Errorf("job %s not found", prefix))
	return nil
}

func waitCaughtUp(kw *antlib.Api, id string, timeout time.Duration) (time.Duration, obj) {
	t0 := time.Now()
	for time.Since(t0) < timeout {
		s := jobStatus(kw, id)
		if s.Str("phase") == "incremental" && s.Int("changes_behind") == 0 && !s.Bool("running") && s.Int("checkpoint") >= s.Int("remote_head") {
			return time.Since(t0), s
		}
		time.Sleep(500 * time.Millisecond)
	}
	return time.Since(t0), jobStatus(kw, id)
}

func incr(a []string) {
	statePath, outPath := a[0], a[1]
	// a[2] is a directory (or the s3poke binary itself) that holds the helper named exactly "s3poke"; it is put on PATH.
	if d := a[2]; d != "" {
		if fi, err := os.Stat(d); err == nil && !fi.IsDir() {
			d = filepath.Dir(d)
		}
		die(os.Setenv("PATH", d+string(os.PathListSeparator)+os.Getenv("PATH")))
	}
	var state obj
	readJSON(statePath, &state)
	oke, kw := antlib.Named("oke"), antlib.Named("kw")
	res := obj{"started": antlib.Now()}
	rawJob := findJob(kw, "ev5s-raw-")
	ociJob := findJob(kw, "ev5s-oci-")
	rid, oid := rawJob.Str("id"), ociJob.Str("id")
	R := func(k string, v any) { res[k] = v; fmt.Println(k, trunc(fmt.Sprint(v), 300)) }
	steps := "inc,corrupt,outage,delete"
	if len(a) > 3 {
		steps = a[3]
	}
	want := func(n string) bool { return strings.Contains(","+steps+",", ","+n+",") }
	var d time.Duration
	var s obj

	if want("inc") {
		published := map[string]time.Time{}
		var pubOrder []string
		for i := 0; i < 20; i++ {
			p := fmt.Sprintf("incr/i%03d.bin", i)
			if s := oke.RawPut("ev5s-raw", p, antlib.Det("ev5i:"+p, 50000+i*1000)); s != 201 {
				die(fmt.Errorf("put %s %d", p, s))
			}
			published[p] = time.Now()
			pubOrder = append(pubOrder, p)
			time.Sleep(300 * time.Millisecond)
		}
		seen := map[string]time.Time{}
		for t0 := time.Now(); time.Since(t0) < 3*time.Minute && len(seen) < len(pubOrder); time.Sleep(500 * time.Millisecond) {
			for _, p := range pubOrder {
				if _, ok := seen[p]; ok {
					continue
				}
				if s, _, d := kw.RawGet("ev5s-raw-replica", p, ""); s == 200 {
					i := 0
					_, _ = fmt.Sscanf(p, "incr/i%d.bin", &i)
					if antlib.SHA(d) == antlib.SHA(antlib.Det("ev5i:"+p, 50000+i*1000)) {
						seen[p] = time.Now()
					}
				}
			}
		}
		var lags []float64
		for _, p := range pubOrder {
			if t, ok := seen[p]; ok {
				lags = append(lags, t.Sub(published[p]).Seconds())
			}
		}
		sort.Float64s(lags)
		R("incremental_raw_20_publishes", obj{"visible_on_kw": len(lags), "of": len(pubOrder), "lag_seconds_sorted": lags, "p50": q(lags, .5), "p95": q(lags, .95), "max": q(lags, 1)})

		// 2. 100 metadata-only updates (OCI tag re-points); poll kw tags/list
		st, hd, man := ociReq(oke, "GET", "/v2/ev5s-oci/busybox/manifests/v1", nil, "")
		ct := hd.Get("Content-Type")
		if st != 200 {
			die(fmt.Errorf("manifest get %d", st))
		}
		tagPub := map[string]time.Time{}
		var tags []string
		var maxReported float64
		var mu sync.Mutex
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if s := jobStatus(kw, oid); s != nil {
					mu.Lock()
					if v, _ := s["lag_seconds"].(float64); v > maxReported {
						maxReported = v
					}
					mu.Unlock()
				}
				time.Sleep(time.Second)
			}
		}()
		for i := 0; i < 100; i++ {
			tg := fmt.Sprintf("t%03d", i)
			if s, _, _ := ociReq(oke, "PUT", "/v2/ev5s-oci/busybox/manifests/"+tg, man, ct); s != 201 {
				die(fmt.Errorf("tag put %d", s))
			}
			tagPub[tg] = time.Now()
			tags = append(tags, tg)
		}
		tagSeen := map[string]time.Time{}
		for t0 := time.Now(); time.Since(t0) < 5*time.Minute && len(tagSeen) < len(tags); time.Sleep(500 * time.Millisecond) {
			_, _, d := ociReq(kw, "GET", "/v2/ev5s-oci-replica/busybox/tags/list", nil, "")
			var tl struct{ Tags []string }
			_ = json.Unmarshal(d, &tl)
			now := time.Now()
			for _, t := range tl.Tags {
				if _, ok := tagPub[t]; ok {
					if _, ok := tagSeen[t]; !ok {
						tagSeen[t] = now
					}
				}
			}
		}
		close(stop)
		var tlags []float64
		for _, t := range tags {
			if s, ok := tagSeen[t]; ok {
				tlags = append(tlags, s.Sub(tagPub[t]).Seconds())
			}
		}
		sort.Float64s(tlags)
		R("metadata_only_100_tag_updates", obj{"visible_on_kw": len(tlags), "of": 100, "p50": q(tlags, .5), "p95": q(tlags, .95), "max": q(tlags, 1), "max_lag_seconds_reported_by_status": maxReported, "target_p95_seconds": 30})
		d, s = waitCaughtUp(kw, oid, 2*time.Minute)
		R("oci_job_caught_up_after", obj{"seconds": d.Seconds(), "status": pick(s, statusKeys)})

		// 3. interruption during incremental: publish 30 files, kill kw mid-sync, verify catch-up and bytes
		before := jobStatus(kw, rid)
		for i := 0; i < 30; i++ {
			p := fmt.Sprintf("incr2/j%03d.bin", i)
			oke.RawPut("ev5s-raw", p, antlib.Det("ev5j:"+p, 400000+i*10000))
		}
		T := &tl{f: os.Stderr}
		time.Sleep(2 * time.Second)
		interruptKW(T, kw, 30*time.Second)
		d, s = waitCaughtUp(kw, rid, 5*time.Minute)
		okc := 0
		for i := 0; i < 30; i++ {
			p := fmt.Sprintf("incr2/j%03d.bin", i)
			if st, _, b := kw.RawGet("ev5s-raw-replica", p, ""); st == 200 && antlib.SHA(b) == antlib.SHA(antlib.Det("ev5j:"+p, 400000+i*10000)) {
				okc++
			}
		}
		uniq := int64(0)
		for i := 0; i < 30; i++ {
			uniq += int64(400000 + i*10000)
		}
		R("incremental_interrupted", obj{"files": 30, "verified_on_kw": okc, "catch_up_seconds_after_ready": d.Seconds(), "unique_bytes_published": uniq,
			"bytes_transferred_delta": s.Int("bytes_transferred") - before.Int("bytes_transferred"), "blobs_transferred_delta": s.Int("blobs_transferred") - before.Int("blobs_transferred"),
			"blobs_deduplicated_delta": s.Int("blobs_deduplicated") - before.Int("blobs_deduplicated"), "status": pick(s, statusKeys)})

	}
	if want("corrupt") {
		cp := "corrupt/c002.bin"
		cdata := antlib.Det("ev5c:"+cp, 300000)
		setEnabled(kw, rid, false) // hold the job so the fault is in place before the destination first sees the change
		for i := 0; i < 60 && jobStatus(kw, rid).Bool("running"); i++ {
			time.Sleep(time.Second)
		}
		oke.RawPut("ev5s-raw", cp, cdata)
		key := findBlobKey("oke", antlib.SHA(cdata))
		tmp, _ := os.MkdirTemp("", "ac5c")
		orig := filepath.Join(tmp, "orig")
		bad := filepath.Join(tmp, "bad")
		_ = exec.Command("s3poke", "oke", "get", key, orig).Run()
		ob, _ := os.ReadFile(orig)
		nb := append([]byte(nil), ob...)
		nb[len(nb)/2] ^= 0xFF
		_ = os.WriteFile(bad, nb, 0o600)
		// inject BEFORE kw sees the change: the poll interval is 10 s, so race it by writing immediately
		die(exec.Command("s3poke", "oke", "put", key, bad).Run())
		var failBefore = jobStatus(kw, rid).Int("failures_total")
		setEnabled(kw, rid, true)
		time.Sleep(45 * time.Second)
		sj := jobStatus(kw, rid)
		stGet, _, _ := kw.RawGet("ev5s-raw-replica", cp, "")
		R("corrupt_blob_injected", obj{"source_object": "flipped one byte, same size", "kw_read_status_while_corrupt": stGet, "failures_total_delta": sj.Int("failures_total") - failBefore,
			"last_error": sj.Str("last_error"), "consecutive_failures": sj.Int("consecutive_failures"), "checkpoint": sj.Int("checkpoint"), "remote_head": sj.Int("remote_head"), "changes_behind": sj.Int("changes_behind"), "phase": sj.Str("phase")})
		die(exec.Command("s3poke", "oke", "put", key, orig).Run())
		d, s = waitCaughtUp(kw, rid, 5*time.Minute)
		stGet, _, gb := kw.RawGet("ev5s-raw-replica", cp, "")
		R("corrupt_blob_after_repair", obj{"caught_up_seconds": d.Seconds(), "kw_read_status": stGet, "kw_bytes_match_original": antlib.SHA(gb) == antlib.SHA(cdata), "status": pick(s, statusKeys)})

	}
	if want("outage") {
		peer := antlib.Must(kw.OK("GET", "/api/v1/replication/peers/"+rawJob.Str("peer_id"), nil))
		origURL := peer.Str("url")
		antlib.Must(kw.OK("PATCH", "/api/v1/replication/peers/"+peer.Str("id"), obj{"revision": peer.Int("revision"), "url": "https://ant-unreachable.invalid"}))
		opath := fmt.Sprintf("outage/o%d.bin", time.Now().Unix())
		oke.RawPut("ev5s-raw", opath, antlib.Det("ev5o", 10000))
		time.Sleep(40 * time.Second)
		so := jobStatus(kw, rid)
		reads, ok := 0, 0
		for p, m := range map[string]string{"small/f0001.bin": "", "medium/m001.bin": "", "incr/i001.bin": ""} {
			_ = m
			reads++
			if st, _, _ := kw.RawGet("ev5s-raw-replica", p, ""); st == 200 {
				ok++
			}
		}
		R("source_outage", obj{"local_reads_ok": ok, "of": reads, "consecutive_failures": so.Int("consecutive_failures"), "last_error": so.Str("last_error"), "lag_seconds": so["lag_seconds"], "status": pick(so, statusKeys)})
		peer = antlib.Must(kw.OK("GET", "/api/v1/replication/peers/"+peer.Str("id"), nil))
		antlib.Must(kw.OK("PATCH", "/api/v1/replication/peers/"+peer.Str("id"), obj{"revision": peer.Int("revision"), "url": origURL}))
		t0 := time.Now()
		for time.Since(t0) < 5*time.Minute {
			if st, _, b := kw.RawGet("ev5s-raw-replica", opath, ""); st == 200 && antlib.SHA(b) == antlib.SHA(antlib.Det("ev5o", 10000)) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		s = jobStatus(kw, rid)
		R("source_outage_recovery", obj{"seconds_from_url_restore_to_object_visible": time.Since(t0).Seconds(), "target_seconds": 60, "status": pick(s, statusKeys)})

	}
	if want("delete") {
		delID := ""
		for _, p := range antlib.Must(oke.OK("GET", "/api/v1/feeds/"+state.Obj("feed_raw").Str("id")+"/packages?limit=100&prefix=incr/i00", nil)).Items() {
			if p.Str("coordinate") == "incr/i003.bin" {
				delID = p.Str("id")
			}
		}
		fid := state.Obj("feed_raw").Str("id")
		dp := antlib.Must(oke.OK("GET", "/api/v1/feeds/"+fid+"/delete-policy", nil))
		antlib.Must(oke.OK("PUT", "/api/v1/feeds/"+fid+"/delete-policy", obj{"mode": "allowed", "immutable_releases": false, "revision": dp.Int("revision")}))
		vers := antlib.Must(oke.OK("GET", "/api/v1/feeds/"+fid+"/packages/"+delID+"/versions", nil)).Items()
		ds, _, dd, _ := oke.Req("DELETE", "/api/v1/feeds/"+fid+"/packages/"+delID+"/versions/"+vers[0].Str("version"), nil)
		_, s = waitCaughtUp(kw, rid, 2*time.Minute)
		dry, _ := kw.OK("GET", "/api/v1/replication/jobs/"+rid+"/deletions", nil)
		st3, _, _ := kw.RawGet("ev5s-raw-replica", "incr/i003.bin", "")
		R("deletion_dry_run", obj{"authority_delete_http": ds, "authority_resp": trunc(string(dd), 120), "dry_run": dry, "kw_still_serves_deleted_object_status": st3, "deletions_held": s.Int("deletions_held"), "deletions_applied": s.Int("deletions_applied")})

	}
	R("final_status_raw", pick(jobStatus(kw, rid), statusKeys))
	R("final_status_oci", pick(jobStatus(kw, oid), statusKeys))
	m, _ := antlib.Kubectl("kw", "-n", nsAnt, "exec", "deploy/ant-ant", "--", "wget", "-q", "-O", "-", "http://127.0.0.1:8080/metrics")
	var rm []string
	for _, l := range strings.Split(m, "\n") {
		if strings.HasPrefix(l, "ant_replication_") {
			rm = append(rm, l)
		}
	}
	R("kw_replication_metrics", rm)
	res["finished"] = antlib.Now()
	writeJSON(outPath, res, 0o644)
}

func q(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	return s[int(p*float64(len(s)-1)+.5)]
}

func setEnabled(kw *antlib.Api, id string, on bool) {
	antlib.Must(kw.OK("PATCH", "/api/v1/replication/jobs/"+id, obj{"revision": kw.Rev("replication/jobs", id), "enabled": on}))
}

func findBlobKey(which, digest string) string {
	out, _ := exec.Command("s3poke", which, "ls", "").Output()
	for _, l := range strings.Split(string(out), "\n") {
		k := strings.Split(l, "\t")[0]
		if strings.HasSuffix(k, "/objects/"+digest) {
			return k
		}
	}
	die(fmt.Errorf("blob %s not found", digest))
	return ""
}
