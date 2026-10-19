package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/rawrepo"
)

// enableRouting mounts the raw adapter behind the federation router on the
// instance's HTTP server (its membership API URL) and wires the holder side:
// proxied reads and signed blob URLs are answered by the unwrapped adapter.
func (in *instance) enableRouting() {
	in.t.Helper()
	raw, err := rawrepo.New(in.pool, in.repl.publisher)
	if err != nil {
		in.t.Fatal(err)
	}
	feeds := http.NewServeMux()
	feeds.Handle("/feeds/{slug}/raw/", raw)
	in.svc.SetRouteRuntime(federation.RouteRuntime{Feeds: feeds, Storage: in.repl.stores.Resolve, Lease: in.repl.mgr.Lease})
	in.mux.Handle("/feeds/{slug}/raw/", in.svc.RouteFeeds(false, raw))
	in.mux.Handle("GET /federation/blob/{name}", in.svc.BlobHandler())
	management.RegisterConsole(in.mux, in.pool, management.NewCursorCodec(make([]byte, 32)))
}

// rawPublish stores content at a raw path of a local feed.
func (in *instance) rawPublish(feed, key, content string) {
	in.t.Helper()
	sum := sha256.Sum256([]byte(content))
	ns := ""
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		ns = key[:i]
	}
	_, err := in.repl.publisher.Publish(context.Background(), in.repl.admin, artifact.Publication{FeedID: feed, Namespace: ns, Coordinate: key, Version: "first", AssetKey: key,
		MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(content))}}, bytes.NewReader([]byte(content)))
	if err != nil {
		in.t.Fatalf("%s raw publish %s: %v", in.name, key, err)
	}
}

type fetched struct {
	status int
	body   string
	header http.Header
}

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 30 * time.Second}

// fetch reads a URL (relative paths go to the instance's HTTP server).
func (in *instance) fetch(method, path, token string, header map[string]string) fetched {
	in.t.Helper()
	target := path
	if strings.HasPrefix(path, "/") {
		target = in.api.URL + path
	}
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		in.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := noRedirects.Do(req)
	if err != nil {
		in.t.Fatalf("%s %s %s: %v", in.name, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return fetched{status: resp.StatusCode, body: string(b), header: resp.Header}
}

func (in *instance) routePlan(key string) federation.RoutePlan {
	in.t.Helper()
	var plan federation.RoutePlan
	fed, slug, _ := strings.Cut(key, "/")
	if code := in.call("GET", "/api/v1/federation/routes/"+fed+"/"+slug, nil, &plan); code != 200 {
		in.t.Fatalf("%s route plan %s: %d", in.name, key, code)
	}
	return plan
}

func order(plan federation.RoutePlan) string {
	var out []string
	for _, c := range plan.Candidates {
		if c.Eligible {
			out = append(out, c.Name)
		}
	}
	return strings.Join(out, ",")
}

func routedBy(f fetched) string {
	v := f.header.Get("X-Kuvryn-Route")
	if _, m, ok := strings.Cut(v, "member="); ok {
		return m
	}
	return v
}

func routingPolicy() map[string]any {
	return map[string]any{
		"identity_domains": []any{map[string]any{"id": "idp", "type": "oidc", "issuer": idpIssuer}},
		"roles": []any{
			map[string]any{"key": "fed:readers", "revision": 1, "grants": []any{
				map[string]any{"kind": "feed", "effect": "allow", "feed_key": "lab/pkgs", "actions": []string{"read"}},
				map[string]any{"kind": "artifact", "effect": "allow", "feed_key": "lab/pkgs", "actions": []string{"read", "publish"}},
			}},
			map[string]any{"key": "fed:other", "revision": 1, "grants": []any{
				map[string]any{"kind": "artifact", "effect": "allow", "feed_key": "lab/other", "actions": []string{"read"}},
			}},
		},
		"principal_bindings": []any{},
		"group_bindings":     []any{},
		"service_accounts": []any{
			map[string]any{"name": "ci", "roles": []string{"fed:readers"}},
			map[string]any{"name": "guest", "roles": []string{"fed:other"}},
		},
		"revocations": []any{},
	}
}

func TestFederationRoutingScenarios(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma, delta := newInstance(t, "alpha"), newInstance(t, "beta"), newInstance(t, "gamma"), newInstance(t, "delta")
	all := []*instance{alpha, beta, gamma, delta}
	for _, in := range all {
		in.enableReplication()
		in.enableRouting()
	}
	if code := alpha.call("POST", "/api/v1/federation", map[string]any{"name": "lab", "member_name": "alpha", "labels": map[string]string{"site": "alpha"}}, nil); code != 201 {
		t.Fatalf("found: %d", code)
	}
	for _, in := range all[1:] {
		if code := in.join(alpha, alpha.invite(in.name)); code != 200 {
			t.Fatalf("join %s: %d", in.name, code)
		}
	}
	syncAll := func() {
		t.Helper()
		for _, in := range all[1:] {
			in.sync()
		}
	}
	syncAll()

	// Home feed on alpha, policy with a reader (ci) and a federated principal
	// without a grant on the key (guest).
	pkgs, _ := alpha.localFeed("pkgs")
	alpha.rawPublish(pkgs, "tools/app.tgz", "app bytes from the home")
	alpha.bind("lab/pkgs", pkgs)
	if code := alpha.call("POST", "/api/v1/federation/feeds", map[string]any{"feed_key": "lab/pkgs", "home_member_id": alpha.id}, nil); code != 201 {
		t.Fatalf("feed key: %d", code)
	}
	if code := alpha.call("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": routingPolicy()}, nil); code != 200 {
		t.Fatalf("draft: %d", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 0, "confirm": true}, nil); code != 200 {
		t.Fatalf("publish: %d", code)
	}
	syncAll()
	eventually(t, "policy everywhere", func() bool {
		return beta.policyGeneration() == 1 && gamma.policyGeneration() == 1 && delta.policyGeneration() == 1
	})
	ci := map[string]string{}
	for _, in := range all {
		ci[in.name] = in.tokenFor(in.principal("ci").ID)
	}
	guest := gamma.tokenFor(gamma.principal("guest").ID)
	const asset = "/feeds/pkgs/raw/tools/app.tgz"

	t.Run("proxied read from the home with Range and HEAD", func(t *testing.T) {
		// gamma holds no feed for lab/pkgs: the read is proxied to the home.
		got := gamma.fetch("GET", asset, ci["gamma"], nil)
		if got.status != 200 || got.body != "app bytes from the home" || routedBy(got) != "alpha" {
			t.Fatalf("proxied read: %d %q route=%q", got.status, got.body, got.header.Get("X-Kuvryn-Route"))
		}
		if got.header.Get("Set-Cookie") != "" || got.header.Get("X-Ant-Fed-Proxied") != "" {
			t.Fatalf("internal headers leaked: %v", got.header)
		}
		part := gamma.fetch("GET", asset, ci["gamma"], map[string]string{"Range": "bytes=0-2"})
		if part.status != 206 || part.body != "app" || part.header.Get("Content-Range") == "" {
			t.Fatalf("range: %d %q %v", part.status, part.body, part.header)
		}
		head := gamma.fetch("HEAD", asset, ci["gamma"], nil)
		if head.status != 200 || head.body != "" || head.header.Get("Content-Length") != "23" {
			t.Fatalf("head: %d %v", head.status, head.header)
		}
		plan := gamma.routePlan("lab/pkgs")
		if plan.Decision != "proxy" || plan.LocalRole != "unbound" || order(plan) != "alpha" || plan.Candidates[0].Role != "home" {
			t.Fatalf("explain: %+v", plan)
		}
	})

	t.Run("each member applies its own authorization", func(t *testing.T) {
		// guest is federated but has no grant on lab/pkgs: the home refuses
		// (its adapter hides the asset) and nothing is served.
		if got := gamma.fetch("GET", asset, guest, nil); got.status != 404 || strings.Contains(got.body, "app bytes") {
			t.Fatalf("guest read: %d %q", got.status, got.body)
		}
		// A local-only principal cannot be asserted to another member.
		if got := gamma.fetch("GET", asset, gamma.token, nil); got.status != 404 || got.header.Get("X-Kuvryn-Route") != "not_federated" {
			t.Fatalf("local principal: %d %v", got.status, got.header)
		}
		// Missing content: every holder answers 404 and so does the entry member.
		if got := gamma.fetch("GET", "/feeds/pkgs/raw/tools/none.tgz", ci["gamma"], nil); got.status != 404 {
			t.Fatalf("missing content: %d", got.status)
		}
		if n := alpha.auditCount("federation.assertion.accepted", "success"); n == 0 {
			t.Fatal("the holder did not record the asserted caller")
		}
	})

	t.Run("writes are never routed", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", gamma.api.URL+"/feeds/pkgs/raw/tools/new.tgz", strings.NewReader("new"))
		req.Header.Set("Authorization", "Bearer "+ci["gamma"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == 201 || resp.StatusCode == 200 || resp.Header.Get("Ant-Feed-Home") != alpha.api.URL+"/feeds/pkgs/raw/" {
			t.Fatalf("non-home publish: %d home=%q", resp.StatusCode, resp.Header.Get("Ant-Feed-Home"))
		}
		if alpha.hasVersion(pkgs, "tools/new.tgz", "first") {
			t.Fatal("a publish reached the home")
		}
		// The holder refuses non-read methods on the proxy endpoint outright.
		id, base, err := gamma.svc.ReplicationIdentity(alpha.id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (peer.Client{Timeout: 5 * time.Second}).Do(ctx, id, base, "POST", "/federation/v1/proxy/feeds/lab/pkgs/raw/tools/x.tgz", []byte("x"), alpha.id); peerCode(err) != "writes_not_routed" {
			t.Fatalf("peer write: %v", err)
		}
	})

	// Replicas on beta and delta.
	var rule federation.ReplicationRule
	if code := alpha.call("POST", "/api/v1/federation/replication/rules", map[string]any{"feed_key": "lab/pkgs", "targets": map[string]any{"members": []string{beta.id, delta.id}}}, &rule); code != 201 {
		t.Fatalf("rule: %d", code)
	}
	syncAll()
	for _, in := range []*instance{beta, delta} {
		in.reconcile()
		in.mustRun(in.jobFor("lab/pkgs").ID)
		if st := in.ruleState(rule.ID); st.State != "in_sync" {
			t.Fatalf("%s replica: %+v", in.name, st)
		}
	}

	t.Run("latency-based selection among replicas, home last", func(t *testing.T) {
		beta.proxy.setDelay(150 * time.Millisecond)
		defer beta.proxy.set(faultPass)
		for range 3 {
			gamma.svc.PingMembers(ctx)
		}
		plan := gamma.routePlan("lab/pkgs")
		if order(plan) != "delta,beta,alpha" {
			t.Fatalf("candidate order: %s (%+v)", order(plan), plan.Candidates)
		}
		for _, c := range plan.Candidates {
			if c.Role == "replica" && (c.ReplicaState != "in_sync" || c.RTTMS == nil) {
				t.Fatalf("replica signals missing: %+v", c)
			}
		}
		if got := gamma.fetch("GET", asset, ci["gamma"], nil); got.status != 200 || routedBy(got) != "delta" {
			t.Fatalf("latency selection: %d %q", got.status, got.header.Get("X-Kuvryn-Route"))
		}
	})

	t.Run("a local replica miss is routed", func(t *testing.T) {
		// Published on the home but not replicated yet: beta misses locally
		// (and delta too), the home answers.
		alpha.rawPublish(pkgs, "tools/fresh.tgz", "fresh from the home")
		got := beta.fetch("GET", "/feeds/pkgs/raw/tools/fresh.tgz", ci["beta"], nil)
		if got.status != 200 || got.body != "fresh from the home" || routedBy(got) != "alpha" {
			t.Fatalf("replica miss: %d %q route=%q", got.status, got.body, got.header.Get("X-Kuvryn-Route"))
		}
		// Local content is still served locally (no route header).
		if got := beta.fetch("GET", asset, ci["beta"], nil); got.status != 200 || got.header.Get("X-Kuvryn-Route") != "" {
			t.Fatalf("local hit: %d %v", got.status, got.header)
		}
		var explain struct {
			Status     string                `json:"status"`
			Federation *federation.RoutePlan `json:"federation"`
		}
		if code := beta.call("GET", "/api/v1/feeds/"+beta.boundFeed("lab/pkgs")+"/resolve?coordinate=tools/none.tgz", nil, &explain); code != 200 ||
			explain.Status != "federated" || explain.Federation == nil || explain.Federation.LocalRole != "replica" || order(*explain.Federation) != "delta,alpha" {
			t.Fatalf("resolve explain: %d %+v", code, explain)
		}
	})

	t.Run("member down: next candidate, circuit, clean 503", func(t *testing.T) {
		delta.proxy.set(faultDrop)
		for range 3 {
			if got := gamma.fetch("GET", asset, ci["gamma"], nil); got.status != 200 || routedBy(got) != "beta" {
				t.Fatalf("failover: %d %q", got.status, got.header.Get("X-Kuvryn-Route"))
			}
		}
		plan := gamma.routePlan("lab/pkgs")
		for _, c := range plan.Candidates {
			if c.Name == "delta" && (c.Circuit != "open" || c.Eligible) {
				t.Fatalf("delta circuit: %+v", c)
			}
		}
		beta.proxy.set(faultDrop)
		alpha.proxy.set(faultDrop)
		got := gamma.fetch("GET", asset, ci["gamma"], nil)
		if got.status != 503 || got.header.Get("Retry-After") == "" || strings.Contains(got.body, "app bytes") {
			t.Fatalf("all down: %d %q", got.status, got.body)
		}
		for _, in := range []*instance{alpha, beta, delta} {
			in.proxy.set(faultPass)
		}
		// Open circuits close after their open period (half-open trial).
		// (within the 60 s signature skew budget of the other members)
		gamma.clock.Advance(35 * time.Second)
		defer gamma.clock.Reset()
		if got := gamma.fetch("GET", asset, ci["gamma"], nil); got.status != 200 {
			t.Fatalf("recovered: %d %q", got.status, got.body)
		}
	})

	setRouting := func(r map[string]any) {
		t.Helper()
		var f federation.TopologyFeed
		if code := alpha.call("PUT", "/api/v1/federation/feeds/lab/pkgs/routing", r, &f); code != 200 || f.Routing == nil {
			t.Fatalf("set routing: %d %+v", code, f)
		}
		syncAll()
	}

	t.Run("redirect mode", func(t *testing.T) {
		if code, ec := alpha.errorCode("PUT", "/api/v1/federation/feeds/lab/pkgs/routing", map[string]any{"mode": "redirect", "blob_urls": true, "cache": false}); code != 400 {
			t.Fatalf("blob_urls without proxy: %d %s", code, ec)
		}
		if code, ec := beta.errorCode("PUT", "/api/v1/federation/feeds/lab/pkgs/routing", map[string]any{"mode": "redirect", "blob_urls": false, "cache": false}); code != 409 || ec != "not_authority" {
			t.Fatalf("routing on a member: %d %s", code, ec)
		}
		setRouting(map[string]any{"mode": "redirect", "blob_urls": false, "cache": false})
		got := gamma.fetch("GET", asset+"?x=1", ci["gamma"], nil)
		loc := got.header.Get("Location")
		if got.status != 307 || !strings.HasSuffix(loc, "/feeds/pkgs/raw/tools/app.tgz?x=1") || !strings.HasPrefix(loc, delta.api.URL) && !strings.HasPrefix(loc, beta.api.URL) {
			t.Fatalf("redirect: %d %q", got.status, loc)
		}
		// The client authenticates at the holder on its own.
		holder := delta
		if strings.HasPrefix(loc, beta.api.URL) {
			holder = beta
		}
		if follow := holder.fetch("GET", loc, ci[holder.name], nil); follow.status != 200 || follow.body != "app bytes from the home" {
			t.Fatalf("follow redirect: %d %q", follow.status, follow.body)
		}
		// A replica that misses proxies instead of redirecting (no chains).
		alpha.rawPublish(pkgs, "tools/late.tgz", "late")
		if got := beta.fetch("GET", "/feeds/pkgs/raw/tools/late.tgz", ci["beta"], nil); got.status != 200 || got.body != "late" || routedBy(got) != "alpha" {
			t.Fatalf("replica miss in redirect mode: %d %q %q", got.status, got.body, got.header.Get("X-Kuvryn-Route"))
		}
		if plan := beta.routePlan("lab/pkgs"); plan.Decision != "proxy" || plan.Routing.Mode != "redirect" {
			t.Fatalf("replica plan in redirect mode: %+v", plan)
		}
	})

	t.Run("signed blob URLs", func(t *testing.T) {
		setRouting(map[string]any{"mode": "proxy", "blob_urls": true, "cache": false})
		got := gamma.fetch("GET", asset, ci["gamma"], nil)
		loc := got.header.Get("Location")
		if got.status != 307 || !strings.Contains(loc, "/federation/blob/app.tgz?") {
			t.Fatalf("blob url: %d %q", got.status, loc)
		}
		holder := delta
		if strings.HasPrefix(loc, beta.api.URL) {
			holder = beta
		}
		// No credentials needed: the URL carries the signed, re-authorized caller.
		if follow := holder.fetch("GET", loc, "", nil); follow.status != 200 || follow.body != "app bytes from the home" {
			t.Fatalf("follow blob url: %d %q", follow.status, follow.body)
		}
		u, _ := url.Parse(loc)
		q := u.Query()
		tampered := *u
		q2 := url.Values{"k": q["k"], "p": {"raw/tools/fresh.tgz"}, "t": q["t"]}
		tampered.RawQuery = q2.Encode()
		if got := holder.fetch("GET", tampered.String(), "", nil); got.status != 403 {
			t.Fatalf("ticket for another path: %d", got.status)
		}
		// Other members cannot use a URL issued for this holder.
		other := beta
		if holder == beta {
			other = delta
		}
		if got := other.fetch("GET", other.api.URL+u.RequestURI(), "", nil); got.status != 403 {
			t.Fatalf("wrong audience: %d", got.status)
		}
		holder.clock.Advance(7 * time.Minute)
		if got := holder.fetch("GET", loc, "", nil); got.status != 403 {
			t.Fatalf("expired blob url: %d", got.status)
		}
		holder.clock.Reset()
		// A caller without a grant gets a URL the holder refuses to serve.
		g := gamma.fetch("GET", asset, guest, nil)
		if g.status == 307 {
			if follow := holder.fetch("GET", g.header.Get("Location"), "", nil); follow.status == 200 {
				t.Fatalf("guest blob url served: %q", follow.body)
			}
		}
		// Metadata-like (non-immutable) paths are still proxied.
		alpha.rawPublish(pkgs, "tools/index.json", `{"ok":true}`)
		if got := gamma.fetch("GET", "/feeds/pkgs/raw/tools/index.json", ci["gamma"], nil); got.status != 200 || got.body != `{"ok":true}` {
			t.Fatalf("proxied metadata: %d %q", got.status, got.body)
		}
	})

	t.Run("entry cache on a member with a bound feed", func(t *testing.T) {
		setRouting(map[string]any{"mode": "proxy", "blob_urls": false, "cache": true, "cache_ttl_seconds": 600})
		alpha.rawPublish(pkgs, "tools/cached.tgz", "cache me")
		got := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", ci["beta"], nil)
		if got.status != 200 || got.body != "cache me" {
			t.Fatalf("first read: %d %q", got.status, got.body)
		}
		eventually(t, "cache entry", func() bool {
			var n int
			_ = beta.pool.QueryRow(ctx, `SELECT count(*) FROM federation_route_cache WHERE instance_id=$1`, beta.id).Scan(&n)
			return n == 1
		})
		alpha.proxy.set(faultDrop)
		delta.proxy.set(faultDrop)
		defer alpha.proxy.set(faultPass)
		defer delta.proxy.set(faultPass)
		hit := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", ci["beta"], nil)
		if hit.status != 200 || hit.body != "cache me" || hit.header.Get("X-Kuvryn-Route") != "cache" {
			t.Fatalf("cache hit: %d %q %v", hit.status, hit.body, hit.header)
		}
		// The cache never answers callers this member does not let read the feed.
		guestBeta := beta.tokenFor(beta.principal("guest").ID)
		if got := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", guestBeta, nil); got.status == 200 {
			t.Fatalf("guest served from cache: %q", got.body)
		}
	})
}
