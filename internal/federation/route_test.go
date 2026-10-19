package federation

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestReplacerStreams(t *testing.T) {
	old, repl := "http://b.example/feeds/x/npm/", "https://a.example/feeds/y/npm/"
	doc := strings.Repeat(`{"tarball":"`+old+`p/-/p-1.tgz"},`, 2000) + old
	for _, chunk := range []int{1, 7, 31, 4096} {
		r := newReplacer(iotest.OneByteReader(io.LimitReader(strings.NewReader(doc), int64(len(doc)))), []byte(old), []byte(repl))
		if chunk > 1 {
			r = newReplacer(&chunked{src: strings.NewReader(doc), n: chunk}, []byte(old), []byte(repl))
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.ReplaceAll(doc, old, repl); string(got) != want {
			t.Fatalf("chunk %d: rewrite mismatch (len %d vs %d)", chunk, len(got), len(want))
		}
	}
	// A partial match at the end of the stream is emitted unchanged.
	r := newReplacer(strings.NewReader("abc http://b.exa"), []byte(old), []byte(repl))
	if got, _ := io.ReadAll(r); string(got) != "abc http://b.exa" {
		t.Fatalf("tail: %q", got)
	}
}

type chunked struct {
	src io.Reader
	n   int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.src.Read(p)
}

func TestSplitFeedPath(t *testing.T) {
	for _, tc := range []struct {
		path               string
		oci, ok            bool
		slug, format, rest string
	}{
		{"/feeds/npm1/npm/@s/p/-/p-1.0.0.tgz", false, true, "npm1", "npm", "@s/p/-/p-1.0.0.tgz"},
		{"/feeds/a/raw/", false, true, "a", "raw", ""},
		{"/feeds/a", false, false, "", "", ""},
		{"/v2/hub/library/alpine/manifests/3", true, true, "hub", "oci", "library/alpine/manifests/3"},
		{"/v2/", true, false, "", "", ""},
	} {
		slug, format, rest, ok := splitFeedPath(tc.path, tc.oci)
		if ok != tc.ok || (ok && (slug != tc.slug || format != tc.format || rest != tc.rest)) {
			t.Fatalf("%s: %q %q %q %v", tc.path, slug, format, rest, ok)
		}
	}
}

func TestValidRestAndImmutable(t *testing.T) {
	for _, bad := range []string{"../x", "a/../../b", "a/%2e%2e/b", "a%2fb", "a\\b", "a/./b"} {
		if validRest(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	if !validRest("library/alpine/blobs/sha256:" + strings.Repeat("a", 64)) {
		t.Fatal("rejected a blob path")
	}
	cases := map[string]bool{
		"oci library/alpine/blobs/sha256:" + strings.Repeat("a", 64): true,
		"oci library/alpine/manifests/latest":                        false,
		"npm p/-/p-1.0.0.tgz":                                        true,
		"npm p":                                                      false,
		"maven org/x/maven-metadata.xml":                             false,
		"maven org/x/1.0/x-1.0.jar":                                  true,
		"go example.com/m/@v/list":                                   false,
		"go example.com/m/@v/v1.0.0.zip":                             true,
	}
	for in, want := range cases {
		format, rest, _ := strings.Cut(in, " ")
		if immutableRead(format, rest) != want {
			t.Fatalf("immutableRead(%s) != %v", in, want)
		}
	}
}

func TestRewritable(t *testing.T) {
	h := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	if !rewritable(h, 200) {
		t.Fatal("json metadata must be rebased")
	}
	for _, hh := range []http.Header{
		{"Content-Type": {"application/vnd.oci.image.manifest.v1+json"}},
		{"Content-Type": {"application/json"}, "Docker-Content-Digest": {"sha256:x"}},
		{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
		{"Content-Type": {"application/octet-stream"}},
	} {
		if rewritable(hh, 200) {
			t.Fatalf("must not rewrite %v", hh)
		}
	}
	if rewritable(h, 206) {
		t.Fatal("partial content must not be rewritten")
	}
}

func TestCircuitBreaker(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := &Service{cfg: Config{Now: func() time.Time { return now }}}
	for range routeBreakerFailures - 1 {
		s.observeMember("m", false, 0, "x")
	}
	if st, _, _ := s.circuit("m"); st != "closed" {
		t.Fatalf("opened early: %s", st)
	}
	s.observeMember("m", false, 0, "x")
	if st, _, _ := s.circuit("m"); st != "open" {
		t.Fatalf("not open: %s", st)
	}
	now = now.Add(routeBreakerOpen + time.Second)
	if st, _, _ := s.circuit("m"); st != "half_open" {
		t.Fatalf("not half open: %s", st)
	}
	s.observeMember("m", true, 20*time.Millisecond, "")
	if st, rtt, f := s.circuit("m"); st != "closed" || f != 0 || rtt != 20 {
		t.Fatalf("not closed after success: %s %v %d", st, rtt, f)
	}
	s.observeMember("m", true, 40*time.Millisecond, "")
	if _, rtt, _ := s.circuit("m"); rtt <= 20 || rtt >= 40 {
		t.Fatalf("EWMA: %v", rtt)
	}
}

func TestInterceptorPassesThroughAndHoldsMisses(t *testing.T) {
	rec := &recorder{h: http.Header{}}
	i := &interceptor{w: rec, header: http.Header{}}
	i.Header().Set("X-A", "1")
	i.WriteHeader(404)
	_, _ = i.Write([]byte("not here"))
	if !i.missed || rec.status != 0 {
		t.Fatalf("404 must be held back: %+v", rec)
	}
	i.replay()
	if rec.status != 404 || rec.h.Get("X-A") != "1" || !bytes.Equal(rec.body.Bytes(), []byte("not here")) {
		t.Fatalf("replay: %+v", rec)
	}
	rec2 := &recorder{h: http.Header{}}
	j := &interceptor{w: rec2, header: http.Header{}}
	_, _ = j.Write([]byte("ok"))
	if j.missed || rec2.status != 200 || rec2.body.String() != "ok" {
		t.Fatalf("pass-through: %+v", rec2)
	}
}

type recorder struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) WriteHeader(s int)           { r.status = s }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
