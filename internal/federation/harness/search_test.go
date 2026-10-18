package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// seedPackage writes a component (and optionally one asset with the digest of
// content) straight into a member's catalog, owned by a source of the kind
// attached to the feed.
func (in *instance) seedPackage(feed, kind, coordinate, namespace, content string) {
	in.t.Helper()
	ctx := context.Background()
	var source string
	err := in.pool.QueryRow(ctx, `SELECT s.id::text FROM feed_sources fs JOIN sources s ON (s.instance_id,s.id)=(fs.instance_id,fs.source_id) WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.kind=$3 ORDER BY fs.priority LIMIT 1`, in.id, feed, kind).Scan(&source)
	if err != nil {
		if err = in.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind) VALUES($1,$2,$3) RETURNING id::text`, in.id, fmt.Sprintf("seed-%s-%d", kind, dbCounter.Add(1)), kind).Scan(&source); err != nil {
			in.t.Fatalf("%s seed source: %v", in.name, err)
		}
		if _, err = in.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,(SELECT COALESCE(max(priority)+1,0) FROM feed_sources WHERE feed_id=$2))`, in.id, feed, source); err != nil {
			in.t.Fatalf("%s attach source: %v", in.name, err)
		}
	}
	var component string
	if err = in.pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,$4) RETURNING id::text`, in.id, source, coordinate, namespace).Scan(&component); err != nil {
		in.t.Fatalf("%s seed component: %v", in.name, err)
	}
	if content == "" {
		return
	}
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	var domain, version, blob string
	if err = in.pool.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) ON CONFLICT(instance_id,name) DO UPDATE SET name=EXCLUDED.name RETURNING id::text`, in.id, "seed").Scan(&domain); err != nil {
		in.t.Fatalf("%s seed domain: %v", in.name, err)
	}
	if err = in.pool.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'1.0.0') RETURNING id::text`, in.id, component).Scan(&version); err != nil {
		in.t.Fatalf("%s seed version: %v", in.name, err)
	}
	if err = in.pool.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$5) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET size_bytes=EXCLUDED.size_bytes RETURNING id::text`, in.id, domain, digest, len(content), "seed/"+digest).Scan(&blob); err != nil {
		in.t.Fatalf("%s seed blob: %v", in.name, err)
	}
	if _, err = in.pool.Exec(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'file',$3)`, in.id, version, blob); err != nil {
		in.t.Fatalf("%s seed asset: %v", in.name, err)
	}
}

type searchResponse struct {
	Items   []federation.MergedHit          `json:"items"`
	Members []federation.MemberSearchStatus `json:"members"`
	Partial bool                            `json:"partial"`
	Page    struct {
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	} `json:"page"`
}

func (r searchResponse) coordinates() string {
	out := make([]string, len(r.Items))
	for i, h := range r.Items {
		out[i] = h.Coordinate
	}
	return strings.Join(out, ",")
}

func (r searchResponse) member(name string) federation.MemberSearchStatus {
	for _, m := range r.Members {
		if m.Name == name {
			return m
		}
	}
	return federation.MemberSearchStatus{Name: name, Status: "absent"}
}

func (in *instance) search(token string, params url.Values) (int, searchResponse) {
	in.t.Helper()
	var out searchResponse
	code := in.callAs(token, "GET", "/api/v1/federation/search?"+params.Encode(), nil, &out)
	return code, out
}

// searchFederation builds the three-member federation of the search scenarios:
// every member binds its own "release" feed to lab/release, and the federated
// service account "ci" may read it everywhere under the shared policy.
func searchFederation(t *testing.T) (alpha, beta, gamma *instance, feeds map[string]string, ci map[string]string) {
	t.Helper()
	ctx := context.Background()
	alpha, beta, gamma = newInstance(t, "alpha"), newInstance(t, "beta"), newInstance(t, "gamma")
	if code := alpha.call("POST", "/api/v1/federation", map[string]any{"name": "lab", "member_name": "alpha"}, nil); code != 201 {
		t.Fatalf("found: %d", code)
	}
	for _, in := range []*instance{beta, gamma} {
		if code := in.join(alpha, alpha.invite(in.name)); code != 200 {
			t.Fatalf("join %s: %d", in.name, code)
		}
	}
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	feeds = map[string]string{}
	for _, in := range []*instance{alpha, beta, gamma} {
		feeds[in.name] = in.createFeed("release")
		in.bind("lab/release", feeds[in.name])
	}
	if code := alpha.call("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": policyBody()}, nil); code != 200 {
		t.Fatalf("draft: %d", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 0, "confirm": true}, nil); code != 200 {
		t.Fatalf("publish: %d", code)
	}
	eventually(t, "policy on every member", func() bool { return beta.policyGeneration() == 1 && gamma.policyGeneration() == 1 })
	ci = map[string]string{}
	for _, in := range []*instance{alpha, beta, gamma} {
		ci[in.name] = in.tokenFor(in.principal("ci").ID)
	}
	return
}

func TestFederatedSearch(t *testing.T) {
	alpha, beta, gamma, feeds, ci := searchFederation(t)

	// Catalogs. "shared" exists on all three (replica on beta); the secret
	// namespace is denied for ci by a local role on gamma only.
	alpha.seedPackage(feeds["alpha"], "local", "pkg-a", "public", "alpha-blob")
	alpha.seedPackage(feeds["alpha"], "local", "shared", "public", "shared-blob")
	beta.seedPackage(feeds["beta"], "replica", "shared", "public", "shared-blob")
	beta.seedPackage(feeds["beta"], "local", "pkg-b", "public", "")
	beta.seedPackage(feeds["beta"], "local", "secret-b", "secret", "")
	gamma.seedPackage(feeds["gamma"], "local", "pkg-g", "public", "")
	gamma.seedPackage(feeds["gamma"], "local", "secret-g", "secret", "")
	gamma.seedPackage(feeds["gamma"], "local", "shared", "public", "shared-blob")
	var block struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if code := gamma.call("POST", "/api/v1/roles", map[string]any{"name": "block-secret"}, &block); code != 201 {
		t.Fatalf("local role: %d", code)
	}
	gamma.mustIfMatch("PUT", "/api/v1/roles/"+block.ID+"/grants", block.Revision, map[string]any{"grants": []any{map[string]any{"kind": "artifact", "effect": "deny", "feed_id": feeds["gamma"], "namespace": "secret", "actions": []string{"read"}}}})
	var princ struct {
		Items []struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		} `json:"items"`
	}
	gamma.call("GET", "/api/v1/principals?limit=200", nil, &princ)
	gci := gamma.principal("ci").ID
	for _, p := range princ.Items {
		if p.ID == gci {
			gamma.mustIfMatch("PUT", "/api/v1/principals/"+gci+"/roles", p.Revision, map[string]any{"role_ids": []string{block.ID}})
		}
	}

	t.Run("results merge across three members and each member authorizes", func(t *testing.T) {
		code, res := alpha.search(ci["alpha"], url.Values{"limit": {"50"}})
		if code != 200 || res.Partial || len(res.Members) != 3 {
			t.Fatalf("search: %d partial=%v members=%+v", code, res.Partial, res.Members)
		}
		// gamma's local deny hides secret-g; beta has no such rule so secret-b is visible.
		if got := res.coordinates(); got != "pkg-a,pkg-b,pkg-g,secret-b,shared" {
			t.Fatalf("merged coordinates: %s", got)
		}
		for _, m := range res.Members {
			if m.Status != "ok" {
				t.Fatalf("member %+v", m)
			}
		}
		if g := res.member("gamma"); g.Hits != 2 {
			t.Fatalf("gamma must return only what it authorizes: %+v", g)
		}
		for _, h := range res.Items {
			if h.Coordinate != "shared" {
				continue
			}
			if len(h.Copies) != 3 || !h.Home || h.Copies[2].Home || h.Copies[2].MemberName != "beta" || h.FeedKey != "lab/release" {
				t.Fatalf("shared must collapse replica under the home copy: %+v", h)
			}
		}
		// The same query from another entry member yields the same answer.
		code, other := gamma.search(ci["gamma"], url.Values{})
		if code != 200 || other.coordinates() != res.coordinates() {
			t.Fatalf("entry at gamma: %d %s", code, other.coordinates())
		}
	})

	t.Run("filters", func(t *testing.T) {
		_, res := beta.search(ci["beta"], url.Values{"q": {"PKG"}})
		if res.coordinates() != "pkg-a,pkg-b,pkg-g" {
			t.Fatalf("q filter: %s", res.coordinates())
		}
		_, res = beta.search(ci["beta"], url.Values{"namespace": {"secret"}})
		if res.coordinates() != "secret-b" {
			t.Fatalf("namespace filter must still be authorized per member: %s", res.coordinates())
		}
		sum := sha256.Sum256([]byte("shared-blob"))
		_, res = beta.search(ci["beta"], url.Values{"digest": {"sha256:" + hex.EncodeToString(sum[:])}})
		if res.coordinates() != "shared" || len(res.Items[0].Copies) != 3 {
			t.Fatalf("checksum lookup: %+v", res.Items)
		}
		_, res = beta.search(ci["beta"], url.Values{"members": {"gamma"}})
		if res.coordinates() != "pkg-b,pkg-g,secret-b,shared" || res.member("alpha").Status != "absent" {
			t.Fatalf("members filter: %s %+v", res.coordinates(), res.Members)
		}
		if code := beta.callAs(ci["beta"], "GET", "/api/v1/federation/search?members=nowhere", nil, nil); code != 400 {
			t.Fatalf("unknown member: %d", code)
		}
	})

	t.Run("pagination merges and rejects tampering", func(t *testing.T) {
		var seen []string
		cursor := ""
		for range 10 {
			params := url.Values{"limit": {"2"}, "q": {""}}
			if cursor != "" {
				params.Set("cursor", cursor)
			}
			code, res := alpha.search(ci["alpha"], params)
			if code != 200 {
				t.Fatalf("page: %d", code)
			}
			for _, h := range res.Items {
				seen = append(seen, h.Coordinate)
			}
			if !res.Page.HasMore {
				break
			}
			cursor = res.Page.NextCursor
		}
		if strings.Join(seen, ",") != "pkg-a,pkg-b,pkg-g,secret-b,shared" {
			t.Fatalf("paged coordinates: %v", seen)
		}
		_, first := alpha.search(ci["alpha"], url.Values{"limit": {"2"}})
		c := first.Page.NextCursor
		bad := c[:len(c)-3] + "AAA"
		if code := alpha.callAs(ci["alpha"], "GET", "/api/v1/federation/search?limit=2&cursor="+url.QueryEscape(bad), nil, nil); code != 400 {
			t.Fatalf("tampered cursor: %d", code)
		}
		// A cursor of another query or principal does not verify either.
		if code := alpha.callAs(ci["alpha"], "GET", "/api/v1/federation/search?limit=2&q=pkg&cursor="+url.QueryEscape(c), nil, nil); code != 400 {
			t.Fatalf("cursor replayed on another query: %d", code)
		}
		if code := beta.callAs(ci["beta"], "GET", "/api/v1/federation/search?limit=2&cursor="+url.QueryEscape(c), nil, nil); code != 400 {
			t.Fatalf("cursor replayed at another member: %d", code)
		}
	})

	t.Run("local principals cannot fan out", func(t *testing.T) {
		if code, c := alpha.errorCode("GET", "/api/v1/federation/search", nil); code != 403 || c != "not_federated" {
			t.Fatalf("local principal: %d %s", code, c)
		}
	})

	t.Run("a scoped token narrows results at every member", func(t *testing.T) {
		tok, err := (identity.Tokens{DB: alpha.pool}).Issue(context.Background(), alpha.id, alpha.principal("ci").ID, alpha.principal("ci").ID,
			[]identity.Scope{{Actions: []identity.Action{identity.Read}, Namespace: "public"}}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		_, res := alpha.search(tok.Secret, url.Values{"q": {"-"}, "limit": {"50"}})
		if res.coordinates() != "pkg-a,pkg-b,pkg-g" {
			t.Fatalf("token scope must follow the assertion: %s %+v", res.coordinates(), res.Members)
		}
	})

	t.Run("one member down reports partial results", func(t *testing.T) {
		gamma.proxy.set(faultDrop)
		defer gamma.proxy.set(faultPass)
		start := time.Now()
		code, res := alpha.search(ci["alpha"], url.Values{"q": {"p"}, "limit": {"50"}})
		if code != 200 || !res.Partial || res.member("gamma").Status != "error" || res.member("gamma").Code != "unreachable" || res.member("beta").Status != "ok" {
			t.Fatalf("down member: %d %+v", code, res.Members)
		}
		if res.coordinates() != "pkg-a,pkg-b" {
			t.Fatalf("surviving members still answer: %s", res.coordinates())
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("a down member must not stall the query: %v", time.Since(start))
		}
	})

	t.Run("a slow member times out without failing the query", func(t *testing.T) {
		beta.proxy.setDelay(3 * time.Second)
		defer beta.proxy.set(faultPass)
		start := time.Now()
		code, res := alpha.search(ci["alpha"], url.Values{"q": {"pk"}, "limit": {"50"}})
		took := time.Since(start)
		if code != 200 || !res.Partial || res.member("beta").Status != "timeout" || res.member("gamma").Status != "ok" {
			t.Fatalf("slow member: %d %+v", code, res.Members)
		}
		if took > 2500*time.Millisecond {
			t.Fatalf("the per-member deadline must bound the query: %v", took)
		}
		if res.coordinates() != "pkg-a,pkg-g" {
			t.Fatalf("results without the slow member: %s", res.coordinates())
		}
		var blackholed time.Duration
		beta.proxy.set(faultBlackhole)
		start = time.Now()
		_, res = alpha.search(ci["alpha"], url.Values{"q": {"pkg"}})
		blackholed = time.Since(start)
		if res.member("beta").Status != "timeout" || blackholed > 2500*time.Millisecond {
			t.Fatalf("blackholed member: %+v %v", res.Members, blackholed)
		}
	})

	t.Run("a member without the caller's identity is skipped", func(t *testing.T) {
		// The guard trigger protects federation-managed rows from the API; the scenario flips the flag underneath it.
		_, _ = gamma.pool.Exec(context.Background(), `ALTER TABLE principals DISABLE TRIGGER USER`)
		defer func() { _, _ = gamma.pool.Exec(context.Background(), `ALTER TABLE principals ENABLE TRIGGER USER`) }()
		if _, err := gamma.pool.Exec(context.Background(), `UPDATE principals SET disabled=true WHERE instance_id=$1 AND id=$2`, gamma.id, gci); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = gamma.pool.Exec(context.Background(), `UPDATE principals SET disabled=false WHERE instance_id=$1 AND id=$2`, gamma.id, gci)
		}()
		_, res := alpha.search(ci["alpha"], url.Values{"q": {"pkg-"}})
		if g := res.member("gamma"); g.Status != "skipped" || g.Code != "no_identity" || !res.Partial {
			t.Fatalf("gamma: %+v", res.Members)
		}
	})

	t.Run("results are cached per principal and query", func(t *testing.T) {
		q := url.Values{"q": {"secret"}}
		_, first := alpha.search(ci["alpha"], q)
		_, second := alpha.search(ci["alpha"], q)
		if first.member("beta").Cached || !second.member("beta").Cached || second.coordinates() != first.coordinates() {
			t.Fatalf("cache: %+v then %+v", first.Members, second.Members)
		}
	})

	t.Run("peer rate limits hold and a captured assertion cannot be reused", func(t *testing.T) {
		ctx := context.Background()
		body := []byte(`{"q":"","format":"","namespace":"","has_namespace":false,"digest":"","feed_key":"","after_feed_key":"","after_coordinate":"","limit":5}`)
		session := identity.Principal{InstanceID: beta.id, PrincipalID: beta.principal("ci").ID}
		limited, ok := 0, 0
		for range 80 {
			req, err := beta.svc.Assert(ctx, session, alpha.id, "POST", "/federation/v1/search", body)
			if err != nil {
				t.Fatal(err)
			}
			_, err = beta.svc.SendAsserted(ctx, req)
			switch peerCode(err) {
			case "":
				ok++
			case "rate_limited":
				limited++
			default:
				t.Fatalf("unexpected: %v", err)
			}
		}
		if limited == 0 || ok == 0 {
			t.Fatalf("rate limit: ok=%d limited=%d", ok, limited)
		}
		req, _ := beta.svc.Assert(ctx, session, alpha.id, "POST", "/federation/v1/search", body)
		req.Body = []byte(`{"q":"x","format":"","namespace":"","has_namespace":false,"digest":"","feed_key":"","after_feed_key":"","after_coordinate":"","limit":5}`)
		time.Sleep(time.Second) // refill the buckets so the binding check is reached
		if code := peerCode(sendAsserted(beta, req)); code != "assertion_request_mismatch" {
			t.Fatalf("assertion bound to another query: %q", code)
		}
	})
}

func TestFederatedSearchConcurrency(t *testing.T) {
	alpha, beta, gamma, feeds, ci := searchFederation(t)
	for i := range 6 {
		alpha.seedPackage(feeds["alpha"], "local", fmt.Sprintf("a-%d", i), "public", "")
		beta.seedPackage(feeds["beta"], "local", fmt.Sprintf("b-%d", i), "public", "")
		gamma.seedPackage(feeds["gamma"], "local", fmt.Sprintf("g-%d", i), "public", "")
	}
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, res := alpha.search(ci["alpha"], url.Values{"q": {fmt.Sprintf("-%d", i)}})
			if len(res.Items) != 3 || res.Partial {
				t.Errorf("concurrent search %d: %+v", i, res.Members)
			}
		}()
	}
	wg.Wait()
	_ = peer.HeaderAssertion
}
