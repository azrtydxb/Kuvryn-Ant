package peer

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Unauthenticated peer routes (join) are rate-limited per remote IP.
func TestPublicRoutesRateLimitedPerIP(t *testing.T) {
	trust := &fakeTrust{members: map[string]Member{}, revoked: map[string]bool{}, now: time.Unix(1_800_000_000, 0)}
	served := 0
	h := &Handler{Trust: trust, Public: map[string]bool{"POST /federation/v1/join": true}, Mux: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ })}
	join := func(ip string) int {
		r := httptest.NewRequest("POST", "/federation/v1/join", nil)
		r.TLS = &tls.ConnectionState{}
		r.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	limited := 0
	for range 3 * DefaultPublicBurst {
		if join("203.0.113.7") == http.StatusTooManyRequests {
			limited++
		}
	}
	if served != DefaultPublicBurst || limited != 2*DefaultPublicBurst {
		t.Fatalf("burst from one IP: served %d, limited %d", served, limited)
	}
	if join("203.0.113.8") == http.StatusTooManyRequests {
		t.Fatal("another IP was limited")
	}
	trust.now = trust.now.Add(time.Duration(float64(time.Second) / DefaultPublicRate))
	if join("203.0.113.7") == http.StatusTooManyRequests {
		t.Fatal("the limit did not refill")
	}
}
