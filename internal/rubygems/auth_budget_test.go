package rubygems

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Basic-challenge clients (the first request carries no credentials) must not
// have those requests counted as failures, and a client that keeps sending a
// wrong password must not lock out a valid user behind the same address.
func TestBasicChallengeFlowIsNotThrottled(t *testing.T) {
	wrong := "ant_" + strings.Repeat("x", 43)
	f := setup(t)
	h := (&identity.ClientAuth{DB: f.pool, InstanceID: f.instance, CacheTTL: -1}).Middleware(f.registry)
	do := func(user, password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", testBase+"versions", nil)
		req.RemoteAddr = "10.0.0.1:4000" // the shared ingress address
		if user != "" {
			req.SetBasicAuth(user, password)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for i := range 60 {
		w := do("", "")
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("challenge %d: %d %v", i, w.Code, w.Header())
		}
		if w := do("good", f.token); w.Code != 200 {
			t.Fatalf("valid token after challenge %d: %d", i, w.Code)
		}
	}
	for i := range 15 {
		if w := do("bad", wrong); w.Code != 401 && w.Code != 429 {
			t.Fatalf("wrong password %d: %d", i, w.Code)
		}
	}
	if w := do("bad", wrong); w.Code != 429 {
		t.Fatalf("wrong-password client should be throttled: %d", w.Code)
	}
	if w := do("good", f.token); w.Code != 200 {
		t.Fatalf("valid user locked out by another user's failures: %d", w.Code)
	}
}
