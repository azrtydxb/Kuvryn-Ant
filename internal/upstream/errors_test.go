package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOffOriginRedirectAndFailureClasses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			w.WriteHeader(503)
			return
		}
		http.Redirect(w, r, "https://elsewhere.example/x", 301)
	}))
	defer server.Close()
	f, err := New(Config{Origin: server.URL, AllowPrivate: true, AllowHTTP: true, Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = f.Fetch(context.Background(), "GET", "/", nil)
	var re *RedirectError
	if !errors.As(err, &re) || re.Status != 301 || !errors.Is(err, ErrPolicy) {
		t.Fatalf("redirect: %v", err)
	}
	_, err = f.Fetch(context.Background(), "GET", "/down", nil)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Class != "status" || fe.Status != 503 || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("status: %v", err)
	}
}
