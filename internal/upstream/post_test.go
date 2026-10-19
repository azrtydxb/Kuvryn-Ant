package upstream

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostIsBoundedQueryOnly(t *testing.T) {
	var gotBody, gotCT, gotAuth string
	var other int
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other++ }))
	defer foreign.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/query":
			b, _ := io.ReadAll(r.Body)
			gotBody, gotCT, gotAuth = string(b), r.Header.Get("Content-Type"), r.Header.Get("Authorization")
			if r.Header.Get("X-Evil") != "" {
				t.Error("unlisted header forwarded")
			}
			_, _ = io.WriteString(w, "answer")
		case "/moved":
			http.Redirect(w, r, "/query", http.StatusTemporaryRedirect)
		case "/away":
			http.Redirect(w, r, foreign.URL+"/x", http.StatusTemporaryRedirect)
		case "/see-other":
			http.Redirect(w, r, "/query", http.StatusSeeOther)
		}
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	roots.AddCert(foreign.Certificate())
	f, err := New(Config{Origin: server.URL, Roots: roots, AllowPrivate: true, Credentials: http.Header{"Authorization": []string{"Bearer secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := http.Header{"Content-Type": []string{"application/json"}, "X-Evil": []string{"1"}}
	for _, path := range []string{"/query", "/moved"} {
		resp, err := f.Post(context.Background(), path, h, []byte(`{"a":1}`))
		if err != nil {
			t.Fatal(path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "answer" || gotBody != `{"a":1}` || gotCT != "application/json" || gotAuth != "Bearer secret" {
			t.Fatalf("%s: %q body=%q ct=%q auth=%q", path, b, gotBody, gotCT, gotAuth)
		}
	}
	for _, path := range []string{"/away", "/see-other"} {
		if _, err = f.Post(context.Background(), path, h, []byte("x")); !errors.Is(err, ErrPolicy) {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if other != 0 {
		t.Fatal("redirect left the source origin")
	}
	if _, err = f.Post(context.Background(), foreign.URL+"/x", h, []byte("x")); !errors.Is(err, ErrPolicy) {
		t.Fatalf("foreign origin: %v", err)
	}
	if _, err = f.Post(context.Background(), "/query", h, nil); !errors.Is(err, ErrPolicy) {
		t.Fatalf("empty body: %v", err)
	}
	if _, err = f.Post(context.Background(), "/query", h, make([]byte, MaxPostBody+1)); !errors.Is(err, ErrPolicy) {
		t.Fatalf("oversized body: %v", err)
	}
	if _, err = f.Fetch(context.Background(), "POST", "/query", nil); !errors.Is(err, ErrPolicy) {
		t.Fatalf("Fetch must still refuse POST: %v", err)
	}
}
