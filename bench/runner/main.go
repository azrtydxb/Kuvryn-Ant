// Command runner is the Kuvryn Ant benchmark orchestrator (spec section 8, release-workloads W1-W6 shapes).
//
// It drives a deployed instance from a pod inside the same cluster (so the shaped edge load balancer and the WAN
// are not what is measured) against a counting origin pod, while sampling the Ant pod's RSS from /proc.
//
//	go run ./bench/runner -target kw  -out evidence/2026-10-18/bench/kw.json
//	go run ./bench/runner -target oke -scenarios warm,fanout -out ...
//
// Requires kubectl contexts azrty-oke / kw, Go (cross-builds bench/antbench for the node
// architecture), and the automation tokens in ~/.kuvryn-ant/. Everything it creates is prefixed evb- and removed
// at the end. Concurrency and sizes are capped by flags so a shared cluster is not stressed.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/bench/antlib"
)

type obj = antlib.Obj

const (
	ns  = "kuvryn-ant"
	pod = "evb-bench"
)

var (
	target   = flag.String("target", "", "oke|kw")
	out      = flag.String("out", "", "result JSON")
	scenFlag = flag.String("scenarios", "warm,cold,fanout,large,outage,publish,npm", "comma list (also: direct, needs EVB_DIRECT_URL)")
	largeGiB = flag.String("large-gib", "1,4", "large object sizes in GiB")
	fanout   = flag.Int("fanout", 32, "concurrent pulls of one uncached key")
	duration = flag.Int("duration", 30, "seconds per timed phase")
	keep     = flag.Bool("keep", false, "keep the bench pod and feeds")
)

var (
	api     *antlib.Api
	ctx     string
	created []string
	result  = obj{}
	tag     = strconv.FormatInt(time.Now().Unix(), 16)
	antSvc  = "http://ant-ant." + ns + ".svc:8080"
)

func k(args ...string) string {
	o, err := antlib.Kubectl(ctx, args...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warn:", err)
	}
	return o
}

func podExec(cmd string) (string, error) {
	return antlib.Kubectl(ctx, "-n", ns, "exec", pod, "--", "sh", "-c", cmd)
}

// ---------------------------------------------------------------- RSS sampler
type rssSampler struct {
	stop    chan struct{}
	wg      sync.WaitGroup
	samples []int64
	hwm     int64
}

func rssOnce() (rss, hwm int64) {
	o, err := antlib.Kubectl(ctx, "-n", ns, "exec", "deploy/ant-ant", "--", "sh", "-c", "grep -E 'VmRSS|VmHWM' /proc/1/status")
	if err != nil {
		return 0, 0
	}
	for _, l := range strings.Split(o, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			switch f[0] {
			case "VmRSS:":
				rss = v
			case "VmHWM:":
				hwm = v
			}
		}
	}
	return
}

func startRSS() *rssSampler {
	s := &rssSampler{stop: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			if r, h := rssOnce(); r > 0 {
				s.samples = append(s.samples, r)
				s.hwm = h
			}
			select {
			case <-s.stop:
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return s
}

func (s *rssSampler) finish() obj {
	close(s.stop)
	s.wg.Wait()
	if len(s.samples) == 0 {
		return obj{}
	}
	peak := int64(0)
	for _, v := range s.samples {
		if v > peak {
			peak = v
		}
	}
	mib := func(kb int64) float64 { return float64(kb) / 1024 }
	return obj{"samples": len(s.samples), "rss_start_mib": mib(s.samples[0]), "rss_peak_sampled_mib": mib(peak), "rss_end_mib": mib(s.samples[len(s.samples)-1]), "hwm_mib": mib(s.hwm)}
}

// ---------------------------------------------------------------- bench pod helpers
func bench(sub string, flags ...string) obj {
	o, err := podExec("/bench/antbench " + sub + " -token-file /secrets/token.json -timeout 90m " + strings.Join(flags, " "))
	var r obj
	if json.Unmarshal([]byte(o), &r) != nil {
		return obj{"error": fmt.Sprint(err, o)}
	}
	return r
}

// ---------------------------------------------------------------- origin (counted pull-through source)
//
// Remote sources must use https with a publicly trusted certificate (CA references are operator-configured and
// plain http is refused), so the origin is a raw feed on the OKE instance (public Let's Encrypt-style TLS,
// anonymous read). Upstream fetches are counted independently at the origin (stats/feeds "pulls" of the origin
// feed) and by the proxy itself (metrics/traffic cache_misses of the proxy feed).

const originBase = "https://ant.azrty.com"

var okeAPI *antlib.Api

type origin struct {
	slug, id, token, tokenID, principal string
}

func newOrigin(name string) *origin {
	if okeAPI == nil {
		okeAPI = antlib.Named("oke")
	}
	slug := "evb-origin-" + name + "-" + tag
	f, _ := okeAPI.MakeFeed(slug, "raw")
	originFeeds = append(originFeeds, f.Str("id"))
	// The raw adapter does not honour anonymous_read (bug 3), so the proxy authenticates with a read-only token
	// scoped to this one feed (the role grants read on feeds; the token scope narrows it to the origin feed).
	var role, pr obj
	for _, r := range antlib.Must(okeAPI.OK("GET", "/api/v1/roles", nil)).Items() {
		if r.Str("name") == "evb-origin-reader-v3" {
			role = r
		}
	}
	if role == nil {
		role = antlib.Must(okeAPI.OK("POST", "/api/v1/roles", obj{"name": "evb-origin-reader-v3"}))
		antlib.Must(okeAPI.OK("PUT", "/api/v1/roles/"+role.Str("id")+"/grants", obj{"grants": []obj{{"kind": "artifact", "effect": "allow", "actions": []string{"read"}}}}, 1))
	}
	for _, p := range antlib.Must(okeAPI.OK("GET", "/api/v1/principals", nil)).Items() {
		if p.Str("subject") == "evb-origin-reader-v3" {
			pr = p
		}
	}
	if pr == nil {
		pr = antlib.Must(okeAPI.OK("POST", "/api/v1/principals", obj{"subject": "evb-origin-reader-v3", "kind": "service"}))
		antlib.Must(okeAPI.OK("PUT", "/api/v1/principals/"+pr.Str("id")+"/roles", obj{"role_ids": []string{role.Str("id")}}, 1))
	}
	tk := antlib.Must(okeAPI.OK("POST", "/api/v1/principals/"+pr.Str("id")+"/tokens", obj{"scopes": []obj{{"feed_id": f.Str("id"), "actions": []string{"read"}}}, "expires_at": time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)}))
	return &origin{slug: slug, id: f.Str("id"), token: tk.Str("secret"), tokenID: tk.Str("id"), principal: pr.Str("id")}
}

var originFeeds []string

func (o *origin) url() string { return originBase + "/feeds/" + o.slug + "/raw" }

// put publishes a deterministic object and returns its SHA-256.
func (o *origin) put(path, seed string, size int) string {
	d := antlib.Det(seed, size)
	s := 0
	for try := 0; try < 6; try++ {
		if s = okeAPI.RawPut(o.slug, path, d); s == 201 || s == 200 || s == 204 {
			return antlib.SHA(d)
		}
		time.Sleep(time.Duration(try+1) * 2 * time.Second) // 429 under the instance's admission limit
	}
	panic(fmt.Sprintf("origin put %s -> %d", path, s))
}

// pulls returns the number of successful non-management GETs the ORIGIN pod (OKE) has served, read from its
// /metrics (ant_http_requests_total GET 2xx for route "unmatched" on older builds, raw.metadata and raw.download on current ones). The feed-level "pulls"
// statistic and metrics/traffic cache counters proved unreliable for this (asynchronous, non-exact), so they are
// not used. When the target is OKE itself the clients' own GETs are in the same counter; adj() subtracts them.
func (o *origin) pulls() int64 {
	m, err := antlib.Kubectl("azrty-oke", "-n", ns, "exec", "deploy/ant-ant", "--", "wget", "-q", "-O", "-", "http://127.0.0.1:8080/metrics")
	if err != nil {
		return -1
	}
	var total int64
	for _, l := range strings.Split(m, "\n") {
		// Older builds label every data-plane request route="unmatched"; current builds name the
		// route <format>.<class>. The scenarios pull from a raw feed, so both spellings are summed.
		for _, route := range []string{"unmatched", "raw.metadata", "raw.download"} {
			if strings.HasPrefix(l, `ant_http_requests_total{route="`+route+`",method="GET",status_class="2xx"}`) {
				f := strings.Fields(l)
				v, _ := strconv.ParseInt(f[len(f)-1], 10, 64)
				total += v
			}
		}
	}
	return total
}

// noiseRate measures the origin pod's background rate of successful GETs (other tenants' replication polling and
// conformance traffic) over a 10 s idle window.
func noiseRate(o *origin) float64 {
	a := o.pulls()
	time.Sleep(10 * time.Second)
	return float64(o.pulls()-a) / 10
}

// estimate converts a raw origin GET delta over a window into upstream fetches by subtracting the expected
// background. It is an ESTIMATE (the background is noisy); for target oke the clients' own GETs are subtracted too.
func estimate(raw int64, rate float64, window time.Duration, client obj) int64 {
	e := float64(raw) - rate*window.Seconds()
	if *target == "oke" {
		st := client.Obj("result").Obj("status")
		e -= float64(st.Int("200") + st.Int("206"))
	}
	if e < 0 {
		e = 0
	}
	return int64(e + 0.5)
}

// settle lets in-flight accounting finish.
func (o *origin) settle() { time.Sleep(3 * time.Second) }

func benchNoAuth(sub string, flags ...string) obj {
	o, err := podExec("/bench/antbench " + sub + " -timeout 90m " + strings.Join(flags, " "))
	var r obj
	if json.Unmarshal([]byte(o), &r) != nil {
		return obj{"error": fmt.Sprint(err, o)}
	}
	return r
}

func slim(r obj) obj {
	res := r.Obj("result")
	if res == nil {
		return r
	}
	o := obj{}
	for _, key := range []string{"requests", "errors", "status", "bytes", "wall_seconds", "req_per_sec", "mib_per_sec", "latency_ms", "ttfb_ms", "extra", "error_samples", "body_sha256_counts"} {
		if v, ok := res[key]; ok {
			o[key] = v
		}
	}
	return o
}

func merge(a obj, b obj) obj {
	for k, v := range b {
		a[k] = v
	}
	return a
}

// ---------------------------------------------------------------- setup
func setup() {
	arch := strings.TrimSpace(k("get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}"))
	root, _ := os.Getwd()
	bin := filepath.Join(root, ".bench-bin")
	_ = os.MkdirAll(bin, 0o755)
	for _, n := range []string{"antbench"} {
		c := exec.Command("go", "build", "-o", filepath.Join(bin, n), "./bench/"+n)
		c.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
		if o, err := c.CombinedOutput(); err != nil {
			panic(string(o))
		}
	}
	k("-n", ns, "delete", "pod", pod, "--ignore-not-found", "--wait=true")
	k("-n", ns, "delete", "secret", "evb-token", "--ignore-not-found")
	home, _ := os.UserHomeDir()
	k("-n", ns, "create", "secret", "generic", "evb-token", "--from-file=token.json="+filepath.Join(home, ".kuvryn-ant", ctx+"-automation-token.txt"))
	apply(obj{"apiVersion": "v1", "kind": "Pod", "metadata": obj{"name": pod, "labels": obj{"app": pod}}, "spec": obj{
		"restartPolicy": "Never",
		"containers": []obj{{"name": "bench", "image": "busybox:1.36", "command": []string{"sleep", "86400"}, "ports": []obj{{"containerPort": 9000}},
			"volumeMounts": []obj{{"name": "tok", "mountPath": "/secrets", "readOnly": true}, {"name": "work", "mountPath": "/work"}},
			"resources":    obj{"requests": obj{"cpu": "100m", "memory": "128Mi"}, "limits": obj{"cpu": "2", "memory": "1Gi"}}}},
		"volumes": []obj{{"name": "tok", "secret": obj{"secretName": "evb-token"}}, {"name": "work", "emptyDir": obj{}}},
	}})
	k("-n", ns, "wait", "--for=condition=Ready", "pod/"+pod, "--timeout=180s")
	_, _ = podExec("mkdir -p /bench")
	for _, n := range []string{"antbench"} {
		k("-n", ns, "cp", filepath.Join(bin, n), pod+":/bench/"+n)
	}
	_, _ = podExec("chmod +x /bench/*")
	su, _ := api.OK("GET", "/api/v1/storage/usage", nil)
	var dep obj
	_ = json.Unmarshal([]byte(k("-n", ns, "get", "deploy", "ant-ant", "-o", "jsonpath={.spec.template.spec.containers[0]}")), &dep)
	node := strings.TrimSpace(k("-n", ns, "get", "pod", "-l", "app.kubernetes.io/name=kuvryn-ant", "-o", "jsonpath={.items[0].spec.nodeName}"))
	result["environment"] = obj{"arch": arch, "ant_image": dep.Str("image"), "ant_resources": dep["resources"], "node": node,
		"node_capacity":   strings.TrimSpace(k("get", "node", node, "-o", "jsonpath={.status.capacity.cpu}cpu/{.status.capacity.memory}")),
		"storage_backend": su.Str("backend"), "bench_path": "pod -> " + antSvc + " (ClusterIP, plain HTTP; ingress/LB excluded)"}
}

func apply(m obj) {
	b, _ := json.Marshal(m)
	c := exec.Command("kubectl", "--context", ctx, "-n", ns, "apply", "-f", "-")
	c.Stdin = bytes.NewReader(b)
	if o, err := c.CombinedOutput(); err != nil {
		panic(string(o))
	}
}

func newFeed(slug, format string, remote string, timeout int, cred ...string) obj {
	if remote == "" {
		f, _ := api.MakeFeed(slug, format)
		created = append(created, f.Str("id"))
		return f
	}
	feed := antlib.Must(api.OK("POST", "/api/v1/feeds", obj{"slug": slug, "format": format}))
	src := antlib.Must(api.OK("POST", "/api/v1/sources", obj{"name": slug, "kind": "remote", "format": format}))
	created = append(created, feed.Str("id"))
	cfg := obj{"origin": remote, "redirect_origins": []string{}, "allow_private": true, "timeout_seconds": timeout,
		"cache": obj{"metadata_ttl_seconds": 60, "negative_ttl_seconds": 5, "stale_if_error_seconds": 3600, "max_entries": 10000, "max_bytes": int64(1) << 30}}
	cfg["allow_private"] = false
	antlib.Must(api.OK("PUT", "/api/v1/sources/"+src.Str("id")+"/remote-config", cfg, api.Rev("sources", src.Str("id"))))
	if len(cred) > 0 && cred[0] != "" {
		// the credential attaches itself to the source's config (config.credential_id)
		antlib.Must(api.OK("PUT", "/api/v1/sources/"+src.Str("id")+"/remote-credential", obj{"authorization": "Bearer " + cred[0]}, api.Rev("sources", src.Str("id"))))
	}
	antlib.Must(api.OK("PUT", "/api/v1/feeds/"+feed.Str("id")+"/sources/"+src.Str("id"), obj{"priority": 0}, api.Rev("feeds", feed.Str("id"))))
	return feed
}

func cleanup() {
	del := func(a *antlib.Api, id string) {
		defer func() { _ = recover() }() // the instance may be restarting; leftovers are removed with `ac5 ctl cleanup <inst> evb-`
		_, _, _, _ = a.Req("DELETE", "/api/v1/feeds/"+id, nil, antlib.Opt{Rev: a.Rev("feeds", id)})
	}
	for _, id := range created {
		del(api, id)
	}
	for _, id := range originFeeds {
		del(okeAPI, id)
	}
	k("-n", ns, "delete", "pod", pod, "--ignore-not-found", "--wait=false")
	k("-n", ns, "delete", "pod", "evb-npm", "--ignore-not-found", "--wait=false")
	k("-n", ns, "delete", "secret", "evb-token", "--ignore-not-found")
}

// ---------------------------------------------------------------- scenarios
func sWarm() obj {
	feed := newFeed("evb-local-"+tag, "raw", "", 0)
	slug := feed.Str("slug")
	res := obj{}
	for _, s := range []struct {
		label string
		size  int
	}{{"64KiB", 64 << 10}, {"8MiB", 8 << 20}} {
		bench("put", "-url-template", fmt.Sprintf("%s/feeds/%s/raw/warm/%s-{i}.bin", antSvc, slug, s.label), "-n", "1", "-size", strconv.Itoa(s.size), "-c", "1", "-seed", s.label)
		url := fmt.Sprintf("%s/feeds/%s/raw/warm/%s-0.bin", antSvc, slug, s.label)
		for _, c := range []int{1, 16} {
			r := startRSS()
			b := bench("get", "-url", url, "-c", strconv.Itoa(c), "-d", fmt.Sprintf("%ds", *duration), "-hash")
			res[fmt.Sprintf("warm %s c=%d", s.label, c)] = merge(slim(b), obj{"ant_rss": r.finish()})
		}
	}
	mu := fmt.Sprintf("%s/api/v1/feeds/%s/packages?limit=50", antSvc, feed.Str("id"))
	res["metadata 100 rps (list packages, 16 workers)"] = slim(bench("get", "-url", mu, "-c", "16", "-d", fmt.Sprintf("%ds", *duration), "-rate", "100"))
	return res
}

func sCold() obj {
	o := newOrigin("cold")
	feed := newFeed("evb-remote-"+tag, "raw", o.url(), 120, o.token)
	slug := feed.Str("slug")
	res := obj{}
	for _, s := range []struct {
		label string
		size  int
	}{{"1MiB", 1 << 20}, {"4MiB", 4 << 20}} {
		var urls []string
		expect := map[string]bool{}
		for i := 0; i < 8; i++ {
			p := fmt.Sprintf("cold/%s_%d.bin", s.label, i)
			expect[o.put(p, "cold:"+p, s.size)] = true
			urls = append(urls, fmt.Sprintf("%s/feeds/%s/raw/%s", antSvc, slug, p))
		}
		_, _ = podExec("cat > /work/urls.txt <<'EOF'\n" + strings.Join(urls, "\n") + "\nEOF")
		rate := noiseRate(o)
		p0 := o.pulls()
		t0 := time.Now()
		cold := bench("get", "-urls-file", "/work/urls.txt", "-n", "8", "-c", "1", "-hash")
		o.settle()
		p1 := o.pulls()
		t1 := time.Now()
		warm := bench("get", "-urls-file", "/work/urls.txt", "-n", "64", "-c", "8", "-hash")
		o.settle()
		p2 := o.pulls()
		t2 := time.Now()
		ok := func(r obj) bool {
			for d := range r.Obj("result").Obj("body_sha256_counts") {
				if !expect[d] {
					return false
				}
			}
			return true
		}
		estCold, estWarm := estimate(p1-p0, rate, t1.Sub(t0), cold), estimate(p2-p1, rate, t2.Sub(t1), warm)
		res["cold "+s.label+" x8 sequential (origin over WAN)"] = merge(slim(cold), obj{"origin_get_delta_raw": p1 - p0, "origin_background_gets_per_s": rate, "origin_fetch_estimate": estCold, "expected_fetches": 8, "digests_ok": ok(cold)})
		res["warm "+s.label+" x64 c=8"] = merge(slim(warm), obj{"origin_get_delta_raw": p2 - p1, "origin_fetch_estimate": estWarm, "warm_run_performed_no_upstream_blob_fetch": estWarm == 0, "digests_ok": ok(warm)})
	}
	return res
}

func sFanout() obj {
	o := newOrigin("fan")
	feed := newFeed("evb-fan-"+tag, "raw", o.url(), 120, o.token)
	slug := feed.Str("slug")
	res := obj{}
	for _, size := range []int{4 << 20, 8 << 20} {
		for round := 0; round < 3; round++ {
			p := fmt.Sprintf("fan/%dMiB_%d.bin", size>>20, round)
			sha := o.put(p, "fan:"+p, size)
			url := fmt.Sprintf("%s/feeds/%s/raw/%s", antSvc, slug, p)
			rate := noiseRate(o)
			p0 := o.pulls()
			t0 := time.Now()
			r := startRSS()
			b := bench("get", "-url", url, "-c", strconv.Itoa(*fanout), "-n", strconv.Itoa(*fanout), "-hash")
			o.settle()
			p1 := o.pulls()
			est := estimate(p1-p0, rate, time.Since(t0), b)
			digs := b.Obj("result").Obj("body_sha256_counts")
			floor := float64(size) / 1.25e6 // one object over the 10 Mbit/s edge
			maxLat := b.Obj("result").Obj("latency_ms").Int("Max")
			res[fmt.Sprintf("fanout %d x %dMiB uncached key, round %d", *fanout, size>>20, round)] = merge(slim(b), obj{
				"origin_get_delta_raw": p1 - p0, "origin_background_gets_per_s": rate, "origin_fetch_estimate": est,
				"assert_at_most_one_upstream_fetch": est <= 1,
				"single_transfer_floor_s":           floor, "slowest_client_s": float64(maxLat) / 1000,
				"completion_consistent_with_one_fetch": float64(maxLat)/1000 < 3*floor+5,
				"all_bodies_equal_expected_sha256":     len(digs) == 1 && digs.Int(sha) == int64(*fanout), "ant_rss": r.finish()})
		}
	}
	return res
}

// sLarge publishes 1..N GiB objects through Ant (streamed from the bench pod) and reads them back, watching RSS.
// A 1-5 GiB pull-through from a remote origin is not practical here: the only publicly-trusted https origin is
// the OKE instance behind a 10 Mbit/s edge load balancer.
func sLarge() obj {
	feed := newFeed("evb-large-"+tag, "raw", "", 0)
	slug := feed.Str("slug")
	res := obj{}
	r0, h0 := rssOnce()
	res["rss_before_mib (rss,hwm)"] = []float64{float64(r0) / 1024, float64(h0) / 1024}
	for _, g := range strings.Split(*largeGiB, ",") {
		gib, _ := strconv.Atoi(g)
		size := int64(gib) << 30
		url := fmt.Sprintf("%s/feeds/%s/raw/large/%dg-{i}.bin", antSvc, slug, gib)
		r := startRSS()
		put := bench("put", "-url-template", url, "-n", "1", "-size", strconv.FormatInt(size, 10), "-c", "1", "-seed", fmt.Sprintf("large%d", gib))
		res[fmt.Sprintf("%d GiB publish (streamed PUT, X-Checksum-Sha256 declared), 1 stream", gib)] = merge(slim(put), obj{"ant_rss": r.finish()})
		shas, _ := put.Obj("result").Obj("extra")["accepted_body_sha256"].([]any)
		sha := ""
		if len(shas) > 0 {
			sha, _ = shas[0].(string)
		}
		get := fmt.Sprintf("%s/feeds/%s/raw/large/%dg-0.bin", antSvc, slug, gib)
		r = startRSS()
		b1 := bench("stream", "-url", get, "-expect-sha256", sha, "-c", "1")
		res[fmt.Sprintf("%d GiB read, 1 stream", gib)] = merge(slim(b1), obj{"ant_rss": r.finish()})
		r = startRSS()
		b4 := bench("stream", "-url", get, "-expect-sha256", sha, "-c", "4")
		res[fmt.Sprintf("%d GiB read, 4 concurrent streams", gib)] = merge(slim(b4), obj{"ant_rss": r.finish()})
		can := bench("stream", "-url", get, "-c", "1", "-cancel-after", strconv.Itoa(100<<20))
		again := bench("stream", "-url", get, "-expect-sha256", sha, "-c", "1")
		res[fmt.Sprintf("%d GiB cancel@100MiB then full re-read", gib)] = obj{"cancelled": slim(can), "reread": slim(again)}
	}
	r1, h1 := rssOnce()
	res["rss_after_mib (rss,hwm)"] = []float64{float64(r1) / 1024, float64(h1) / 1024}
	return res
}

// setOrigin re-points the remote source attached to feed (found through the feed's source list, because the global
// source list is paginated and grows without bound), keeping its cache settings and credential.
func setOrigin(feed obj, origin string, timeout int, allowPrivate bool) {
	for _, it := range antlib.Must(api.OK("GET", "/api/v1/feeds/"+feed.Str("id")+"/sources", nil)).Items() {
		s := it.Obj("source")
		if s.Str("kind") != "remote" {
			continue
		}
		cfg := antlib.Must(api.OK("GET", "/api/v1/sources/"+s.Str("id")+"/remote-config", nil))
		c := cfg
		if cfg.Obj("config") != nil {
			c = cfg.Obj("config")
		}
		body := obj{"origin": origin, "redirect_origins": []string{}, "allow_private": allowPrivate, "timeout_seconds": timeout, "cache": c["cache"]}
		if id := c.Str("credential_id"); id != "" {
			body["credential_id"] = id
		}
		antlib.Must(api.OK("PUT", "/api/v1/sources/"+s.Str("id")+"/remote-config", body, api.Rev("sources", s.Str("id"))))
	}
}

func sOutage() obj {
	o := newOrigin("out")
	feed := newFeed("evb-outage-"+tag, "raw", o.url(), 20, o.token)
	slug := feed.Str("slug")
	res := obj{}
	var urls []string
	for i := 0; i < 4; i++ {
		p := fmt.Sprintf("out/warm%d.bin", i)
		o.put(p, "out:"+p, 262144)
		u := fmt.Sprintf("%s/feeds/%s/raw/%s", antSvc, slug, p)
		urls = append(urls, u)
		bench("get", "-url", u, "-n", "1")
	}
	time.Sleep(3 * time.Second) // let the streamed cache fill commit: with <1 s between the first response and the outage, cached reads returned 503 in 2 of 3 runs on 375611f (see kw-rerun2.json, kw-outage-recheck.json)
	cp := "out/cold.bin"
	csha := o.put(cp, "out:"+cp, 262144)
	cold := fmt.Sprintf("%s/feeds/%s/raw/%s", antSvc, slug, cp)
	setOrigin(feed, "https://evb-origin-down.invalid", 5, false) // origin outage: unresolvable host
	_, _ = podExec("cat > /work/urls.txt <<'EOF'\n" + strings.Join(urls, "\n") + "\nEOF")
	res["origin down: cached objects x40"] = slim(bench("get", "-urls-file", "/work/urls.txt", "-n", "40", "-c", "4"))
	res["origin down: uncached object x5"] = slim(bench("get", "-url", cold, "-n", "5", "-c", "1"))
	setOrigin(feed, o.url(), 20, false)
	b := bench("get", "-url", cold, "-n", "3", "-c", "1", "-hash")
	res["origin back: previously failed object (no cache poisoning)"] = merge(slim(b), obj{"digest_ok": b.Obj("result").Obj("body_sha256_counts").Int(csha) > 0})
	res["variant B: origin rejects the proxy credential (token revoked), source config unchanged"] = outageRevoked(o, feed, urls)
	return res
}

// sDirect compares warm reads through Ant with reads of the same-size object straight from the S3 backend, from the
// same pod in the same minute (target: Ant warm throughput >= 80% of direct backend). EVB_DIRECT_URL must be a
// pre-signed GET URL of an object of the same size (see bench/README.md); it is never recorded.
func sDirect() obj {
	direct := os.Getenv("EVB_DIRECT_URL")
	if f := os.Getenv("EVB_DIRECT_URL_FILE"); direct == "" && f != "" {
		b, _ := os.ReadFile(f)
		direct = strings.TrimSpace(string(b))
	}
	if direct == "" {
		return obj{"skipped": "EVB_DIRECT_URL not set"}
	}
	feed := newFeed("evb-direct-"+tag, "raw", "", 0)
	slug := feed.Str("slug")
	size := 64 << 20
	bench("put", "-url-template", fmt.Sprintf("%s/feeds/%s/raw/d/obj-{i}.bin", antSvc, slug), "-n", "1", "-size", strconv.Itoa(size), "-c", "1", "-seed", "direct", "-declare-digest")
	url := fmt.Sprintf("%s/feeds/%s/raw/d/obj-0.bin", antSvc, slug)
	res := obj{}
	for _, c := range []int{1, 4, 16} {
		a := bench("get", "-url", url, "-c", strconv.Itoa(c), "-d", fmt.Sprintf("%ds", *duration))
		d := benchNoAuth("get", "-insecure", "-url", "'"+direct+"'", "-c", strconv.Itoa(c), "-d", fmt.Sprintf("%ds", *duration))
		am, dm := a.Obj("result"), d.Obj("result")
		ratio := 0.0
		if v, _ := dm["mib_per_sec"].(float64); v > 0 {
			ratio, _ = am["mib_per_sec"].(float64)
			ratio /= v
		}
		res[fmt.Sprintf("64 MiB warm read c=%d", c)] = obj{"ant": slim(a), "direct_backend": slim(d), "ant_over_direct": ratio}
	}
	return res
}

// outageRevoked fails the origin without touching the proxy's source configuration: the origin token the proxy uses
// is revoked, so every upstream request is refused (401) while the cached objects stay in place.
func outageRevoked(o *origin, feed obj, cachedURLs []string) obj {
	res := obj{}
	slug := feed.Str("slug")
	p := "out/cold2.bin"
	sha := o.put(p, "out:"+p, 262144)
	cold := fmt.Sprintf("%s/feeds/%s/raw/%s", antSvc, slug, p)
	s, _, _, _ := okeAPI.Req("DELETE", "/api/v1/tokens/"+o.tokenID, nil)
	res["token revoked http"] = s
	time.Sleep(2 * time.Second)
	_, _ = podExec("cat > /work/urls.txt <<'EOF'\n" + strings.Join(cachedURLs, "\n") + "\nEOF")
	res["cached objects x40"] = slim(bench("get", "-urls-file", "/work/urls.txt", "-n", "40", "-c", "4"))
	res["uncached object x5"] = slim(bench("get", "-url", cold, "-n", "5", "-c", "1"))
	tk := antlib.Must(okeAPI.OK("POST", "/api/v1/principals/"+o.principal+"/tokens", obj{"scopes": []obj{{"feed_id": o.id, "actions": []string{"read"}}}, "expires_at": time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)}))
	for _, it := range antlib.Must(api.OK("GET", "/api/v1/feeds/"+feed.Str("id")+"/sources", nil)).Items() {
		if src := it.Obj("source"); src.Str("kind") == "remote" {
			antlib.Must(api.OK("PUT", "/api/v1/sources/"+src.Str("id")+"/remote-credential", obj{"authorization": "Bearer " + tk.Str("secret")}, api.Rev("sources", src.Str("id"))))
		}
	}
	b := bench("get", "-url", cold, "-n", "3", "-c", "1", "-hash")
	res["credential restored: previously failed object"] = merge(slim(b), obj{"digest_ok": b.Obj("result").Obj("body_sha256_counts").Int(sha) > 0})
	return res
}

func sPublish() obj {
	feed := newFeed("evb-pub-"+tag, "raw", "", 0)
	slug := feed.Str("slug")
	res := obj{}
	for _, c := range []int{4, 16} {
		t := fmt.Sprintf("%s/feeds/%s/raw/pub%d/p{i}.bin", antSvc, slug, c)
		res[fmt.Sprintf("publish 200 x 64KiB, c=%d", c)] = slim(bench("put", "-url-template", t, "-n", "200", "-size", "65536", "-c", strconv.Itoa(c), "-seed", fmt.Sprintf("pub%d", c)))
	}
	dup := fmt.Sprintf("%s/feeds/%s/raw/dup/same.bin", antSvc, slug)
	r := bench("put", "-url-template", dup+"?i={i}", "-n", "32", "-size", "4096", "-c", "16", "-dup", "-seed", "dup")
	res["duplicate-coordinate writers: 32 PUTs, 16 concurrent, same path, different bodies"] = slim(r)
	final := bench("get", "-url", dup, "-n", "1", "-hash").Obj("result")
	var created201 []any
	created201, _ = r.Obj("result").Obj("extra")["created_201_body_sha256"].([]any)
	match := false
	for d := range final.Obj("body_sha256_counts") {
		match = len(created201) == 1 && created201[0] == d
	}
	res["duplicate writers: final stored body"] = obj{"created_201_count": len(created201), "exactly_one_accepted": len(created201) == 1, "final_read_sha256_matches_accepted": match}
	return res
}

// sNPM runs the real npm client in a pod against a pull-through npm feed: cold install, warm install with an empty
// client cache (everything from Ant's cache), then the same install with the origin made unreachable.
func sNPM() obj {
	feed := newFeed("evb-npm-"+tag, "npm", "https://registry.npmjs.org", 60)
	slug := feed.Str("slug")
	srcs := antlib.Must(api.OK("GET", "/api/v1/feeds/"+feed.Str("id")+"/sources", nil)).Items()
	_ = srcs
	k("-n", ns, "delete", "pod", "evb-npm", "--ignore-not-found", "--wait=true")
	apply(obj{"apiVersion": "v1", "kind": "Pod", "metadata": obj{"name": "evb-npm"}, "spec": obj{"restartPolicy": "Never",
		"containers": []obj{{"name": "n", "image": "node:20-alpine", "command": []string{"sleep", "3600"}, "resources": obj{"requests": obj{"cpu": "100m", "memory": "256Mi"}, "limits": obj{"cpu": "2", "memory": "3Gi"}}}}}})
	k("-n", ns, "wait", "--for=condition=Ready", "pod/evb-npm", "--timeout=240s")
	tok, _ := antlib.TokenFor("~/.kuvryn-ant/" + ctx + "-automation-token.txt")
	// Tarball URLs in packuments carry the instance public URL, so the client pod needs its CA (kw uses a private CA).
	if pem, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k("-n", ns, "get", "secret", "ant-ingress-tls", "-o", "jsonpath={.data.ca\\.crt}"))); err == nil && len(pem) > 0 {
		caFile := filepath.Join(os.TempDir(), "evb-ca.pem")
		_ = os.WriteFile(caFile, pem, 0o600)
		k("-n", ns, "cp", caFile, "evb-npm:/tmp/ant-ca.pem")
	}
	reg := fmt.Sprintf("%s/feeds/%s/npm/", antSvc, slug)
	pubReg := strings.TrimPrefix(antlib.Instances[*target][0], "https://") + "/feeds/" + slug + "/npm/"
	script := func(cache string) string {
		return fmt.Sprintf("export NODE_EXTRA_CA_CERTS=/tmp/ant-ca.pem; cd /tmp && rm -rf proj node_modules && mkdir proj && cd proj && echo '{\"name\":\"evb\",\"version\":\"1.0.0\",\"dependencies\":{\"express\":\"4.21.2\",\"mocha\":\"10.8.2\"}}' > package.json && "+
			"printf 'registry=%s\\n//%s:_authToken=%s\\n//%s:_authToken=%s\\n' > .npmrc && "+
			"s=$(date +%%s.%%N); npm install --no-audit --no-fund --cache %s --prefer-online >/tmp/npm.log 2>&1; rc=$?; e=$(date +%%s.%%N); "+
			"echo rc=$rc; echo seconds=$(echo \"$e - $s\" | bc); echo packages=$(find node_modules -name package.json | wc -l); tail -n 6 /tmp/npm.log", reg, strings.TrimPrefix(reg, "http://"), tok, pubReg, tok, cache)
	}
	run := func(cache string) obj {
		o, err := antlib.Kubectl(ctx, "-n", ns, "exec", "evb-npm", "--", "sh", "-c", script(cache))
		r := obj{"raw": strings.ReplaceAll(strings.TrimSpace(o), tok, "***")}
		if err != nil {
			r["error"] = strings.ReplaceAll(err.Error(), tok, "***")
		}
		return r
	}
	res := obj{}
	res["npm install (express+mocha) cold: origin npmjs via Ant"] = run("/tmp/c1")
	res["npm install warm Ant cache, empty client cache"] = run("/tmp/c2")
	// outage: point the feed's remote source at an unreachable origin and repeat with a fresh client cache
	setOrigin(feed, "https://evb-origin-down.invalid", 5, false)
	res["npm install with origin unreachable, served from Ant cache"] = run("/tmp/c3")
	return res
}

func main() {
	flag.Parse()
	in, ok := antlib.Instances[*target]
	if !ok || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx = in[1]
	api = antlib.Named(*target)
	result["target"], result["base_url"], result["started"] = *target, in[0], antlib.Now()
	result["scenarios"] = obj{}
	save := func() {
		_ = os.MkdirAll(filepath.Dir(*out), 0o755)
		b, _ := json.MarshalIndent(result, "", " ")
		_ = os.WriteFile(*out, b, 0o644)
	}
	setup()
	defer func() {
		result["finished"] = antlib.Now()
		save()
		if !*keep {
			cleanup()
		}
	}()
	want := map[string]bool{}
	for _, s := range strings.Split(*scenFlag, ",") {
		want[s] = true
	}
	for _, sc := range []struct {
		name string
		fn   func() obj
	}{{"warm", sWarm}, {"cold", sCold}, {"fanout", sFanout}, {"large", sLarge}, {"outage", sOutage}, {"publish", sPublish}, {"npm", sNPM}, {"direct", sDirect}} {
		if !want[sc.name] {
			continue
		}
		fmt.Println("== scenario", sc.name)
		t0 := time.Now()
		func() {
			defer func() {
				if r := recover(); r != nil {
					result["scenarios"].(obj)[sc.name] = obj{"error": fmt.Sprint(r)}
				}
			}()
			r := sc.fn()
			r["_wall_seconds"] = time.Since(t0).Seconds()
			result["scenarios"].(obj)[sc.name] = r
		}()
		save()
	}
}
