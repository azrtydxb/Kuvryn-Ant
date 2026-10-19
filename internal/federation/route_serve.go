package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Holder side of placement-aware routing: proxied reads arriving on the mTLS
// federation listener (GET|HEAD /federation/v1/proxy/feeds/{fed}/{slug}/...)
// and signed blob URLs on the public listener (GET /federation/blob/{name}).
// Both are answered by this member's own package adapters for the feed bound
// to the key, as the asserted principal: this member applies its own policy
// (local overrides, token scopes) and never trusts the entry member's
// authorization decision. Writes are refused with 405 writes_not_routed.
// Proxied reads share the listener's per-member budget of signed requests
// (peer.DefaultMemberRate) and a per-(issuer, principal) rate; an entry
// member over either gets 429 and tries the next candidate. Blob URLs serve
// digest-addressed reads only: the holder checks the feed holds an asset
// with the digest in the path and refuses any answer naming another digest.

// AnonymousDomain is the assertion domain of an anonymous caller. It is
// accepted only for proxied reads and blob URLs, where the holder serves it
// as its own anonymous reader (feeds with anonymous_read only).
const AnonymousDomain = "anonymous"

// maxRouteAssertionsPerIssuer bounds the in-memory replay cache of proxied
// reads per issuing member. A record lives for the assertion lifetime plus
// skew (about three minutes), so the quota is far above what the listener's
// per-member request budget (peer.DefaultMemberRate) can produce; a member
// that exhausts it is refused with 429 alone and never affects the reads
// other members proxy. The cache is bounded overall by the membership size.
const maxRouteAssertionsPerIssuer = 1 << 16

// issuerJTIs is one issuer's share of the proxied-read replay cache.
type issuerJTIs struct {
	seen  map[string]time.Time // jti → until
	swept time.Time            // last expiry sweep of a full quota
}

// rememberAssertion records the jti of a proxied-read assertion until it
// expires: "" when new, assertion_replayed when seen, rate_limited when the
// issuer's quota is full of unexpired records. A full quota is swept for
// expired records at most once per second, so an issuer at its quota cannot
// make every request scan its records.
func (s *Service) rememberAssertion(a peer.Assertion) string {
	rs := &s.route
	now := s.cfg.Now()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.jtis == nil {
		rs.jtis = map[string]*issuerJTIs{}
	}
	q := rs.jtis[a.Issuer]
	if q == nil {
		q = &issuerJTIs{seen: map[string]time.Time{}}
		rs.jtis[a.Issuer] = q
	}
	if until, ok := q.seen[a.ID]; ok && now.Before(until) {
		return "assertion_replayed"
	}
	if len(q.seen) >= maxRouteAssertionsPerIssuer {
		if now.Sub(q.swept) < time.Second {
			return "rate_limited"
		}
		q.swept = now
		for k, until := range q.seen {
			if !now.Before(until) {
				delete(q.seen, k)
			}
		}
		if len(q.seen) >= maxRouteAssertionsPerIssuer {
			return "rate_limited"
		}
	}
	q.seen[a.ID] = time.Unix(a.Expires, 0).Add(peer.MaxSkew + time.Minute)
	return ""
}

// assertAnonymous signs an anonymous-caller assertion for a request.
func (s *Service) assertAnonymous(member, method, requestURI string, body []byte) (string, error) {
	_, t, err := s.identity()
	if err != nil {
		return "", err
	}
	if m, ok := t.doc.Member(member); !ok || m.Status != "active" || member == s.cfg.InstanceID {
		return "", ErrNotFound
	}
	a := peer.Assertion{Issuer: s.cfg.InstanceID, Federation: t.doc.FederationID, Domain: AnonymousDomain, Subject: AnonymousDomain, Audience: member}
	now := s.cfg.Now()
	a.IssuedAt, a.Expires = now.Unix(), now.Add(peer.AssertionLifetime).Unix()
	a.Request = peer.RequestHash(method, requestURI, body)
	header, err := peer.SignAssertion(a, t.signer)
	if err == nil {
		s.metrics.issued.Add(1)
	}
	return header, err
}

// principalFor maps a verified assertion (or ticket) to the principal the
// adapters run as.
func (s *Service) principalFor(ctx context.Context, a peer.Assertion, policy *PolicyDocument, fid string) (identity.Principal, string) {
	if a.Domain == AnonymousDomain {
		if a.Subject != AnonymousDomain || a.Scopes != nil {
			return identity.Principal{}, "assertion_malformed"
		}
		p, err := (identity.Roles{DB: s.cfg.DB}).EnsureAnonymous(ctx, s.cfg.InstanceID)
		if err != nil {
			return identity.Principal{}, "unavailable"
		}
		return p, ""
	}
	p, _, reason := s.assertedPrincipal(ctx, a, policy, fid)
	return p, reason
}

// peerProxy answers a proxied read from another member.
func (s *Service) peerProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.route.writesNotRouted.Add(1)
		w.Header().Set("Allow", "GET, HEAD")
		peerFail(w, http.StatusMethodNotAllowed, "writes_not_routed")
		return
	}
	body, ok := readPeerBody(w, r)
	if !ok {
		return
	}
	// Proxied reads are idempotent GETs and HEADs that only the issuing
	// member can send (mTLS caller = issuer, request signature with its own
	// nonce), so their assertion replay record is kept in an in-memory cache
	// with a per-issuer quota rather than the durable nonce table (which
	// mutations and other assertions use): the issuer could mint a new
	// assertion anyway, a read must not cost a database write, and a restart
	// that forgets the records cannot replay a write because writes are never
	// routed (D6).
	a, policy, fid, reason := s.checkAssertion(r, body)
	if reason == "" {
		reason = s.rememberAssertion(a)
	}
	if reason == "rate_limited" {
		s.assertionRejected(r, a, reason)
		peerFail(w, http.StatusTooManyRequests, reason)
		return
	}
	if reason != "" {
		s.assertionFailed(w, r, a, reason)
		return
	}
	p, reason := s.principalFor(r.Context(), a, policy, fid)
	if reason != "" {
		s.assertionFailed(w, r, a, reason)
		return
	}
	// Per asserted principal (and issuer): one busy caller of a member cannot
	// use up the member's whole request budget on this listener.
	if !p.Anonymous && !s.routeAllow("proxy:"+a.Issuer+"/"+p.PrincipalID, s.routeLimits().rate) {
		peerFail(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	s.assertionAccepted(r.Context(), p, a, r.URL.Path)
	fed, slug := r.PathValue("fed"), r.PathValue("slug")
	if !slugPattern.MatchString(slug) || !slugPattern.MatchString(fed) {
		peerFail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/federation/v1/proxy/feeds/"+fed+"/"+slug+"/")
	if !ok {
		peerFail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	format, rest, _ := strings.Cut(rest, "/")
	if format == "v2" {
		format = "oci"
	}
	if r.URL.RawQuery != "" {
		rest += "?" + r.URL.RawQuery
	}
	s.dispatchHeld(w, r, p, fed+"/"+slug, format, rest, true)
}

// validRest rejects remainders that could leave the feed (dot segments,
// empty segments, encoded slashes or backslashes, control characters).
func validRest(escaped string) bool {
	if len(escaped) > 4096 || strings.ContainsAny(escaped, "\\\x00\r\n") {
		return false
	}
	lower := strings.ToLower(escaped)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%00") {
		return false
	}
	for _, seg := range strings.Split(escaped, "/") {
		if seg == "." || seg == ".." || strings.EqualFold(seg, "%2e") || strings.EqualFold(seg, "%2e%2e") || strings.EqualFold(seg, ".%2e") || strings.EqualFold(seg, "%2e.") {
			return false
		}
	}
	return true
}

// dispatchHeld serves a read of a held feed through the local adapters as p.
// peerMode answers refusals in the peer error format.
func (s *Service) dispatchHeld(w http.ResponseWriter, r *http.Request, p identity.Principal, key, format, restQuery string, peerMode bool) {
	fail := func(status int, code string) {
		if peerMode {
			peerFail(w, status, code)
			return
		}
		routeError(w, status, code, strings.ReplaceAll(code, "_", " "))
	}
	rt := s.routeRT.Load()
	if rt == nil || rt.Feeds == nil {
		fail(http.StatusServiceUnavailable, "unavailable")
		return
	}
	feedID, slug, feedFormat, err := s.heldFeed(r.Context(), key)
	if errors.Is(err, ErrNotFound) {
		fail(http.StatusNotFound, "not_held")
		return
	}
	if err != nil {
		fail(http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !formatMatches(feedFormat, format) {
		fail(http.StatusNotFound, "not_held")
		return
	}
	rest, rawQuery, _ := strings.Cut(restQuery, "?")
	if !validRest(rest) {
		fail(http.StatusBadRequest, "invalid_request")
		return
	}
	prefix := "/feeds/" + slug + "/" + format + "/"
	if format == "oci" {
		prefix = "/v2/" + slug + "/"
	}
	u, err := url.Parse(prefix + rest)
	if err != nil || !strings.HasPrefix(path.Clean(u.Path)+"/", prefix) {
		fail(http.StatusBadRequest, "invalid_request")
		return
	}
	u.RawQuery = rawQuery
	if p.Anonymous {
		policy, _, err := identity.LoadEffectivePolicy(r.Context(), s.cfg.DB, s.cfg.InstanceID, feedID)
		if err != nil || !policy.Allows(identity.MethodAnonymous) || len(policy.AnonymousNetworkGroupIDs) > 0 {
			// Anonymous access here is off or limited to client networks
			// this member cannot verify through a proxy.
			w.Header().Set(headerProxied, "1")
			routeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
			return
		}
	}
	req := r.Clone(r.Context())
	req.URL, req.RequestURI = u, ""
	if api, err := url.Parse(s.cfg.APIURL); err == nil && api.Host != "" {
		req.Host = api.Host
	}
	req.Header = http.Header{}
	for _, h := range requestHeaders {
		for _, v := range r.Header.Values(h) {
			req.Header.Add(h, v)
		}
	}
	ticket, drop := identity.IssueTicket(p)
	defer drop()
	req.Header.Set("Authorization", "Bearer "+ticket)
	w.Header().Del("Cache-Control") // the peer listener's default; the adapter decides
	pw := &proxiedWriter{w: w, rc: http.NewResponseController(w), slug: slug, svc: s}
	_ = pw.rc.SetWriteDeadline(time.Now().Add(5 * time.Minute)) // first byte may follow a pull-through
	s.route.servedRequests.Add(1)
	rt.Feeds.ServeHTTP(pw, req)
	s.route.servedBytes.Add(pw.n)
}

// proxiedWriter marks adapter answers for the entry member and keeps long
// transfers alive past the listener's write timeout.
type proxiedWriter struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	slug   string
	svc    *Service
	wrote  bool
	n      int64
	marked bool
}

func (p *proxiedWriter) Header() http.Header { return p.w.Header() }
func (p *proxiedWriter) mark() {
	if !p.marked {
		p.marked = true
		p.w.Header().Set(headerProxied, "1")
		p.w.Header().Set(headerFeedSlug, p.slug)
	}
}
func (p *proxiedWriter) WriteHeader(status int) {
	if p.wrote {
		return
	}
	p.wrote = true
	p.mark()
	p.w.WriteHeader(status)
}
func (p *proxiedWriter) Write(b []byte) (int, error) {
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	_ = p.rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
	n, err := p.w.Write(b)
	p.n += int64(n)
	return n, err
}
func (p *proxiedWriter) Flush()                      { _ = p.rc.Flush() }
func (p *proxiedWriter) Unwrap() http.ResponseWriter { return p.w }

// --- signed blob URLs ---

// Client binding of a blob URL (b= parameter): the ticket is valid only for
// the client address the entry member saw. Both members must resolve the
// same address (direct clients, or trusted proxies configured on both).
const ticketBindIP = "ip"

func ticketBinding(key, target, client string) string {
	sum := sha256.Sum256([]byte("kuvryn-ant-blob-url-v2\n" + key + "\n" + target + "\n" + client))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// errTicketScope refuses a blob URL for a path that is not content-addressed.
var errTicketScope = errors.New("federation: blob URLs are issued only for digest-addressed reads")

// issueBlobURL returns a signed, five-minute URL on member m's public
// listener for one digest-addressed read of a feed key. The ticket is signed
// by this member and carries the caller as an assertion; m re-authorizes it
// and serves only bytes whose digest is the one in the path. With bind the
// ticket is also bound to the client address.
func (s *Service) issueBlobURL(ctx context.Context, c caller, m Member, key, format, rest, client string) (string, error) {
	if contentAddressed(format, rest) == "" {
		return "", errTicketScope
	}
	var header string
	var err error
	if c.anonymous && c.principal.PrincipalID == "" || c.principal.Anonymous {
		header, err = s.assertAnonymous(m.InstanceID, http.MethodGet, "/", nil)
	} else {
		var req AssertedRequest
		req, err = s.Assert(ctx, c.principal, m.InstanceID, http.MethodGet, "/", nil)
		header = req.Header
	}
	if err != nil {
		return "", err
	}
	a, _, _, err := peer.ParseAssertion(header)
	if err != nil {
		return "", err
	}
	_, t, err := s.identity()
	if err != nil {
		return "", err
	}
	target := format + "/" + rest
	now := s.cfg.Now()
	a.IssuedAt, a.Expires = now.Unix(), now.Add(routeTicketLifetime).Unix()
	bound := ""
	if client != "" {
		bound = ticketBindIP + ":" + client
	}
	a.Request = ticketBinding(key, target, bound)
	ticket, err := peer.SignAssertion(a, t.signer)
	if err != nil {
		return "", err
	}
	q := "?k=" + url.QueryEscape(key) + "&p=" + url.QueryEscape(target)
	if client != "" {
		q += "&b=" + ticketBindIP
	}
	return m.APIURL + "/federation/blob/" + url.PathEscape(blobName(rest)) + q + "&t=" + url.QueryEscape(ticket), nil
}

var errTicket = errors.New("federation: blob URL invalid")

// verifyTicket checks a blob URL ticket: signed by an active member, for
// this member, unexpired (five minutes at most) and bound to the key, target
// and (when bound) client address in the URL. Tickets may be used
// repeatedly within their lifetime (clients retry and resume); they grant
// only what this member's own policy grants the asserted caller.
func (s *Service) verifyTicket(raw, key, target, bound string) (peer.Assertion, error) {
	a, payload, sig, err := peer.ParseAssertion(raw)
	if err != nil {
		return a, errTicket
	}
	if len(a.ID) < 16 || len(a.ID) > 64 || a.Subject == "" || len(a.Subject) > 512 || a.Domain == "" || len(a.Domain) > 64 || len(a.Scopes) > 64 ||
		a.Expires <= a.IssuedAt || time.Duration(a.Expires-a.IssuedAt)*time.Second > routeTicketLifetime {
		return a, errTicket
	}
	m, ok := s.Lookup(a.Issuer)
	if !ok || m.Status != "active" || !ed25519.Verify(m.SigningKey, payload, sig) {
		return a, errTicket
	}
	if a.Audience != s.cfg.InstanceID || a.Request != ticketBinding(key, target, bound) {
		return a, errTicket
	}
	now := s.cfg.Now()
	if now.Before(time.Unix(a.IssuedAt, 0).Add(-peer.MaxSkew)) || now.After(time.Unix(a.Expires, 0).Add(peer.MaxSkew)) {
		return a, errTicket
	}
	return a, nil
}

// blobName is the cosmetic last path segment of a blob URL (clients such as
// pip derive file names from it).
func blobName(rest string) string {
	p, _, _ := strings.Cut(rest, "?")
	name := path.Base("/" + p)
	if u, err := url.PathUnescape(name); err == nil {
		name = u
	}
	if name == "/" || name == "." || name == "" || strings.ContainsAny(name, "/\\") {
		return "blob"
	}
	return name
}

// redactTicketURL removes the blob URL parameters (the ticket is a bearer
// capability) from the request in place, so that nothing downstream of the
// handler, including middleware that logs or traces the request after it
// returns, ever sees them.
func redactTicketURL(r *http.Request) {
	if r.URL != nil {
		r.URL.RawQuery, r.URL.ForceQuery = "", false
	}
	r.RequestURI = r.URL.EscapedPath()
	r.Form, r.PostForm = nil, nil
}

// heldDigest reports whether a feed holds an asset with the blob digest.
func (s *Service) heldDigest(ctx context.Context, feedID, digest string) bool {
	var ok bool
	err := s.cfg.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM blobs b JOIN assets a ON a.blob_id=b.id JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id
JOIN feed_sources fs ON fs.source_id=c.source_id WHERE b.instance_id=$1 AND b.digest=$2 AND fs.instance_id=$1 AND fs.feed_id=$3)`, s.cfg.InstanceID, digest, feedID).Scan(&ok)
	return err == nil && ok
}

// digestWriter passes an adapter answer through only when a successful
// answer names the expected content digest; anything else becomes a refusal,
// so a ticket can never carry bytes other than the digest it was issued for.
type digestWriter struct {
	w       http.ResponseWriter
	want    string
	wrote   bool
	refused bool
}

func (d *digestWriter) Header() http.Header { return d.w.Header() }
func (d *digestWriter) WriteHeader(status int) {
	if d.wrote {
		return
	}
	d.wrote = true
	if status >= 200 && status < 300 && d.w.Header().Get("Docker-Content-Digest") != "sha256:"+d.want {
		d.refused = true
		h := d.w.Header()
		for k := range h {
			delete(h, k)
		}
		routeError(d.w, http.StatusForbidden, "blob_url_invalid", "the blob URL is invalid or expired")
		return
	}
	d.w.WriteHeader(status)
}
func (d *digestWriter) Write(b []byte) (int, error) {
	if !d.wrote {
		d.WriteHeader(http.StatusOK)
	}
	if d.refused {
		return len(b), nil
	}
	return d.w.Write(b)
}
func (d *digestWriter) Flush() {
	if d.wrote && !d.refused {
		_ = http.NewResponseController(d.w).Flush()
	}
}
func (d *digestWriter) Unwrap() http.ResponseWriter {
	if d.refused {
		return nil
	}
	return d.w
}

// BlobHandler serves signed blob URLs on the public listener.
func (s *Service) BlobHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redactTicketURL(r)
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			routeError(w, http.StatusMethodNotAllowed, "writes_not_routed", "signed blob URLs serve reads only")
			return
		}
		if len(q["k"]) != 1 || len(q["p"]) != 1 || len(q["t"]) != 1 || len(q["b"]) > 1 || (len(q["b"]) == 1 && q.Get("b") != ticketBindIP) {
			routeError(w, http.StatusBadRequest, "invalid_request", "invalid blob URL")
			return
		}
		key, target, raw := q.Get("k"), q.Get("p"), q.Get("t")
		if len(key) > 200 || len(target) > 4096 {
			routeError(w, http.StatusBadRequest, "invalid_request", "invalid blob URL")
			return
		}
		format, rest, _ := strings.Cut(target, "/")
		digest := contentAddressed(format, rest)
		if digest == "" {
			// Path-scoped tickets would let mutable content (raw, Maven,
			// archives under a stable name) change behind a capability.
			s.route.ticketsRefused.Add(1)
			routeError(w, http.StatusForbidden, "blob_url_not_immutable", "signed blob URLs serve digest-addressed content only")
			return
		}
		bound := ""
		if q.Get("b") == ticketBindIP {
			bound = ticketBindIP + ":" + clientKey(r)
		}
		a, err := s.verifyTicket(raw, key, target, bound)
		if err != nil {
			s.route.ticketsRefused.Add(1)
			routeError(w, http.StatusForbidden, "blob_url_invalid", "the blob URL is invalid or expired")
			return
		}
		policy, fid, err := s.currentPolicy(r.Context(), s.cfg.DB)
		if err != nil || policy == nil || a.Federation != fid {
			s.route.ticketsRefused.Add(1)
			routeError(w, http.StatusForbidden, "blob_url_invalid", "the blob URL is invalid or expired")
			return
		}
		p, reason := s.principalFor(r.Context(), a, policy, fid)
		if reason != "" {
			s.route.ticketsRefused.Add(1)
			routeError(w, http.StatusForbidden, "blob_url_invalid", "the blob URL is invalid or expired")
			return
		}
		if blobName(rest) != r.PathValue("name") {
			routeError(w, http.StatusBadRequest, "invalid_request", "invalid blob URL")
			return
		}
		if !p.Anonymous && !s.routeAllow("ticket:"+a.Issuer+"/"+p.PrincipalID, s.routeLimits().rate) {
			routeError(w, http.StatusTooManyRequests, "rate_limited", "too many blob URL reads for this caller; retry shortly")
			return
		}
		feedID, _, _, err := s.heldFeed(r.Context(), key)
		if err == nil && !s.heldDigest(r.Context(), feedID, digest) {
			s.route.ticketsRefused.Add(1)
			routeError(w, http.StatusNotFound, "not_found", "no asset of this feed has the digest")
			return
		}
		s.route.ticketsAccepted.Add(1)
		s.dispatchHeld(&digestWriter{w: w, want: digest}, r, p, key, format, rest, false)
	})
}
