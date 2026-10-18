package denyaudit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/testdb"
)

func TestSurface(t *testing.T) {
	for path, want := range map[string]string{"/api/v1/feeds": "management", "/mcp": "mcp", "/v2/x/blobs/uploads/": "package", "/npm/left-pad": "package", "/auth/session": "", "/console/": "", "/": ""} {
		if got := Surface(path); got != want {
			t.Fatal(path, got, want)
		}
	}
}

func do(h http.Handler, method, path, remote string) {
	r := httptest.NewRequest(method, path+"?token=secret", nil)
	r.RemoteAddr = remote
	h.ServeHTTP(httptest.NewRecorder(), r)
}

func TestDeniedEventsWrittenAndRateLimited(t *testing.T) {
	f := testdb.New(t)
	var principal string
	if err := f.Pool.QueryRow(context.Background(), `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','svc') RETURNING id::text`, f.InstanceID).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rec := &Recorder{DB: f.Pool, InstanceID: f.InstanceID, GlobalLimit: 5, Now: func() time.Time { return now }}
	h := rec.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/forbidden":
			identity.NoteAuthenticated(r.Context(), identity.Principal{InstanceID: f.InstanceID, PrincipalID: principal, TokenID: "tok"})
			w.WriteHeader(http.StatusForbidden)
		case "/ok":
			w.WriteHeader(200)
		case "/v2/":
			w.WriteHeader(401)
		default:
			w.WriteHeader(401)
		}
	}))
	count := func(where string, args ...any) int {
		rec.Wait()
		return f.Count(t, `SELECT count(*) FROM audit_events WHERE instance_id=$1 AND result='denied' AND `+where, append([]any{f.InstanceID}, args...)...)
	}
	// Management 403 as an authenticated principal.
	do(h, "GET", "/api/v1/forbidden", "198.51.100.7:1111")
	if count(`action='auth.denied' AND principal_id=$2::uuid AND details->>'surface'='management' AND details->>'reason'='forbidden'`, principal) != 1 {
		t.Fatal("management denial not recorded with principal")
	}
	// Package protocol and MCP 401s; query strings never recorded.
	do(h, "GET", "/npm/pkg", "198.51.100.8:1")
	do(h, "POST", "/mcp", "198.51.100.9:1")
	if count(`details->>'surface'='package'`) != 1 || count(`details->>'surface'='mcp'`) != 1 {
		t.Fatal("package/mcp denials not recorded")
	}
	if count(`details::text LIKE '%secret%'`) != 0 {
		t.Fatal("query string leaked into audit details")
	}
	// Successes, registry challenge probes and unaudited surfaces are ignored.
	before := count(`true`)
	do(h, "GET", "/ok", "198.51.100.10:1")
	do(h, "GET", "/v2/", "198.51.100.10:1")
	do(h, "GET", "/auth/x", "198.51.100.10:1")
	if count(`true`) != before {
		t.Fatal("non-denials recorded")
	}
	// Flood from one peer: a single event, the rest suppressed and counted.
	for i := 0; i < 200; i++ {
		do(h, "GET", "/npm/pkg", "203.0.113.5:1")
	}
	if n := count(`details->>'peer'='203.0.113.5'`); n != 1 {
		t.Fatal("flood from one peer produced", n)
	}
	now = now.Add(61 * time.Second)
	do(h, "GET", "/npm/pkg", "203.0.113.5:1")
	if count(`details->>'peer'='203.0.113.5' AND (details->>'suppressed_since_last')::int=199`) != 1 {
		t.Fatal("suppressed count not carried to next window event")
	}
	// Many distinct peers hit the global cap; one flood marker is written.
	for i := 0; i < 50; i++ {
		do(h, "GET", "/pypi/x", "192.0.2."+string(rune('0'+i%10))+string(rune('0'+i/10))+":1")
	}
	rec.Wait()
	if n := f.Count(t, `SELECT count(*) FROM audit_events WHERE instance_id=$1 AND action=$2`, f.InstanceID, FloodAction); n != 1 {
		t.Fatal("flood marker count", n)
	}
	if n := count(`created_at>now()-interval '1 hour'`); n > 5+5+4 {
		t.Fatal("global cap not applied", n)
	}
}
