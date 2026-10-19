package management

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func (f *sourceHTTPFixture) adminCredential() string {
	f.t.Helper()
	token, err := (identity.Tokens{DB: f.db}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Configure, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		f.t.Fatal(err)
	}
	return token.Secret
}

func decodeFeed(t *testing.T, body []byte) Feed {
	t.Helper()
	var feed Feed
	if err := json.Unmarshal(body, &feed); err != nil {
		t.Fatal(err)
	}
	return feed
}

func TestCacheFeedPurposeValidation(t *testing.T) {
	f := newSourceHTTPFixture(t)
	create := func(key, body string, status int) Feed {
		t.Helper()
		w := f.request("POST", "/api/v1/feeds", body, key, "")
		expectSourceStatus(t, w, status)
		if status != 201 {
			return Feed{}
		}
		return decodeFeed(t, w.Body.Bytes())
	}
	// Default purpose, no cache policy.
	plain := create("plain", `{"slug":"plain","format":"oci"}`, 201)
	if plain.Purpose != "artifacts" || plain.CachePolicy != nil {
		t.Fatalf("default feed %+v", plain)
	}
	// Cache feeds: oci and raw only, with the default seven-day idle time.
	for _, format := range []string{"oci", "raw"} {
		feed := create("cache-"+format, fmt.Sprintf(`{"slug":"cache-%s","format":%q,"purpose":"cache"}`, format, format), 201)
		if feed.Purpose != "cache" || feed.CachePolicy == nil || feed.CachePolicy.MaxIdleSeconds != 7*86400 || feed.CachePolicy.MaxBytes != nil {
			t.Fatalf("%s cache feed %+v", format, feed)
		}
	}
	sized := create("sized", `{"slug":"sized","format":"raw","purpose":"cache","cache_policy":{"max_idle_seconds":3600,"max_bytes":1048576}}`, 201)
	if sized.CachePolicy == nil || sized.CachePolicy.MaxIdleSeconds != 3600 || sized.CachePolicy.MaxBytes == nil || *sized.CachePolicy.MaxBytes != 1048576 {
		t.Fatalf("sized cache feed %+v", sized)
	}
	for name, body := range map[string]string{
		"npm":              `{"slug":"npmc","format":"npm","purpose":"cache"}`,
		"maven":            `{"slug":"mvnc","format":"maven","purpose":"cache"}`,
		"unknown purpose":  `{"slug":"weird","format":"oci","purpose":"scratch"}`,
		"policy on plain":  `{"slug":"pol","format":"oci","cache_policy":{"max_idle_seconds":3600}}`,
		"idle too small":   `{"slug":"idle","format":"oci","purpose":"cache","cache_policy":{"max_idle_seconds":5}}`,
		"negative bytes":   `{"slug":"neg","format":"oci","purpose":"cache","cache_policy":{"max_bytes":-1}}`,
		"unknown property": `{"slug":"unk","format":"oci","purpose":"cache","cache_policy":{"max_age":1}}`,
	} {
		create("bad-"+name, body, 400)
	}
	// Stored and reported identically by get and by the purpose filter of list.
	got := f.request("GET", "/api/v1/feeds/"+sized.ID, "", "", "")
	expectSourceStatus(t, got, 200)
	if again := decodeFeed(t, got.Body.Bytes()); again.Purpose != "cache" || again.CachePolicy == nil || again.CachePolicy.MaxIdleSeconds != 3600 {
		t.Fatalf("get %+v", again)
	}
	list := func(query string) []Feed {
		w := f.request("GET", "/api/v1/feeds"+query, "", "", "")
		expectSourceStatus(t, w, 200)
		var page FeedPage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	if n := len(list("")); n != 4 {
		t.Fatalf("%d feeds listed, want 4", n)
	}
	if items := list("?purpose=cache"); len(items) != 3 {
		t.Fatalf("cache filter lists %d feeds, want 3", len(items))
	}
	if items := list("?purpose=artifacts"); len(items) != 1 || items[0].Slug != "plain" {
		t.Fatalf("artifacts filter lists %+v", items)
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/feeds?purpose=bogus", "", "", ""), 400)
}

func TestCacheFeedPurposeChange(t *testing.T) {
	f := newSourceHTTPFixture(t)
	admin := f.adminCredential()
	feed := f.feed("builds", "raw")
	local := f.source("builds-local", "local", "raw")
	attach := func(f2 Feed, s Source, status int) {
		t.Helper()
		w := f.request("PUT", "/api/v1/feeds/"+f2.ID+"/sources/"+s.ID, `{"priority":0}`, "attach-"+f2.Slug+s.Name, fmt.Sprintf(`"%d"`, f.currentRevision(f2.ID)))
		expectSourceStatus(t, w, status)
	}
	attach(feed, local, 200)

	patch := func(credential, body string, status int) Feed {
		t.Helper()
		w := f.requestAs("PATCH", "/api/v1/feeds/"+feed.ID, body, fmt.Sprintf("patch-%d-%x", time.Now().UnixNano(), len(body)), fmt.Sprintf(`"%d"`, f.currentRevision(feed.ID)), credential)
		expectSourceStatus(t, w, status)
		if status != 200 {
			return Feed{}
		}
		return decodeFeed(t, w.Body.Bytes())
	}
	// Changing the purpose needs feed administration, not just configure.
	patch(f.credential, `{"purpose":"cache"}`, 403)
	// A source another feed uses cannot become cache content.
	other := f.feed("releases", "raw")
	attach(other, local, 200)
	patch(admin, `{"purpose":"cache"}`, 409)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/feeds/"+other.ID+"/sources/"+local.ID, "", "detach-other", fmt.Sprintf(`"%d"`, f.currentRevision(other.ID))), 200)

	cache := patch(admin, `{"purpose":"cache","cache_policy":{"max_idle_seconds":7200,"max_bytes":5000000}}`, 200)
	if cache.Purpose != "cache" || cache.CachePolicy == nil || cache.CachePolicy.MaxIdleSeconds != 7200 || *cache.CachePolicy.MaxBytes != 5000000 {
		t.Fatalf("converted feed %+v", cache)
	}
	// Audited, with the old and new purpose.
	if f.count("SELECT count(*) FROM audit_events WHERE action='feed.purpose.change' AND resource_id=$1 AND details->>'from'='artifacts' AND details->>'to'='cache'", feed.ID) != 1 {
		t.Fatal("purpose change was not audited")
	}
	// A cache feed takes no second feed's source and no non-local source.
	remote := f.source("upstream", "remote", "raw")
	attach(feed, remote, 409)
	releases2 := f.feed("releases2", "raw")
	attach(releases2, local, 409)
	// Policy-only change keeps the purpose and is audited too; max_bytes 0 removes the limit.
	again := patch(admin, `{"cache_policy":{"max_bytes":0}}`, 200)
	if again.Purpose != "cache" || again.CachePolicy == nil || again.CachePolicy.MaxBytes != nil || again.CachePolicy.MaxIdleSeconds != 7200 {
		t.Fatalf("policy change %+v", again)
	}
	patch(admin, `{"cache_policy":{"max_idle_seconds":10}}`, 400)
	// Back to artifacts: the policy disappears from the representation.
	back := patch(admin, `{"purpose":"artifacts"}`, 200)
	if back.Purpose != "artifacts" || back.CachePolicy != nil {
		t.Fatalf("converted back %+v", back)
	}
	patch(admin, `{"cache_policy":{"max_idle_seconds":3600}}`, 400)
	// Only oci and raw feeds can be caches.
	npm := f.feed("npmfeed", "npm")
	w := f.requestAs("PATCH", "/api/v1/feeds/"+npm.ID, `{"purpose":"cache"}`, "npm-cache", fmt.Sprintf(`"%d"`, npm.Revision), admin)
	expectSourceStatus(t, w, 400)
}

func (f *sourceHTTPFixture) currentRevision(feed string) int64 {
	f.t.Helper()
	var revision int64
	if err := f.db.QueryRow(context.Background(), "SELECT revision FROM feeds WHERE id=$1", feed).Scan(&revision); err != nil {
		f.t.Fatal(err)
	}
	return revision
}

// Cache feeds are excluded from the package search unless asked for, and cache
// change history is not recorded for replication.
func TestCacheFeedsAreHiddenFromPackageSearch(t *testing.T) {
	f := newSourceHTTPFixture(t)
	ctx := context.Background()
	art := f.feed("artifacts-feed", "raw")
	cache := f.feed("cache-feed", "raw")
	srcA, srcC := f.source("a-local", "local", "raw"), f.source("c-local", "local", "raw")
	for _, c := range []struct {
		feed Feed
		src  Source
	}{{art, srcA}, {cache, srcC}} {
		if _, err := f.db.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, f.instance, c.feed.ID, c.src.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,'')`, f.instance, c.src.ID, "pkg-"+c.feed.Slug); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(ctx, `UPDATE feeds SET purpose='cache' WHERE id=$1`, cache.ID); err != nil {
		t.Fatal(err)
	}
	search := func(query string) (slugs []string) {
		w := f.request("GET", "/api/v1/packages"+query, "", "", "")
		expectSourceStatus(t, w, 200)
		var page PackageSearchPage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			slugs = append(slugs, item.FeedSlug)
		}
		return
	}
	if got := search(""); len(got) != 1 || got[0] != "artifacts-feed" {
		t.Fatalf("default search %v, want only the artifacts feed", got)
	}
	if got := search("?purpose=cache"); len(got) != 1 || got[0] != "cache-feed" {
		t.Fatalf("purpose=cache search %v", got)
	}
	if got := search("?purpose=all"); len(got) != 2 {
		t.Fatalf("purpose=all search %v", got)
	}
	if got := search("?feed=" + cache.ID); len(got) != 1 {
		t.Fatalf("addressing the cache feed directly finds %v", got)
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/packages?purpose=bogus", "", "", ""), 400)
}

// Retention policies do not apply to cache feeds (their cache policy does), and
// the cleanup endpoint validates its input before touching the artifact core.
func TestCacheFeedRetentionPolicyAndCleanupInput(t *testing.T) {
	o := newOpsFixture(t)
	cache := decodeFeed(t, mustBody(t, o.request("POST", "/api/v1/feeds", `{"slug":"bc","format":"oci","purpose":"cache"}`, "bc", ""), 201))
	plain := o.feed("plain-feed", "oci")
	expectSourceStatus(t, o.request("POST", "/api/v1/retention/policies", fmt.Sprintf(`{"feed_id":%q,"keep_last":3}`, cache.ID), "rp-cache", ""), 409)
	expectSourceStatus(t, o.request("POST", "/api/v1/retention/policies", fmt.Sprintf(`{"feed_id":%q,"keep_last":3}`, plain.ID), "rp-plain", ""), 201)
	if o.count(`SELECT count(*) FROM retention_policies`) != 1 {
		t.Fatal("a retention policy was created for a cache feed")
	}
	// A source that only a cache feed uses is refused as well.
	src := o.source("bc-local", "local", "oci")
	expectSourceStatus(t, o.request("PUT", "/api/v1/feeds/"+cache.ID+"/sources/"+src.ID, `{"priority":0}`, "attach-bc", fmt.Sprintf(`"%d"`, o.currentRevision(cache.ID))), 200)
	expectSourceStatus(t, o.request("POST", "/api/v1/retention/policies", fmt.Sprintf(`{"source_id":%q,"max_idle_seconds":60}`, src.ID), "rp-src", ""), 409)

	clean := func(query string) *httptest.ResponseRecorder {
		return o.request("DELETE", "/api/v1/feeds/"+cache.ID+"/cache"+query, "", "", "")
	}
	expectSourceStatus(t, clean(""), 400)                   // prefix is required
	expectSourceStatus(t, clean("?prefix="), 400)           // and non-empty
	expectSourceStatus(t, clean("?prefix=a&prefix=b"), 400) // and single
	expectSourceStatus(t, clean("?prefix=a&other=1"), 400)  // no unknown parameters
	expectSourceStatus(t, o.request("DELETE", "/api/v1/feeds/not-a-uuid/cache?prefix=a", "", "", ""), 400)
	// Authorized and well-formed, but this fixture serves no artifact core.
	expectSourceStatus(t, clean("?prefix=app:main-"), 503)
}

func mustBody(t *testing.T, w *httptest.ResponseRecorder, status int) []byte {
	t.Helper()
	expectSourceStatus(t, w, status)
	return w.Body.Bytes()
}

// DELETE /api/v1/feeds/{id}/cache end to end with a scoped CI token: delete on
// the cache feed is enough, publish is not, other feeds are out of reach, and
// a feed that is not a cache feed is refused.
func TestCacheCleanupEndpointAuthorization(t *testing.T) {
	o := newOpsFixture(t)
	ctx := context.Background()
	cache := o.feed("buildcache", "oci")
	art := o.feed("releases", "oci")
	for _, f := range []Feed{cache, art} {
		src := o.source(f.Slug+"-local", "local", "oci")
		if _, err := o.db.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, o.instance, f.ID, src.ID); err != nil {
			t.Fatal(err)
		}
		var component string
		if err := o.db.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,'org/app','org') RETURNING id::text`, o.instance, src.ID).Scan(&component); err != nil {
			t.Fatal(err)
		}
		for _, tag := range []string{"main-amd64", "feat-amd64"} {
			var version string
			if err := o.db.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) RETURNING id::text`, o.instance, component, "sha-"+tag).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if _, err := o.db.Exec(ctx, `INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3)`, component, tag, version); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := o.db.Exec(ctx, `UPDATE feeds SET purpose='cache' WHERE id=$1`, cache.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := ownership.Acquire(ctx, o.db, o.instance, "00000000-0000-4000-8000-0000000000aa", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	o.deps.Artifacts = func(string) *artifact.Service {
		return &artifact.Service{DB: o.db, Lease: func() ownership.Lease { return lease }}
	}
	mux := http.NewServeMux()
	RegisterFeeds(mux, o.db, o.deps.Codec)
	RegisterOperations(mux, o.deps)
	o.handler = WithRequestID(mux)

	token := func(feed string, actions ...identity.Action) string {
		t.Helper()
		tok, err := (identity.Tokens{DB: o.db}).Issue(ctx, o.instance, o.principal, o.principal, []identity.Scope{{FeedID: feed, Actions: actions}}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return tok.Secret
	}
	call := func(credential, feed, query string) *httptest.ResponseRecorder {
		return o.requestAs("DELETE", "/api/v1/feeds/"+feed+"/cache"+query, "", "", "", credential)
	}
	ci := token(cache.ID, identity.Read, identity.Publish, identity.Delete)
	expectSourceStatus(t, call("", cache.ID, "?prefix=org/app:feat-"), 401)
	expectSourceStatus(t, call(token(cache.ID, identity.Read, identity.Publish), cache.ID, "?prefix=org/app:feat-"), 403) // no delete
	expectSourceStatus(t, call(token(art.ID, identity.Read, identity.Delete), cache.ID, "?prefix=org/app:feat-"), 404)    // other feed
	expectSourceStatus(t, call(token(art.ID, identity.Read, identity.Delete), art.ID, "?prefix=org/app:feat-"), 409)      // not a cache feed
	if o.count(`SELECT count(*) FROM tags`) != 4 {
		t.Fatal("a refused call removed tags")
	}
	w := call(ci, cache.ID, "?prefix=org/app:feat-")
	expectSourceStatus(t, w, 200)
	res := decodeJSON[CacheCleanResponse](t, w)
	if res.TagsRemoved != 1 || res.VersionsRemoved != 1 || res.Matched != 1 || res.FeedID != cache.ID || res.Truncated {
		t.Fatalf("result %+v", res)
	}
	if o.count(`SELECT count(*) FROM tags`) != 3 {
		t.Fatal("wrong tag count after cleanup")
	}
	// Idempotent; the other feed's identical repository is untouched.
	again := decodeJSON[CacheCleanResponse](t, call(ci, cache.ID, "?prefix=org/app:feat-"))
	if again.TagsRemoved != 0 || again.Matched != 0 {
		t.Fatalf("repeat %+v", again)
	}
	if o.count(`SELECT count(*) FROM audit_events WHERE action='cache.clean'`) != 2 {
		t.Fatal("cleanup calls were not audited")
	}
}
