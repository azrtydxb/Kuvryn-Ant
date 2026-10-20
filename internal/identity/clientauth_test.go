package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDirectory struct {
	calls       atomic.Int32
	unavailable atomic.Bool
}

func (d *fakeDirectory) VerifyPassword(_ context.Context, user string, pw []byte) (string, string, error) {
	d.calls.Add(1)
	if d.unavailable.Load() {
		return "", "", ErrUnavailable
	}
	switch {
	case user == "alice" && string(pw) == "correct horse":
		return "ldap-main", "uid-alice", nil
	case user == "bob" && string(pw) == "bobs password":
		return "ldap-main", "uid-bob", nil // authenticates, but has no binding
	}
	return "", "", ErrUnauthenticated
}

type authEnv struct {
	f       tokenFixture
	h       http.Handler
	ca      *ClientAuth
	dir     *fakeDirectory
	openID  string // anonymous_read feed
	roleID  string
	service string
}

// adapter mimics a package adapter: its own Bearer pipeline plus a real
// authorization check against the principal's persisted role grants.
func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	f := newTokenFixture(t)
	ctx := context.Background()
	e := &authEnv{f: f, dir: &fakeDirectory{}}
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format,anonymous_read) VALUES($1,'open','raw',true) RETURNING id", f.instance).Scan(&e.openID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, "INSERT INTO roles(instance_id,name) VALUES($1,'rw') RETURNING id", f.instance).Scan(&e.roleID); err != nil {
		t.Fatal(err)
	}
	for _, principal := range []string{f.actor, f.service} {
		must(f.pool.Exec(ctx, "INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)", f.instance, principal, e.roleID))
	}
	must(f.pool.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,$2,'artifact','allow',ARRAY['read','publish'])", f.instance, e.roleID))
	must(f.pool.Exec(ctx, "INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,'ldap-main','uid-alice',$2)", f.instance, f.actor))

	bearer := NewBearer(Tokens{DB: f.pool})
	adapter := func(feedFromPath func(*http.Request) string) http.Handler {
		return bearer.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := PrincipalFromContext(r.Context())
			policy, err := (Roles{DB: f.pool}).Resolve(r.Context(), p)
			if err != nil {
				w.WriteHeader(403)
				return
			}
			action := Read
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				action = Publish
			}
			feed := feedFromPath(r)
			if !policy.Authorize(p, Resource{InstanceID: f.instance, FeedID: feed, Kind: ArtifactResource}, action) {
				w.WriteHeader(403)
				return
			}
			w.Header().Set("X-Principal", p.PrincipalID)
			w.Header().Set("X-Anonymous", map[bool]string{true: "yes", false: "no"}[p.Anonymous])
			w.WriteHeader(204)
		}))
	}
	mux := http.NewServeMux()
	mux.Handle("/feeds/{slug}/raw/", adapter(func(r *http.Request) string {
		if r.PathValue("slug") == "open" {
			return e.openID
		}
		return f.feed
	}))
	// Cargo and NuGet adapters rewrite their raw credential locations into Bearer
	// before their pipeline, exactly as internal/cargo and internal/nuget do.
	rawAdapter := adapter(func(*http.Request) string { return f.feed })
	mux.Handle("/feeds/{slug}/cargo/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Values("Authorization"); len(v) == 1 && !strings.ContainsAny(v[0], " \t\r\n") {
			r.Header.Set("Authorization", "Bearer "+v[0])
		}
		rawAdapter.ServeHTTP(w, r)
	}))
	mux.Handle("/feeds/{slug}/nuget/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := r.Header.Values("X-NuGet-ApiKey"); len(k) == 1 && r.Method != http.MethodGet {
			r.Header.Set("Authorization", "Bearer "+k[0])
		}
		rawAdapter.ServeHTTP(w, r)
	}))
	mux.Handle("/v2/", adapter(func(*http.Request) string { return f.feed }))
	mux.Handle("/other", adapter(func(*http.Request) string { return f.feed }))
	e.ca = &ClientAuth{DB: f.pool, InstanceID: f.instance, Directory: e.dir, CacheTTL: -1}
	e.h = e.ca.Middleware(mux)
	return e
}

func (e *authEnv) do(method, path, remote string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}
func basic(user, pass string) map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))}
}
func bearerHeader(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

func (e *authEnv) setPolicy(t *testing.T, feed string, methods []string, groups ...string) {
	t.Helper()
	ctx := context.Background()
	if groups == nil {
		groups = []string{}
	}
	var err error
	if feed == "" {
		_, err = e.f.pool.Exec(ctx, "INSERT INTO client_auth_policies(instance_id,methods,anonymous_network_group_ids) VALUES($1,$2,$3::uuid[]) ON CONFLICT(instance_id) WHERE feed_id IS NULL DO UPDATE SET methods=excluded.methods,anonymous_network_group_ids=excluded.anonymous_network_group_ids", e.f.instance, methods, groups)
	} else {
		_, err = e.f.pool.Exec(ctx, "INSERT INTO client_auth_policies(instance_id,feed_id,methods,anonymous_network_group_ids) VALUES($1,$2,$3,$4::uuid[]) ON CONFLICT(instance_id,feed_id) WHERE feed_id IS NOT NULL DO UPDATE SET methods=excluded.methods,anonymous_network_group_ids=excluded.anonymous_network_group_ids", e.f.instance, feed, methods, groups)
	}
	if err != nil {
		t.Fatal(err)
	}
}
func (e *authEnv) group(t *testing.T, name string, cidrs ...string) string {
	t.Helper()
	var id string
	if err := e.f.pool.QueryRow(context.Background(), "INSERT INTO network_groups(instance_id,name,cidrs) VALUES($1,$2,$3) RETURNING id", e.f.instance, name, cidrs).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func (e *authEnv) pat(t *testing.T, actions ...Action) (PATRecord, string) {
	t.Helper()
	rec, secret, err := (PATs{DB: e.f.pool}).Create(context.Background(), e.f.instance, e.f.actor, "ci", []Scope{{Actions: actions}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return rec, secret
}

const lan = "10.1.2.3:4000"

func TestPolicyAllowsAndDeniesEachMethodPerFeed(t *testing.T) {
	e := newAuthEnv(t)
	service := e.f.issue(t)
	e.f.issue(t) // a second token for the revocation checks below
	if _, err := e.f.pool.Exec(context.Background(), "UPDATE service_tokens SET scopes='[{\"actions\":[\"read\",\"publish\"]}]'"); err != nil {
		t.Fatal(err)
	}
	_, pat := e.pat(t, Read)
	path := "/feeds/feed/raw/x"
	all := []string{"anonymous", "token", "basic-directory", "pat", "github-oidc"}
	creds := map[string]map[string]string{
		"token":           bearerHeader(service.Secret),
		"pat":             bearerHeader(pat),
		"basic-directory": basic("alice", "correct horse"),
	}
	e.setPolicy(t, e.f.feed, all)
	for name, h := range creds {
		if w := e.do("GET", path, lan, h); w.Code != 204 {
			t.Fatalf("%s denied under permissive policy: %d", name, w.Code)
		}
	}
	for allowed := range creds {
		e.setPolicy(t, e.f.feed, []string{allowed})
		for name, h := range creds {
			w := e.do("GET", path, lan, h)
			want := 204
			if name != allowed {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("policy [%s] with %s credential: got %d want %d", allowed, name, w.Code, want)
			}
		}
	}
	// A feed without its own policy inherits the instance default.
	e.setPolicy(t, e.f.feed, all)
	if _, err := e.f.pool.Exec(context.Background(), "DELETE FROM client_auth_policies WHERE feed_id IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
	e.setPolicy(t, "", []string{"pat"})
	if w := e.do("GET", path, lan, creds["pat"]); w.Code != 204 {
		t.Fatalf("pat under inherited default: %d", w.Code)
	}
	if w := e.do("GET", path, lan, creds["token"]); w.Code != 403 {
		t.Fatalf("token under inherited default: %d", w.Code)
	}
	// Other feeds are unaffected by a feed-level override.
	e.setPolicy(t, e.f.feed, all)
	e.setPolicy(t, e.openID, []string{"token"})
	if w := e.do("GET", "/feeds/open/raw/x", lan, creds["pat"]); w.Code != 403 {
		t.Fatalf("override missing on open feed: %d", w.Code)
	}
	if w := e.do("GET", path, lan, creds["pat"]); w.Code != 204 {
		t.Fatalf("override leaked to other feed: %d", w.Code)
	}
	// Unrelated routes are untouched by the middleware.
	if w := e.do("GET", "/other", lan, creds["pat"]); w.Code != 401 {
		t.Fatalf("non package route should keep its own rules: %d", w.Code)
	}
}

func TestAnonymousCIDRAndPublishNeverAnonymous(t *testing.T) {
	e := newAuthEnv(t)
	open := "/feeds/open/raw/x"
	// Default policy: anonymous works only where the feed already has anonymous_read.
	w := e.do("GET", open, "203.0.113.9:1", nil)
	if w.Code != 204 || w.Header().Get("X-Anonymous") != "yes" {
		t.Fatalf("anonymous read of anonymous_read feed: %d", w.Code)
	}
	if w := e.do("HEAD", open, "203.0.113.9:1", nil); w.Code != 204 {
		t.Fatalf("anonymous HEAD: %d", w.Code)
	}
	if w := e.do("GET", "/feeds/feed/raw/x", "203.0.113.9:1", nil); w.Code != 401 {
		t.Fatalf("anonymous read of private feed: %d", w.Code)
	}
	for _, method := range []string{"PUT", "POST", "DELETE", "PATCH"} {
		if w := e.do(method, open, "203.0.113.9:1", nil); w.Code != 401 {
			t.Fatalf("anonymous %s must never be accepted: %d", method, w.Code)
		}
	}
	// A policy without anonymous disables it even on an anonymous_read feed.
	e.setPolicy(t, e.openID, []string{"token", "pat"})
	if w := e.do("GET", open, "203.0.113.9:1", nil); w.Code != 401 {
		t.Fatalf("anonymous allowed despite policy: %d", w.Code)
	}
	// Restrict to a named network group.
	cluster := e.group(t, "cluster", "10.0.0.0/8", "fd00::/8")
	e.setPolicy(t, e.openID, []string{"anonymous", "token"}, cluster)
	for remote, want := range map[string]int{"10.1.2.3:1": 204, "[fd00::5]:1": 204, "192.168.1.1:1": 401, "11.0.0.1:1": 401} {
		if w := e.do("GET", open, remote, nil); w.Code != want {
			t.Fatalf("%s: got %d want %d", remote, w.Code, want)
		}
	}
	// X-Forwarded-For from an untrusted peer is ignored.
	if w := e.do("GET", open, "192.168.1.1:1", map[string]string{"X-Forwarded-For": "10.1.2.3"}); w.Code != 401 {
		t.Fatalf("spoofed XFF accepted: %d", w.Code)
	}
	// ...but honored when the TCP peer is a configured trusted proxy.
	e.ca.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
	if w := e.do("GET", open, "192.168.1.1:1", map[string]string{"X-Forwarded-For": "10.1.2.3"}); w.Code != 204 {
		t.Fatalf("trusted proxy XFF ignored: %d", w.Code)
	}
	if w := e.do("GET", open, "192.168.1.1:1", map[string]string{"X-Forwarded-For": "10.1.2.3, 198.51.100.7"}); w.Code != 401 {
		t.Fatalf("right-most untrusted hop must decide: %d", w.Code)
	}
	// A restriction whose group vanished fails closed instead of widening.
	if _, err := e.f.pool.Exec(context.Background(), "DELETE FROM network_groups"); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", open, "10.1.2.3:1", nil); w.Code != 401 {
		t.Fatalf("unresolvable network group widened access: %d", w.Code)
	}
	// An invalid credential never falls back to anonymous access.
	e.setPolicy(t, e.openID, []string{"anonymous", "token"})
	if w := e.do("GET", open, lan, bearerHeader("ant_"+strings.Repeat("A", 43))); w.Code != 401 {
		t.Fatalf("bad token fell back to anonymous: %d", w.Code)
	}
}

func TestBasicDirectoryAuthCacheAndLockout(t *testing.T) {
	e := newAuthEnv(t)
	e.setPolicy(t, e.f.feed, []string{"basic-directory"})
	path := "/feeds/feed/raw/x"
	w := e.do("GET", path, lan, basic("alice", "correct horse"))
	if w.Code != 204 || w.Header().Get("X-Principal") != e.f.actor {
		t.Fatalf("directory login: %d %s", w.Code, w.Header().Get("X-Principal"))
	}
	if e.dir.calls.Load() != 1 {
		t.Fatal("unexpected directory calls")
	}
	// Positive cache: no second directory round trip, and no password stored.
	if w = e.do("GET", path, lan, basic("alice", "correct horse")); w.Code != 204 || e.dir.calls.Load() != 1 {
		t.Fatalf("cache miss: %d calls=%d", w.Code, e.dir.calls.Load())
	}
	// A different password for the same user is not served from the cache.
	if w = e.do("GET", path, lan, basic("alice", "correct horse!")); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("wrong password: %d", w.Code)
	}
	// Directory success without a local binding grants nothing.
	if w = e.do("GET", path, lan, basic("bob", "bobs password")); w.Code != 401 {
		t.Fatalf("unbound directory user: %d", w.Code)
	}
	// PAT used as the Basic password with an arbitrary username.
	e.setPolicy(t, e.f.feed, []string{"pat", "basic-directory"})
	_, pat := e.pat(t, Read)
	if w = e.do("GET", path, lan, basic("whatever", pat)); w.Code != 204 || w.Header().Get("X-Principal") != e.f.actor {
		t.Fatalf("pat as basic password: %d", w.Code)
	}
	// Infrastructure faults are 503, not credential failures.
	e.dir.unavailable.Store(true)
	if w = e.do("GET", path, lan, basic("alice", "another")); w.Code != 503 {
		t.Fatalf("directory outage: %d", w.Code)
	}
	e.dir.unavailable.Store(false)
	// Lockout: repeated failures for one username block even the right password.
	for i := 0; i < 5; i++ {
		e.do("GET", path, "10.9.9."+string(rune('1'+i))+":1", basic("carol", "nope"))
	}
	if w = e.do("GET", path, "10.9.9.9:1", basic("carol", "nope")); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("username lockout missing: %d", w.Code)
	}
	for i := 0; i < 5; i++ {
		e.do("GET", path, "10.8.8.8:1", basic("alice", "bad"))
	}
	if w = e.do("GET", path, "10.7.7.7:1", basic("alice", "correct horse")); w.Code != 429 {
		t.Fatalf("locked user authenticated: %d", w.Code)
	}
	// Per-peer limit across many usernames.
	for i := 0; i < 30; i++ {
		e.do("GET", path, "10.6.6.6:1", basic("user"+string(rune('a'+i%26))+string(rune('a'+i/26)), "x"))
	}
	if w = e.do("GET", path, "10.6.6.6:1", basic("dave", "x")); w.Code != 429 {
		t.Fatalf("peer limit missing: %d", w.Code)
	}
}

func TestPATLifecycle(t *testing.T) {
	e := newAuthEnv(t)
	ctx := context.Background()
	pats := PATs{DB: e.f.pool}
	rec, secret := e.pat(t, Read)
	if !strings.HasPrefix(secret, "antp_") || len(secret) != patLength {
		t.Fatalf("token shape: %q", secret)
	}
	var stored []byte
	if err := e.f.pool.QueryRow(ctx, "SELECT verifier FROM personal_access_tokens WHERE id=$1", rec.ID).Scan(&stored); err != nil || strings.Contains(string(stored), secret) {
		t.Fatal("secret stored")
	}
	list, err := pats.List(ctx, e.f.instance, e.f.actor)
	if err != nil || len(list) != 1 || list[0].LastUsedAt != nil {
		t.Fatalf("list: %v %+v", err, list)
	}
	// Usable as Bearer; scopes narrow the owner's grants; last-used is recorded.
	path := "/feeds/feed/raw/x"
	if w := e.do("GET", path, lan, bearerHeader(secret)); w.Code != 204 {
		t.Fatalf("read with PAT: %d", w.Code)
	}
	if w := e.do("PUT", path, lan, bearerHeader(secret)); w.Code != 403 {
		t.Fatalf("read-only PAT published: %d", w.Code)
	}
	list, _ = pats.List(ctx, e.f.instance, e.f.actor)
	if list[0].LastUsedAt == nil {
		t.Fatal("last used not recorded")
	}
	// The secret cannot be recovered from the listing.
	if strings.Contains(list[0].Name+list[0].ID, secret) {
		t.Fatal("secret in list")
	}
	_, publisher := e.pat(t, Read, Publish)
	if w := e.do("PUT", path, lan, bearerHeader(publisher)); w.Code != 204 {
		t.Fatalf("publish PAT: %d", w.Code)
	}
	// A user cannot revoke someone else's token; service principals cannot own one.
	if err = pats.Revoke(ctx, e.f.instance, e.f.service, rec.ID); !errors.Is(err, ErrPATNotFound) {
		t.Fatalf("cross-user revoke: %v", err)
	}
	if _, _, err = pats.Create(ctx, e.f.instance, e.f.service, "x", []Scope{{Actions: []Action{Read}}}, time.Now().Add(time.Hour)); !errors.Is(err, ErrPATNotUser) {
		t.Fatalf("service PAT: %v", err)
	}
	for _, bad := range [][]Scope{nil, {{Actions: []Action{Admin}}}, {{Actions: []Action{Configure}}}, {{FeedID: e.f.foreignFeed, Actions: []Action{Read}}}} {
		if _, _, err = pats.Create(ctx, e.f.instance, e.f.actor, "x", bad, time.Now().Add(time.Hour)); !errors.Is(err, ErrPATInvalid) {
			t.Fatalf("invalid scopes accepted: %v %v", bad, err)
		}
	}
	if _, _, err = pats.Create(ctx, e.f.instance, e.f.actor, "x", []Scope{{Actions: []Action{Read}}}, time.Now().Add(400*24*time.Hour)); !errors.Is(err, ErrPATInvalid) {
		t.Fatalf("over-long expiry accepted: %v", err)
	}
	// Roles.Resolve rechecks the token, so revocation also stops in-flight work.
	p, err := pats.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (Roles{DB: e.f.pool}).Resolve(ctx, p); err != nil {
		t.Fatalf("active PAT resolve: %v", err)
	}
	if err = pats.Revoke(ctx, e.f.instance, e.f.actor, rec.ID); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", path, lan, bearerHeader(secret)); w.Code != 401 {
		t.Fatalf("revoked PAT accepted: %d", w.Code)
	}
	if _, err = (Roles{DB: e.f.pool}).Resolve(ctx, p); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked PAT resolved: %v", err)
	}
	// Expired and owner-disabled tokens stop working.
	if _, err = e.f.pool.Exec(ctx, "UPDATE personal_access_tokens SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if w := e.do("PUT", path, lan, bearerHeader(publisher)); w.Code != 401 {
		t.Fatalf("expired PAT accepted: %d", w.Code)
	}
	_, live := e.pat(t, Read)
	if _, err = e.f.pool.Exec(ctx, "UPDATE principals SET disabled=true WHERE id=$1", e.f.actor); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", path, lan, bearerHeader(live)); w.Code != 401 {
		t.Fatalf("PAT of disabled user accepted: %d", w.Code)
	}
	// A tampered token fails.
	if w := e.do("GET", path, lan, bearerHeader(live[:len(live)-1]+"A")); w.Code != 401 {
		t.Fatalf("tampered PAT accepted: %d", w.Code)
	}
	// The cap is enforced.
	if _, err = e.f.pool.Exec(ctx, "UPDATE principals SET disabled=false WHERE id=$1", e.f.actor); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxActivePATs; i++ {
		if _, _, err = pats.Create(ctx, e.f.instance, e.f.actor, "bulk", []Scope{{Actions: []Action{Read}}}, time.Now().Add(time.Hour)); err != nil {
			break
		}
	}
	if _, _, err = pats.Create(ctx, e.f.instance, e.f.actor, "over", []Scope{{Actions: []Action{Read}}}, time.Now().Add(time.Hour)); !errors.Is(err, ErrPATLimit) {
		t.Fatalf("cap not enforced: %v", err)
	}
}

func TestTicketsAreSingleUseAndNotGuessable(t *testing.T) {
	p := Principal{InstanceID: "i", PrincipalID: "p"}
	raw := mintTicket(p)
	if len(raw) != 47 {
		t.Fatal("ticket shape")
	}
	if got, ok := takeTicket(raw); !ok || !got.ticket || got.PrincipalID != "p" {
		t.Fatal("ticket not redeemed")
	}
	if _, ok := takeTicket(raw); ok {
		t.Fatal("ticket replayed")
	}
	if _, ok := takeTicket("ant_" + strings.Repeat("A", 43)); ok {
		t.Fatal("forged ticket accepted")
	}
}

func TestParseCredential(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cases := []struct {
		header, user, secret string
		basic, ok            bool
	}{
		{"Bearer abc", "", "abc", false, true},
		{"bearer abc", "", "abc", false, true},
		{"Basic " + enc("u:p:q"), "u", "p:q", true, true},
		{"Basic " + enc("u:"), "", "", false, false},
		{"Basic " + enc(":p"), "", "", false, false},
		{"Basic !!!", "", "", false, false},
		{"Bearer a b", "", "", false, false},
		{"Digest x", "", "", false, false},
		{"Bearer", "", "", false, false},
	}
	for _, c := range cases {
		u, s, b, ok := parseCredential(c.header)
		if u != c.user || s != c.secret || b != c.basic || ok != c.ok {
			t.Errorf("%q: got %q %q %v %v", c.header, u, s, b, ok)
		}
	}
}
