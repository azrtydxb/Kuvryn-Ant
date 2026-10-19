package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oci"
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
	registry, err := oci.New(in.pool, in.repl.publisher, filepath.Join(in.t.TempDir(), "uploads"))
	if err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(func() { _ = registry.Close() })
	feeds := http.NewServeMux()
	feeds.Handle("/feeds/{slug}/raw/", raw)
	feeds.Handle("/v2/", registry)
	in.svc.SetRouteRuntime(federation.RouteRuntime{Feeds: feeds, Storage: in.repl.stores.Resolve, Lease: in.repl.mgr.Lease})
	in.mux.Handle("/feeds/{slug}/raw/", in.svc.RouteFeeds(false, raw))
	in.mux.Handle("/v2/", in.svc.RouteFeeds(true, registry))
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
				map[string]any{"kind": "feed", "effect": "allow", "feed_key": "lab/images", "actions": []string{"read"}},
				map[string]any{"kind": "artifact", "effect": "allow", "feed_key": "lab/images", "actions": []string{"read"}},
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
	// An OCI feed on alpha for digest-addressed reads (blob URLs).
	images, layer := alpha.ociFeed("images", "layer bytes from the home")
	alpha.bind("lab/images", images)
	if code := alpha.call("POST", "/api/v1/federation/feeds", map[string]any{"feed_key": "lab/images", "home_member_id": alpha.id}, nil); code != 201 {
		t.Fatalf("images feed key: %d", code)
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

	setImagesRouting := func(r map[string]any) {
		t.Helper()
		var f federation.TopologyFeed
		if code := alpha.call("PUT", "/api/v1/federation/feeds/lab/images/routing", r, &f); code != 200 || f.Routing == nil {
			t.Fatalf("set images routing: %d %+v", code, f)
		}
		syncAll()
	}

	t.Run("signed blob URLs only for digest-addressed reads", func(t *testing.T) {
		setRouting(map[string]any{"mode": "proxy", "blob_urls": true, "cache": false})
		setImagesRouting(map[string]any{"mode": "proxy", "blob_urls": true, "cache": false})
		// A raw path names mutable content: never a path-scoped ticket, the
		// read is proxied (and the holder authorizes the bytes it streams).
		got := gamma.fetch("GET", asset, ci["gamma"], nil)
		if got.status != 200 || got.body != "app bytes from the home" || !strings.HasPrefix(got.header.Get("X-Kuvryn-Route"), "proxy;") {
			t.Fatalf("raw read with blob URLs on: %d %q route=%q", got.status, got.body, got.header.Get("X-Kuvryn-Route"))
		}
		if code, ec := alpha.errorCode("PUT", "/api/v1/federation/feeds/lab/images/routing", map[string]any{"mode": "proxy", "blob_urls": false, "blob_url_bind_client": true, "cache": false}); code != 400 {
			t.Fatalf("client binding without blob URLs: %d %s", code, ec)
		}

		blobPath := "/v2/images/app/blobs/sha256:" + layer
		got = gamma.fetch("GET", blobPath, ci["gamma"], nil)
		loc := got.header.Get("Location")
		if got.status != 307 || !strings.HasPrefix(loc, alpha.api.URL+"/federation/blob/") || got.header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("blob url: %d %q %v", got.status, loc, got.header)
		}
		// No credentials needed: the URL carries the signed, re-authorized
		// caller, and the holder serves only bytes with the digest in the path.
		follow := alpha.fetch("GET", loc, "", nil)
		if follow.status != 200 || follow.body != "layer bytes from the home" || follow.header.Get("Docker-Content-Digest") != "sha256:"+layer {
			t.Fatalf("follow blob url: %d %q %v", follow.status, follow.body, follow.header)
		}
		u, _ := url.Parse(loc)
		q := u.Query()
		tampered := *u
		other := strings.Repeat("0", 64)
		tampered.RawQuery = url.Values{"k": q["k"], "p": {"oci/app/blobs/sha256:" + other}, "t": q["t"]}.Encode()
		tampered.Path = "/federation/blob/sha256:" + other
		if got := alpha.fetch("GET", tampered.String(), "", nil); got.status != 403 {
			t.Fatalf("ticket for another digest: %d", got.status)
		}
		// A ticket can never be re-pointed at a mutable path.
		mutable := *u
		mutable.RawQuery = url.Values{"k": {"lab/pkgs"}, "p": {"raw/tools/app.tgz"}, "t": q["t"]}.Encode()
		mutable.Path = "/federation/blob/app.tgz"
		if got := alpha.fetch("GET", mutable.String(), "", nil); got.status != 403 || strings.Contains(got.body, "app bytes") {
			t.Fatalf("ticket on a mutable path: %d %q", got.status, got.body)
		}
		// Other members cannot use a URL issued for this holder.
		if got := beta.fetch("GET", beta.api.URL+u.RequestURI(), "", nil); got.status != 403 {
			t.Fatalf("wrong audience: %d", got.status)
		}
		alpha.clock.Advance(7 * time.Minute)
		if got := alpha.fetch("GET", loc, "", nil); got.status != 403 {
			t.Fatalf("expired blob url: %d", got.status)
		}
		alpha.clock.Reset()
		// A caller without a grant gets a URL the holder refuses to serve.
		if g := gamma.fetch("GET", blobPath, guest, nil); g.status == 307 {
			if follow := alpha.fetch("GET", g.header.Get("Location"), "", nil); follow.status == 200 {
				t.Fatalf("guest blob url served: %q", follow.body)
			}
		} else if g.status == 200 {
			t.Fatalf("guest read served: %q", g.body)
		}

		// Optional client binding: the URL is valid only from the client
		// address the entry member saw, and the binding cannot be stripped.
		setImagesRouting(map[string]any{"mode": "proxy", "blob_urls": true, "blob_url_bind_client": true, "cache": false})
		got = gamma.fetch("GET", blobPath, ci["gamma"], nil)
		loc = got.header.Get("Location")
		if got.status != 307 || !strings.Contains(loc, "b=ip") {
			t.Fatalf("bound blob url: %d %q", got.status, loc)
		}
		if follow := alpha.fetch("GET", loc, "", nil); follow.status != 200 || follow.body != "layer bytes from the home" {
			t.Fatalf("bound blob url from its client: %d %q", follow.status, follow.body)
		}
		u, _ = url.Parse(loc)
		stripped := *u
		sq := u.Query()
		sq.Del("b")
		stripped.RawQuery = sq.Encode()
		if got := alpha.fetch("GET", stripped.String(), "", nil); got.status != 403 {
			t.Fatalf("binding stripped: %d", got.status)
		}
		setImagesRouting(map[string]any{"mode": "proxy", "blob_urls": false, "cache": false})
	})

	t.Run("entry cache: digest-addressed only, revalidated otherwise, invalidated by home deletes", func(t *testing.T) {
		setRouting(map[string]any{"mode": "proxy", "blob_urls": false, "cache": true, "cache_ttl_seconds": 600})
		cacheRows := func(in *instance) int {
			var n int
			_ = in.pool.QueryRow(ctx, `SELECT count(*) FROM federation_route_cache WHERE instance_id=$1`, in.id).Scan(&n)
			return n
		}
		alpha.rawPublish(pkgs, "tools/cached.tgz", "cache me")
		got := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", ci["beta"], nil)
		if got.status != 200 || got.body != "cache me" {
			t.Fatalf("first read: %d %q", got.status, got.body)
		}
		eventually(t, "cache entry", func() bool { return cacheRows(beta) == 1 })
		// A raw path is not digest-addressed: with every holder down the
		// cached copy is not served unrevalidated.
		alpha.proxy.set(faultDrop)
		delta.proxy.set(faultDrop)
		down := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", ci["beta"], nil)
		alpha.proxy.set(faultPass)
		delta.proxy.set(faultPass)
		if down.status == 200 || strings.Contains(down.body, "cache me") {
			t.Fatalf("unrevalidated cache answer for a mutable path: %d %q %v", down.status, down.body, down.header)
		}
		hit := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", ci["beta"], nil)
		if hit.status != 200 || hit.body != "cache me" || !strings.HasPrefix(hit.header.Get("X-Kuvryn-Route"), "cache; revalidated") ||
			hit.header.Get("X-Content-Type-Options") != "nosniff" || hit.header.Get("Content-Security-Policy") != "sandbox" {
			t.Fatalf("revalidated cache hit: %d %q %v", hit.status, hit.body, hit.header)
		}
		// The cache never answers callers this member does not let read the feed.
		guestBeta := beta.tokenFor(beta.principal("guest").ID)
		if got := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", guestBeta, nil); got.status == 200 {
			t.Fatalf("guest served from cache: %q", got.body)
		}
		// Overwritten at the home: the revalidation returns the new bytes.
		req, _ := http.NewRequest("DELETE", alpha.api.URL+"/feeds/pkgs/raw/tools/cached.tgz", nil)
		req.Header.Set("Authorization", "Bearer "+alpha.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("home delete: %d", resp.StatusCode)
		}
		if got := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", ci["beta"], nil); got.status != 404 || strings.Contains(got.body, "cache me") {
			t.Fatalf("deleted at the home but served: %d %q", got.status, got.body)
		}
		if n := cacheRows(beta); n != 0 {
			t.Fatalf("home 404 did not drop the entry: %d rows", n)
		}
		alpha.rawPublish(pkgs, "tools/cached.tgz", "cache me again, changed")
		if got := beta.fetch("GET", "/feeds/pkgs/raw/tools/cached.tgz", ci["beta"], nil); got.status != 200 || got.body != "cache me again, changed" {
			t.Fatalf("changed at the home: %d %q", got.status, got.body)
		}
		eventually(t, "cache entry again", func() bool { return cacheRows(beta) == 1 })
		// A delete replicated from the home invalidates the feed's entries.
		req, _ = http.NewRequest("DELETE", alpha.api.URL+"/feeds/pkgs/raw/tools/cached.tgz", nil)
		req.Header.Set("Authorization", "Bearer "+alpha.token)
		if resp, err = http.DefaultClient.Do(req); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		beta.mustRun(beta.jobFor("lab/pkgs").ID)
		if n := cacheRows(beta); n != 0 {
			t.Fatalf("replicated home delete left %d cache rows", n)
		}
	})
}

// ociFeed creates an OCI feed with one local source (default publish target)
// and stores one blob in repository app; it returns the feed and the blob
// digest (hex).
func (in *instance) ociFeed(slug, content string) (feed, digest string) {
	in.t.Helper()
	ctx := context.Background()
	must := func(err error) {
		in.t.Helper()
		if err != nil {
			in.t.Fatal(err)
		}
	}
	var source string
	must(in.pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,$2,'oci') RETURNING id::text`, in.id, slug).Scan(&feed))
	must(in.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,'local','oci') RETURNING id::text`, in.id, slug+"-local").Scan(&source))
	_, err := in.pool.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, in.id, "source:"+source)
	must(err)
	_, err = in.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, in.id, feed, source)
	must(err)
	_, err = in.pool.Exec(ctx, `INSERT INTO publish_policies(instance_id,feed_id,default_source_id) VALUES($1,$2,$3)`, in.id, feed, source)
	must(err)
	sum := sha256.Sum256([]byte(content))
	digest = hex.EncodeToString(sum[:])
	_, err = in.repl.publisher.Publish(ctx, in.repl.admin, artifact.Publication{FeedID: feed, Coordinate: "app", Version: digest, AssetKey: "blob",
		MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(digest), Size: int64(len(content))}}, bytes.NewReader([]byte(content)))
	must(err)
	return feed, digest
}
