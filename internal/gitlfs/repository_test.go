package gitlfs

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfiguration(t *testing.T) {
	if _, e := New(nil, nil); e == nil {
		t.Fatal("nil config")
	}
	s := &Repository{}
	for _, u := range []string{"https://packages.example", "http://127.0.0.1:9000", "http://[::1]:9000"} {
		if e := s.SetPublicURL(u); e != nil {
			t.Fatal(u, e)
		}
	}
	for _, u := range []string{"http://example.org", "http://localhost", "https://user:pass@example.org", "https://example.org/path", "https://example.org?x", "https://example.org#"} {
		if e := s.SetPublicURL(u); e == nil {
			t.Fatal(u)
		}
	}
}
func TestRange(t *testing.T) {
	for _, tc := range []struct {
		s    string
		o, n int64
	}{{"bytes=0-0", 0, 1}, {"bytes=5-", 5, 5}, {"bytes=-3", 7, 3}, {"bytes=-99", 0, 10}, {"bytes=3-99", 3, 7}} {
		o, n, e := parseRange(tc.s, 10)
		if e != nil || o != tc.o || n != tc.n {
			t.Fatal(tc, o, n, e)
		}
	}
	for _, raw := range []string{"bytes=", "bytes=-0", "bytes=10-", "bytes=3-2", "bytes=0-2,5-8", "bytes=+1-2", "items=0-1", "bytes=0-999999999999999999999999"} {
		if _, _, e := parseRange(raw, 10); e == nil {
			t.Fatal(raw)
		}
	}
}
func TestBoundedDecode(t *testing.T) {
	for _, body := range []string{`{} {}`, strings.Repeat(" ", 5000) + `{}`, `{"size":1.5}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", mediaType)
		if e := decode(httptest.NewRecorder(), r, new(object), 4096); e == nil {
			t.Fatal("accepted", len(body))
		}
	}
}
