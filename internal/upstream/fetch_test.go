package upstream

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTLSPrivateCAAndOriginCredentials(t *testing.T) {
	var stolen atomic.Bool
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			stolen.Store(true)
		}
		io.WriteString(w, "safe")
	}))
	defer second.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing configured credential")
		}
		http.Redirect(w, r, second.URL+"/asset", 302)
	}))
	defer first.Close()
	roots := x509.NewCertPool()
	roots.AddCert(first.Certificate())
	roots.AddCert(second.Certificate())
	f, err := New(Config{Origin: first.URL, RedirectOrigins: []string{second.URL}, Roots: roots, AllowPrivate: true, Credentials: http.Header{"Authorization": []string{"Bearer secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	response, err := f.Fetch(context.Background(), "GET", "/package", http.Header{"Authorization": []string{"attacker"}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "safe" || stolen.Load() {
		t.Fatal("redirect credential leak")
	}
	denied, _ := New(Config{Origin: first.URL, Roots: roots})
	defer denied.Close()
	if _, err = denied.Fetch(context.Background(), "GET", "/", nil); !errors.Is(err, ErrPolicy) {
		t.Fatal(err)
	}
	untrusted, _ := New(Config{Origin: first.URL, AllowPrivate: true, Sleep: func(context.Context, time.Duration) error { return nil }})
	defer untrusted.Close()
	if _, err = untrusted.Fetch(context.Background(), "GET", "/", nil); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatal("TLS verification bypass", err)
	}
}
func TestRedirectPolicyAndRetriesCircuit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/credentials", 302)
			return
		}
		w.WriteHeader(503)
	}))
	defer server.Close()
	now := time.Unix(1, 0)
	f, e := New(Config{Origin: server.URL, AllowPrivate: true, AllowHTTP: true, FailureThreshold: 1, Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil }})
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e = f.Fetch(context.Background(), "POST", "/", nil); !errors.Is(e, ErrPolicy) {
		t.Fatal(e)
	}
	if _, e = f.Fetch(context.Background(), "GET", "/redirect", nil); !errors.Is(e, ErrPolicy) || calls.Load() != 1 {
		t.Fatal(e, calls.Load())
	}
	calls.Store(0)
	if _, e = f.Fetch(context.Background(), "GET", "/", nil); !errors.Is(e, ErrUnavailable) || calls.Load() != 3 {
		t.Fatal(e, calls.Load())
	}
	if _, e = f.Fetch(context.Background(), "GET", "/", nil); !errors.Is(e, ErrCircuitOpen) || calls.Load() != 3 {
		t.Fatal(e, calls.Load())
	}
	now = now.Add(time.Minute)
	if _, e = f.Fetch(context.Background(), "HEAD", "/", nil); !errors.Is(e, ErrUnavailable) || calls.Load() != 6 {
		t.Fatal(e, calls.Load())
	}
}
func TestDNSPolicyEveryRequestAndAddress(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.1", "::ffff:127.0.0.1", "2002:7f00:1::", "100.64.0.1"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Fatal(ip)
		}
	}
	f, e := New(Config{Origin: "https://registry.example", Resolver: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("127.0.0.1")}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e = f.Fetch(context.Background(), "GET", "/", nil); !errors.Is(e, ErrPolicy) {
		t.Fatal(e)
	}
}
func TestCancellationStopsRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	f, _ := New(Config{Origin: server.URL, AllowPrivate: true, AllowHTTP: true, Sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }})
	defer f.Close()
	if _, e := f.Fetch(ctx, "GET", "/", nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestFetchSendsUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("User-Agent") }))
	defer srv.Close()
	f, err := New(Config{Origin: srv.URL, AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Fetch(context.Background(), http.MethodGet, srv.URL+"/x", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != UserAgent {
		t.Fatalf("user agent = %q", got)
	}
}
