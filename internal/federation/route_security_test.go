package federation

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
)

// Regression tests for the P2-M5 routing security review.

func TestStreamSharesPerPrincipalAndClient(t *testing.T) {
	s := &Service{cfg: Config{Now: time.Now, RouteMaxStreams: 6, RouteMaxStreamsPerPrincipal: 2, RouteMaxStreamsPerClient: 3}}
	var held []func()
	take := func(p, c, want string) {
		t.Helper()
		release, got := s.acquireStream(p, c)
		if got != want {
			t.Fatalf("acquire %s/%s: %q, want %q", p, c, got, want)
		}
		if release != nil {
			held = append(held, release)
		}
	}
	// One slow principal cannot take the whole budget.
	take("p:a", "10.0.0.1", "")
	take("p:a", "10.0.0.1", "")
	take("p:a", "10.0.0.1", streamPrincipalBusy)
	// Nor can one client address with many principals.
	take("p:b", "10.0.0.1", "")
	take("p:c", "10.0.0.1", streamClientBusy)
	// Others still get slots until the global budget is exhausted.
	take("p:c", "10.0.0.2", "")
	take("p:d", "10.0.0.3", "")
	take("p:e", "10.0.0.4", "")
	take("p:f", "10.0.0.5", streamBusy)
	for _, release := range held {
		release()
	}
	if len(s.route.byPrincipal) != 0 || len(s.route.byClient) != 0 || len(s.routeStreams()) != 0 {
		t.Fatalf("slots leaked: %v %v %d", s.route.byPrincipal, s.route.byClient, len(s.routeStreams()))
	}
	take("p:a", "10.0.0.1", "")
}

func TestRoutePrincipalRate(t *testing.T) {
	s := &Service{cfg: Config{Now: time.Now, RoutePrincipalRate: 5}}
	rate := s.routeLimits().rate
	for i := range 2 * rate {
		if !s.routeAllow("entry:p:a", rate) {
			t.Fatalf("request %d refused inside the burst", i)
		}
	}
	if s.routeAllow("entry:p:a", rate) {
		t.Fatal("burst exceeded without refusal")
	}
	if !s.routeAllow("entry:p:b", rate) {
		t.Fatal("another principal was refused")
	}
	// Flooding the table with other keys must not reset an active bucket.
	for i := range routeBucketLimit + 10 {
		s.routeAllow("entry:anon:"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String(), rate)
	}
	if s.routeAllow("entry:p:a", rate) {
		t.Fatal("flooding other keys reset the limited principal")
	}
	time.Sleep(300 * time.Millisecond)
	if !s.routeAllow("entry:p:a", rate) {
		t.Fatal("bucket did not refill")
	}
}

func proxiedResponse(body io.ReadCloser, h http.Header) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: body, ContentLength: -1}
}

type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
func (endless) Close() error { return nil }

// A client that stops reading loses its slot once the minimum average rate
// is missed, instead of holding it with one chunk per write timeout.
func TestStreamAnswerDropsStalledClient(t *testing.T) {
	s := &Service{cfg: Config{Now: time.Now, RouteMinTransferRate: 64 << 20, RouteTransferGrace: 200 * time.Millisecond}}
	done := make(chan time.Duration, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() { done <- time.Since(start) }()
		s.streamAnswer(w, r, proxiedResponse(endless{}, nil), routeTarget{format: "raw"}, "x", Member{Name: "m"}, FeedRouting{}, "a.bin", func() {})
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// Read the status line, then stop reading.
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-done:
		if d > 10*time.Second {
			t.Fatalf("stalled client held the stream for %v", d)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stalled client still holds the stream")
	}
}

// A holder that stops sending is cancelled by the watchdog.
func TestStreamAnswerCancelsStalledHolder(t *testing.T) {
	s := &Service{cfg: Config{Now: time.Now, RouteMinTransferRate: 1 << 20, RouteTransferGrace: 200 * time.Millisecond}}
	done := make(chan time.Duration, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		start := time.Now()
		defer func() { done <- time.Since(start) }()
		body := io.NopCloser(&stalled{ctx: ctx})
		s.streamAnswer(w, r, proxiedResponse(body, nil), routeTarget{format: "raw"}, "x", Member{Name: "m"}, FeedRouting{}, "a.bin", cancel)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	select {
	case d := <-done:
		if d > 5*time.Second {
			t.Fatalf("stalled holder kept the stream for %v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stalled holder still holds the stream")
	}
}

type stalled struct {
	ctx  context.Context
	sent bool
}

func (s *stalled) Read(p []byte) (int, error) {
	if !s.sent {
		s.sent = true
		return copy(p, "first bytes"), nil
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func TestProxiedResponseHeaders(t *testing.T) {
	s := &Service{cfg: Config{Now: time.Now}}
	holder := http.Header{
		"Content-Type":                 {"text/html"},
		"Content-Security-Policy":      {"default-src *"},
		"X-Content-Type-Options":       {"off"},
		"Access-Control-Allow-Origin":  {"*"},
		"Access-Control-Allow-Headers": {"Authorization"},
		"Cross-Origin-Resource-Policy": {"cross-origin"},
		"Clear-Site-Data":              {`"*"`},
		"Refresh":                      {"0; url=https://evil.example/"},
		"Set-Cookie":                   {"s=1"},
		"Connection":                   {"X-Hop, close"},
		"X-Hop":                        {"secret"},
		"Strict-Transport-Security":    {"max-age=0"},
		"Etag":                         {`"abc"`},
		"Content-Disposition":          {"attachment"},
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Strict-Transport-Security", "max-age=63072000")
	r := httptest.NewRequest(http.MethodGet, "/feeds/x/raw/a.html", nil)
	s.streamAnswer(rec, r, proxiedResponse(io.NopCloser(strings.NewReader("<script>alert(1)</script>")), holder), routeTarget{format: "raw"}, "x", Member{Name: "m"}, FeedRouting{}, "a.html", nil)
	h := rec.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("proxied answer not hardened: %v", h)
	}
	for _, k := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Cross-Origin-Resource-Policy", "Clear-Site-Data", "Refresh", "Set-Cookie", "Connection", "X-Hop"} {
		if h.Get(k) != "" {
			t.Fatalf("holder header %s reached the client: %v", k, h)
		}
	}
	if h.Get("Strict-Transport-Security") != "max-age=63072000" {
		t.Fatalf("holder replaced the entry origin's HSTS: %v", h)
	}
	if h.Get("Etag") != `"abc"` || h.Get("Content-Disposition") != "attachment" || h.Get("Content-Type") != "text/html" {
		t.Fatalf("content headers lost: %v", h)
	}
}

func TestReplayCacheQuotaIsPerIssuer(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := &Service{cfg: Config{Now: func() time.Time { return now }}}
	exp := now.Add(peer.AssertionLifetime).Unix()
	// A busy member fills far more than any one issuer may hold.
	busy := 0
	for i := range 1 << 18 {
		if s.rememberAssertion(peer.Assertion{Issuer: "busy", ID: "j" + time.Duration(i).String(), Expires: exp}) == "rate_limited" {
			busy++
		}
	}
	if busy == 0 {
		t.Fatal("one issuer was never limited")
	}
	if got := s.rememberAssertion(peer.Assertion{Issuer: "quiet", ID: "first-assertion-x", Expires: exp}); got != "" {
		t.Fatalf("a busy issuer limited another member: %q", got)
	}
	if got := s.rememberAssertion(peer.Assertion{Issuer: "quiet", ID: "first-assertion-x", Expires: exp}); got != "assertion_replayed" {
		t.Fatalf("replay not detected: %q", got)
	}
	// Expired records free the busy issuer's quota.
	now = now.Add(peer.AssertionLifetime + peer.MaxSkew + 2*time.Minute)
	if got := s.rememberAssertion(peer.Assertion{Issuer: "busy", ID: "after-expiry-xx", Expires: now.Add(time.Minute).Unix()}); got != "" {
		t.Fatalf("expired records kept the quota full: %q", got)
	}
}

func TestExportRequiresActiveTransfer(t *testing.T) {
	const self, target, other = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	doc := &Membership{Members: []Member{{InstanceID: target, Status: "active"}, {InstanceID: other, Status: "active"}}}
	body := func(home string, tr *FeedTransfer) *TopologyBody {
		return &TopologyBody{Feeds: []TopologyFeed{{Key: "lab/pkgs", Home: home, Transfer: tr}}}
	}
	// Active transfer away from this home: both sides may export.
	if !exportAllowed(body(self, &FeedTransfer{From: self, To: target, State: TransferDrain}), doc, self, target, "lab/pkgs") {
		t.Fatal("the target of an active transfer was refused")
	}
	// Aborted transfer still recorded: the would-be target has no access.
	if exportAllowed(body(self, &FeedTransfer{From: self, To: target, State: TransferAborted}), doc, self, target, "lab/pkgs") {
		t.Fatal("an aborted transfer still grants export access")
	}
	// Completed transfer to this member: only the former home (it demotes
	// and follows this member) keeps access, nobody else.
	done := &FeedTransfer{From: target, To: self, State: TransferComplete}
	if !exportAllowed(body(self, done), doc, self, target, "lab/pkgs") {
		t.Fatal("the demoted former home cannot follow the new home")
	}
	if exportAllowed(body(self, done), doc, self, other, "lab/pkgs") {
		t.Fatal("a member without a rule was granted access")
	}
	// A completed transfer that moved the home elsewhere grants this member's
	// view nothing (it is not the home).
	if exportAllowed(body(other, &FeedTransfer{From: self, To: other, State: TransferComplete}), doc, self, other, "lab/pkgs") {
		t.Fatal("a former home exported a feed it no longer homes")
	}
}

func TestContentAddressed(t *testing.T) {
	d := strings.Repeat("a", 64)
	cases := map[string]string{
		"oci library/alpine/blobs/sha256:" + d:                  d,
		"oci library/alpine/manifests/sha256:" + d:              d,
		"oci library/alpine/manifests/latest":                   "",
		"oci library/alpine/blobs/sha256:" + d + "?x=1":         "",
		"oci blobs/sha256:" + d:                                 "",
		"oci library/alpine/tags/sha256:" + d:                   "",
		"oci library/alpine/blobs/sha256:" + strings.ToUpper(d): "",
		"raw tools/sha256:" + d:                                 "",
		"raw blobs/sha256:" + d:                                 "",
		"maven org/x/1.0/x-1.0.jar":                             "",
		"npm p/-/p-1.0.0.tgz":                                   "",
	}
	for in, want := range cases {
		format, rest, _ := strings.Cut(in, " ")
		if got := contentAddressed(format, rest); got != want {
			t.Fatalf("contentAddressed(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestBlobURLsOnlyForDigestAddressedReads(t *testing.T) {
	s := &Service{cfg: Config{Now: time.Now}}
	for _, target := range []struct{ format, rest string }{{"raw", "tools/app.tgz"}, {"maven", "org/x/1.0/x-1.0.jar"}, {"helm", "charts/x-1.0.0.tgz"}, {"npm", "p/-/p-1.0.0.tgz"}} {
		if _, err := s.issueBlobURL(context.Background(), caller{}, Member{InstanceID: "m", APIURL: "https://m.example"}, "lab/pkgs", target.format, target.rest, ""); !errors.Is(err, errTicketScope) {
			t.Fatalf("ticket issued for %s %s: %v", target.format, target.rest, err)
		}
	}
	// The holder refuses path-scoped tickets outright, whoever signed them.
	rec := httptest.NewRecorder()
	q := url.Values{"k": {"lab/pkgs"}, "p": {"raw/tools/app.tgz"}, "t": {"x.y"}}
	r := httptest.NewRequest(http.MethodGet, "/federation/blob/app.tgz?"+q.Encode(), nil)
	r.SetPathValue("name", "app.tgz")
	s.BlobHandler().ServeHTTP(rec, r)
	var e struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&e)
	if rec.Code != http.StatusForbidden || e.Code != "blob_url_not_immutable" {
		t.Fatalf("holder accepted a mutable-path ticket: %d %q", rec.Code, e.Code)
	}
}

func TestBlobHandlerRedactsTicket(t *testing.T) {
	s := &Service{cfg: Config{Now: time.Now}}
	var seen string
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.BlobHandler().ServeHTTP(w, r)
		// Access logs and tracing run after the handler on the same request.
		seen = r.URL.String() + " " + r.RequestURI
	})
	q := url.Values{"k": {"lab/pkgs"}, "p": {"raw/tools/app.tgz"}, "t": {"secret-ticket"}}
	r := httptest.NewRequest(http.MethodGet, "/federation/blob/app.tgz?"+q.Encode(), nil)
	r.SetPathValue("name", "app.tgz")
	rec := httptest.NewRecorder()
	logged.ServeHTTP(rec, r)
	if strings.Contains(seen, "secret-ticket") || strings.Contains(seen, "t=") {
		t.Fatalf("ticket visible after the handler: %s", seen)
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("blob answer may leak its URL as a referrer: %v", rec.Header())
	}
}

func TestTicketClientBinding(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	const issuer, holder = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	now := time.Unix(1_700_000_000, 0)
	s := &Service{cfg: Config{Now: func() time.Time { return now }, InstanceID: holder}}
	s.trust.Store(&trust{state: State{State: "active"}, doc: &Membership{Members: []Member{{InstanceID: issuer, Status: "active", SigningKey: encodeKey(pub)}}}})
	key, target := "lab/images", "oci/app/blobs/sha256:"+strings.Repeat("b", 64)
	sign := func(bound string) string {
		a := peer.Assertion{Issuer: issuer, Federation: "f", Domain: "idp", Subject: "ci", Audience: holder, IssuedAt: now.Unix(), Expires: now.Add(routeTicketLifetime).Unix(),
			ID: "0123456789abcdef0123", Request: ticketBinding(key, target, bound)}
		raw, err := peer.SignAssertion(a, priv)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	bound := sign(ticketBindIP + ":192.0.2.10")
	if _, err := s.verifyTicket(bound, key, target, ticketBindIP+":192.0.2.10"); err != nil {
		t.Fatalf("bound ticket from its client: %v", err)
	}
	if _, err := s.verifyTicket(bound, key, target, ticketBindIP+":198.51.100.7"); err == nil {
		t.Fatal("bound ticket accepted from another client address")
	}
	if _, err := s.verifyTicket(bound, key, target, ""); err == nil {
		t.Fatal("bound ticket accepted with the binding stripped from the URL")
	}
	if _, err := s.verifyTicket(sign(""), key, target, ""); err != nil {
		t.Fatalf("unbound ticket: %v", err)
	}
}
