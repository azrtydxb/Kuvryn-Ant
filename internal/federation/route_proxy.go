package federation

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
)

// Entry-member side of placement-aware routing. RouteFeeds wraps the package
// adapters: a GET or HEAD of a federated feed that this member is not the
// home of is first answered locally (when a local feed is bound to the key);
// a local 404, or a feed key without a local feed, is routed to the best
// member holding the key:
//
//   - proxy (default, D5): the request is sent over the mTLS federation
//     listener with the caller's signed principal assertion; the holder
//     authorizes it under its own policy and the answer is streamed back with
//     bounded memory (32 KiB buffer, global stream budget), feed URLs in
//     textual metadata rebased onto this member, Range and conditional
//     headers passed through and client cancellation propagated;
//   - redirect (opt-in): 307 to the holder's public feed endpoint;
//   - blob URLs (opt-in in proxy mode): immutable blob reads get a 307 to a
//     signed, five-minute URL on the holder's public listener.
//
// Writes are never routed (D6): every non-read request is answered locally,
// with Ant-Feed-Home naming the home's feed endpoint.

// RouteRuntime is what routing needs from the serving process.
type RouteRuntime struct {
	// Feeds serves the package adapters directly (no routing wrapper), so a
	// proxied request is answered from this member's own content only and can
	// never be routed again.
	Feeds http.Handler
	// Storage and Lease enable the optional entry-member cache.
	Storage func(context.Context, string) (blob.Store, error)
	Lease   func() ownership.Lease
}

// SetRouteRuntime wires the serving hooks; nil Feeds disables serving
// proxied reads and blob URLs on this member.
func (s *Service) SetRouteRuntime(rt RouteRuntime) { s.routeRT.Store(&rt) }

// Headers on the federation proxy.
const (
	headerProxied  = "X-Ant-Fed-Proxied"   // set by the holder on adapter answers
	headerFeedSlug = "X-Ant-Fed-Feed-Slug" // the holder's local feed slug
	headerRoute    = "X-Kuvryn-Route"      // diagnostic route on the client answer
	HeaderFeedHome = "Ant-Feed-Home"       // non-home write: the home's feed endpoint
)

// routeClient is a pooled mTLS transport to one member (rebuilt when this
// member's certificate renews).
type routeClient struct {
	cert      *tls.Certificate
	transport *http.Transport
}

func (s *Service) transportFor(id peer.Identity, member string) *http.Transport {
	rs := &s.route
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.clients == nil {
		rs.clients = map[string]*routeClient{}
	}
	if c := rs.clients[member]; c != nil && c.cert == id.Certificate {
		return c.transport
	}
	if old := rs.clients[member]; old != nil {
		old.transport.CloseIdleConnections()
	}
	t := &http.Transport{
		TLSClientConfig:       peer.ClientTLS(id, member),
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // bytes pass through unchanged
		ForceAttemptHTTP2:     true,
	}
	rs.clients[member] = &routeClient{cert: id.Certificate, transport: t}
	return t
}

// splitFeedPath splits /feeds/{slug}/{format}/{rest} or /v2/{slug}/{rest}
// (escaped form) into slug, format ("oci" for /v2) and the escaped remainder
// after the format segment.
func splitFeedPath(escaped string, oci bool) (slug, format, rest string, ok bool) {
	if oci {
		tail, found := strings.CutPrefix(escaped, "/v2/")
		if !found {
			return "", "", "", false
		}
		slug, rest, found = strings.Cut(tail, "/")
		return slug, "oci", rest, found && slug != "" && rest != ""
	}
	tail, found := strings.CutPrefix(escaped, "/feeds/")
	if !found {
		return "", "", "", false
	}
	parts := strings.SplitN(tail, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	if len(parts) == 3 {
		rest = parts[2]
	}
	return parts[0], parts[1], rest, true
}

// caller identifies the client of a routed request.
type caller struct {
	principal identity.Principal
	anonymous bool
	valid     bool
}

// routeCaller authenticates the request the way the adapters would: a
// browser session principal from the context, or exactly one bearer (or
// basic password) credential. A credential that does not verify yields an
// invalid caller: the request is then answered locally, unchanged, so the
// adapter applies its own failure budget.
func (s *Service) routeCaller(r *http.Request) caller {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		if p, ok := identity.PrincipalFromContext(r.Context()); ok && p.PrincipalID != "" {
			return caller{principal: p, valid: true}
		}
		return caller{anonymous: true, valid: true}
	}
	if len(values) != 1 || len(values[0]) > 1024 {
		return caller{}
	}
	token := ""
	if scheme, rest, ok := strings.Cut(values[0], " "); ok && strings.EqualFold(scheme, "Bearer") {
		token = strings.TrimSpace(rest)
	} else if _, password, ok := r.BasicAuth(); ok {
		token = password
	}
	if token == "" {
		return caller{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	p, err := (identity.Tokens{DB: s.cfg.DB}).Authenticate(ctx, token)
	if err != nil || p.PrincipalID == "" || p.InstanceID != s.cfg.InstanceID {
		return caller{}
	}
	return caller{principal: p, anonymous: p.Anonymous, valid: true}
}

// RouteFeeds wraps a package adapter (oci: the /v2/ registry) with routing.
func (s *Service) RouteFeeds(oci bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := s.trust.Load(); t.doc == nil || t.state.State != "active" {
			next.ServeHTTP(w, r)
			return
		}
		slug, format, rest, ok := splitFeedPath(r.URL.EscapedPath(), oci)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		target, ok := s.lookupTarget(r.Context(), slug, format)
		if !ok || target.home {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// D6: publishes and deletes go to the home only. The local answer
			// stands (the replica denial, or feed not found) and names the home.
			s.route.writesNotRouted.Add(1)
			if home := s.homeEndpoint(r.Context(), target, oci); home != "" {
				w.Header().Set(HeaderFeedHome, home)
			}
			next.ServeHTTP(w, r)
			return
		}
		c := s.routeCaller(r)
		if !c.valid {
			next.ServeHTTP(w, r)
			return
		}
		local := r
		if !c.anonymous || c.principal.PrincipalID != "" {
			// routeCaller consumed a single-use ticket; hand the adapter a
			// fresh one for the same principal.
			if len(r.Header.Values("Authorization")) == 1 {
				ticket, drop := identity.IssueTicket(c.principal)
				defer drop()
				local = r.Clone(r.Context())
				local.Header.Set("Authorization", "Bearer "+ticket)
			}
		}
		var fallback func()
		if target.localFeed != "" {
			ic := &interceptor{w: w, header: http.Header{}}
			next.ServeHTTP(ic, local)
			if !ic.missed {
				return
			}
			if s.serveCached(w, r, target, rest, c) {
				return
			}
			fallback = func() { ic.replay() }
		} else {
			fallback = func() { next.ServeHTTP(w, local) }
		}
		s.routeRead(w, r, target, slug, rest, oci, c, fallback)
	})
}

// homeEndpoint is the home's feed endpoint for Ant-Feed-Home (best effort).
func (s *Service) homeEndpoint(ctx context.Context, target routeTarget, oci bool) string {
	doc := s.trust.Load().doc
	m, ok := doc.Member(target.feed.Home)
	if !ok || m.APIURL == "" {
		return ""
	}
	info, err := s.memberInfo(ctx, m, target.key)
	if err != nil {
		return m.APIURL
	}
	if oci {
		return m.APIURL + "/v2/" + info.FeedSlug + "/"
	}
	return m.APIURL + "/feeds/" + info.FeedSlug + "/" + target.format + "/"
}

// interceptor holds back a local 404 so the read can be routed; every other
// answer passes through untouched (including streaming and flushes).
type interceptor struct {
	w       http.ResponseWriter
	header  http.Header
	status  int
	missed  bool
	wrote   bool
	body    bytes.Buffer
	dropped bool
}

func (i *interceptor) Header() http.Header { return i.header }
func (i *interceptor) WriteHeader(status int) {
	if i.wrote || i.missed {
		return
	}
	i.status = status
	if status == http.StatusNotFound {
		i.missed = true
		return
	}
	i.wrote = true
	dst := i.w.Header()
	for k, v := range i.header {
		dst[k] = v
	}
	i.w.WriteHeader(status)
}
func (i *interceptor) Write(p []byte) (int, error) {
	if !i.wrote && !i.missed {
		i.WriteHeader(http.StatusOK)
	}
	if i.missed {
		if i.body.Len()+len(p) <= 8<<10 {
			i.body.Write(p)
		} else {
			i.dropped = true
		}
		return len(p), nil
	}
	return i.w.Write(p)
}
func (i *interceptor) Flush() {
	if i.wrote {
		_ = http.NewResponseController(i.w).Flush()
	}
}
func (i *interceptor) Unwrap() http.ResponseWriter {
	if i.missed {
		return nil
	}
	return i.w
}

// replay writes the held-back local 404.
func (i *interceptor) replay() {
	dst := i.w.Header()
	for k, v := range i.header {
		dst[k] = v
	}
	if i.dropped {
		dst.Del("Content-Length")
	}
	i.w.WriteHeader(http.StatusNotFound)
	_, _ = i.w.Write(i.body.Bytes())
}

func routeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "5")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}

// immutableRead reports reads of content-addressed or versioned blobs that a
// signed blob URL or the entry cache may answer.
func immutableRead(format, rest string) bool {
	path, _, _ := strings.Cut(rest, "?")
	switch format {
	case "oci":
		return strings.Contains(path, "/blobs/sha256:") || strings.Contains(path, "/manifests/sha256:")
	case "npm":
		return strings.Contains(path, "/-/") && strings.HasSuffix(path, ".tgz")
	case "pypi":
		return strings.HasPrefix(path, "packages/") || strings.HasPrefix(path, "files/")
	case "go":
		return strings.Contains(path, "/@v/") && (strings.HasSuffix(path, ".zip") || strings.HasSuffix(path, ".mod") || strings.HasSuffix(path, ".info")) && !strings.HasSuffix(path, "/list")
	case "maven":
		return !strings.Contains(path, "maven-metadata") && !strings.Contains(path, "-SNAPSHOT")
	case "nuget":
		return strings.HasSuffix(path, ".nupkg") || strings.HasSuffix(path, ".snupkg")
	case "cargo":
		return strings.HasSuffix(path, "/download") || strings.HasSuffix(path, ".crate")
	case "helm", "rubygems", "cran", "conda", "composer", "pub", "terraform", "swift", "ansible", "conan", "raw", "huggingface", "p2", "cocoapods":
		for _, ext := range []string{".tgz", ".tar.gz", ".gem", ".zip", ".whl", ".conda", ".tar.bz2", ".jar", ".crate"} {
			if strings.HasSuffix(path, ext) {
				return true
			}
		}
	}
	return false
}

// assertFor builds the assertion of the caller for a request to a member.
func (s *Service) assertFor(ctx context.Context, c caller, member, method, requestURI string) (string, error) {
	if c.anonymous && c.principal.PrincipalID == "" || c.principal.Anonymous {
		return s.assertAnonymous(member, method, requestURI, nil)
	}
	req, err := s.Assert(ctx, c.principal, member, method, requestURI, nil)
	if err != nil {
		return "", err
	}
	return req.Header, nil
}

// anonymousAllowed reports whether this member lets anonymous callers reach
// feeds it does not hold: its instance-wide client policy must allow
// anonymous reads without a network restriction (the holder sees the entry
// member, not the client address).
func (s *Service) anonymousAllowed(ctx context.Context) bool {
	p, _, err := identity.LoadEffectivePolicy(ctx, s.cfg.DB, s.cfg.InstanceID, "")
	return err == nil && p.Allows(identity.MethodAnonymous) && len(p.AnonymousNetworkGroupIDs) == 0
}

func (s *Service) routeRead(w http.ResponseWriter, r *http.Request, target routeTarget, slug, rest string, oci bool, c caller, fallback func()) {
	ctx := r.Context()
	topo, err := s.appliedTopology(ctx)
	if err != nil || topo == nil {
		fallback()
		return
	}
	candidates, routing := s.plan(ctx, target.feed, topo, false)
	decision, _ := decide("", routing, candidates)
	switch decision {
	case RouteOff, "no_candidates":
		fallback()
		return
	case "unavailable":
		s.route.count(routing.Mode, "unavailable")
		routeError(w, http.StatusServiceUnavailable, "route_unavailable", "no member holding this feed is reachable; retry later")
		return
	}
	if c.anonymous && c.principal.PrincipalID == "" && !s.anonymousAllowed(ctx) {
		fallback() // the local adapter issues its authentication challenge
		return
	}
	doc := s.trust.Load().doc
	var eligible []Member
	for _, cand := range candidates {
		if !cand.Eligible {
			continue
		}
		if m, ok := doc.Member(cand.MemberID); ok {
			eligible = append(eligible, m)
		}
		if len(eligible) == routeMaxAttempts {
			break
		}
	}
	query := ""
	if r.URL.RawQuery != "" {
		query = "?" + r.URL.RawQuery
	}
	switch {
	case routing.Mode == RouteRedirect && target.localFeed == "":
		// Only an entry member without a local copy redirects. Redirect
		// targets always hold a bound feed, and a member with a bound feed
		// proxies its misses instead, so redirects never chain or loop.
		s.redirectRead(w, r, target, rest+query, oci, eligible, fallback)
		return
	case routing.BlobURLs && r.Method == http.MethodGet && immutableRead(target.format, rest):
		if s.blobURL(w, r, target, rest+query, c, eligible) {
			return
		}
	}
	s.proxyRead(w, r, target, slug, rest+query, oci, c, eligible, routing, fallback)
}

// redirectRead answers 307 with the first reachable candidate's public feed
// endpoint. The client must be able to authenticate there on its own.
func (s *Service) redirectRead(w http.ResponseWriter, r *http.Request, target routeTarget, rest string, oci bool, eligible []Member, fallback func()) {
	for _, m := range eligible {
		if m.APIURL == "" {
			continue
		}
		info, err := s.memberInfo(r.Context(), m, target.key)
		if err != nil {
			var pe *peer.ErrPeer
			if !errors.As(err, &pe) {
				s.observeMember(m.InstanceID, false, 0, "unreachable")
			}
			continue
		}
		loc := m.APIURL + "/feeds/" + info.FeedSlug + "/" + target.format + "/" + rest
		if oci {
			loc = m.APIURL + "/v2/" + info.FeedSlug + "/" + rest
		}
		s.route.count(RouteRedirect, "ok")
		w.Header().Set("Location", loc)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set(headerRoute, "redirect; member="+m.Name)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	s.route.count(RouteRedirect, "unavailable")
	routeError(w, http.StatusServiceUnavailable, "route_unavailable", "no member holding this feed is reachable; retry later")
}

// blobURL answers an immutable blob read with a 307 to a signed, short-lived
// URL on the first candidate's public listener. It returns false when no
// ticket can be issued (the read is then proxied).
func (s *Service) blobURL(w http.ResponseWriter, r *http.Request, target routeTarget, rest string, c caller, eligible []Member) bool {
	for _, m := range eligible {
		if m.APIURL == "" {
			continue
		}
		loc, err := s.issueBlobURL(r.Context(), c, m, target.key, target.format, rest)
		if err != nil {
			return false
		}
		s.route.ticketsIssued.Add(1)
		s.route.count("blob_url", "ok")
		w.Header().Set("Location", loc)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set(headerRoute, "blob_url; member="+m.Name)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return true
	}
	return false
}

// requestHeaders are forwarded to the holder; everything else (credentials,
// cookies, client addresses) stays on the entry member.
var requestHeaders = []string{"Accept", "Range", "If-Range", "If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since", "User-Agent", "Docker-Distribution-Api-Version"}

// dropResponseHeaders never reach the client from a holder.
var dropResponseHeaders = map[string]bool{"Set-Cookie": true, "Www-Authenticate": true, "Strict-Transport-Security": true, "Connection": true, "Keep-Alive": true,
	"Transfer-Encoding": true, "Trailer": true, "Upgrade": true, "Proxy-Authenticate": true, "Alt-Svc": true, "X-Request-Id": true}

type attemptResult int

const (
	attemptNext     attemptResult = iota // try the next member (not held, 404, error)
	attemptDone                          // the answer was written
	attemptDenied                        // the holder refused the caller
	attemptNotFed                        // the caller cannot be asserted
	attemptLimited                       // the holder rate limited the request
	attemptFailed                        // transport or server failure
	attemptNotFound                      // the holder answered 404/410
)

func (s *Service) proxyRead(w http.ResponseWriter, r *http.Request, target routeTarget, slug, rest string, oci bool, c caller, eligible []Member, routing FeedRouting, fallback func()) {
	streams := s.routeStreams()
	select {
	case streams <- struct{}{}:
		defer func() { <-streams }()
	default:
		s.route.count(RouteProxy, "busy")
		routeError(w, http.StatusServiceUnavailable, "route_busy", "the federation proxy transfer budget is exhausted; retry shortly")
		return
	}
	// Waiting for the holder must not trip the public write timeout; the
	// stream extends the deadline per chunk afterwards.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(90 * time.Second))
	s.route.active.Add(1)
	defer s.route.active.Add(-1)
	var failed, limited, notFound bool
	for _, m := range eligible {
		res := s.proxyOnce(w, r, target, slug, rest, oci, c, m, routing)
		switch res {
		case attemptDone:
			s.route.count(RouteProxy, "ok")
			return
		case attemptDenied:
			s.route.count(RouteProxy, "denied")
			if c.anonymous && c.principal.PrincipalID == "" {
				fallback()
				return
			}
			routeError(w, http.StatusForbidden, "forbidden", "the member holding this feed denied access")
			return
		case attemptNotFed:
			s.route.count(RouteProxy, "not_federated")
			w.Header().Set(headerRoute, "not_federated")
			fallback()
			return
		case attemptLimited:
			limited = true
		case attemptFailed:
			failed = true
		case attemptNotFound:
			notFound = true
		}
	}
	switch {
	case failed || limited:
		s.route.count(RouteProxy, "unavailable")
		if limited && !failed {
			routeError(w, http.StatusServiceUnavailable, "route_rate_limited", "the member holding this feed is rate limiting this member; retry later")
			return
		}
		routeError(w, http.StatusServiceUnavailable, "route_unavailable", "no member holding this feed answered; retry later")
	default:
		_ = notFound
		s.route.count(RouteProxy, "not_found")
		fallback()
	}
}

// proxyOnce sends the read to one member and, when it answers with content,
// streams it to the client.
func (s *Service) proxyOnce(w http.ResponseWriter, r *http.Request, target routeTarget, slug, rest string, oci bool, c caller, m Member, routing FeedRouting) attemptResult {
	id, _, err := s.identity()
	if err != nil {
		return attemptFailed
	}
	fed, keySlug, _ := strings.Cut(target.key, "/")
	segment := target.format
	if oci {
		segment = "v2"
	}
	requestURI := "/federation/v1/proxy/feeds/" + url.PathEscape(fed) + "/" + url.PathEscape(keySlug) + "/" + segment + "/" + rest
	full, err := peer.TargetURL(m.FederationURL, requestURI)
	if err != nil {
		return attemptFailed
	}
	ctx := r.Context()
	req, err := http.NewRequestWithContext(ctx, r.Method, full, nil)
	if err != nil {
		return attemptFailed
	}
	// Bind the assertion to the request target exactly as the holder will
	// see it (the parsed, canonical request URI).
	assertion, err := s.assertFor(ctx, c, m.InstanceID, r.Method, req.URL.RequestURI())
	if errors.Is(err, ErrNotFederated) {
		return attemptNotFed
	}
	if err != nil {
		return attemptFailed
	}
	for _, h := range requestHeaders {
		for _, v := range r.Header.Values(h) {
			req.Header.Add(h, v)
		}
	}
	req.Header.Set(peer.HeaderAssertion, assertion)
	peer.SignRequest(req, id, nil, s.cfg.Now())
	if st, _, _ := s.circuit(m.InstanceID); st == "half_open" {
		s.trial(m.InstanceID)
	}
	start := s.cfg.Now()
	resp, err := s.transportFor(id, m.InstanceID).RoundTrip(req)
	if err != nil {
		if ctx.Err() == nil {
			s.observeMember(m.InstanceID, false, 0, "unreachable")
		}
		return attemptFailed
	}
	defer func() { _ = resp.Body.Close() }()
	rtt := s.cfg.Now().Sub(start)
	if resp.Header.Get(headerProxied) == "" {
		// A refusal by the federation listener itself (not an adapter answer).
		var e struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&e)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			s.observeMember(m.InstanceID, true, rtt, "")
			return attemptLimited
		case resp.StatusCode == http.StatusNotFound && e.Code == "not_held":
			s.observeMember(m.InstanceID, true, rtt, "")
			return attemptNotFound
		case resp.StatusCode == http.StatusForbidden && (e.Code == "principal_unknown" || e.Code == "assertion_unknown_domain"):
			s.observeMember(m.InstanceID, true, rtt, "")
			return attemptDenied
		default:
			s.observeMember(m.InstanceID, false, 0, "refused:"+e.Code)
			return attemptFailed
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		s.observeMember(m.InstanceID, true, rtt, "")
		return attemptNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		s.observeMember(m.InstanceID, true, rtt, "")
		return attemptDenied
	case resp.StatusCode == http.StatusTooManyRequests:
		s.observeMember(m.InstanceID, true, rtt, "")
		return attemptLimited
	case resp.StatusCode >= 500:
		s.observeMember(m.InstanceID, false, 0, "status "+strconv.Itoa(resp.StatusCode))
		return attemptFailed
	}
	s.observeMember(m.InstanceID, true, rtt, "")
	s.streamAnswer(w, r, resp, target, slug, m, routing, rest)
	return attemptDone
}

// rewritable reports textual metadata whose feed URLs are rebased onto this
// member. Content-addressed documents (OCI manifests) are never touched.
func rewritable(h http.Header, status int) bool {
	if status != http.StatusOK || h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" || h.Get("Docker-Content-Digest") != "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return false
	}
	if strings.HasPrefix(mt, "application/vnd.oci.") || strings.HasPrefix(mt, "application/vnd.docker.") {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "text/"), mt == "application/json", mt == "application/xml", strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"),
		strings.Contains(mt, "yaml"), mt == "application/javascript":
		return true
	}
	return false
}

// rebase returns the URL rewrites from the holder's feed endpoint to this
// member's (absolute first, then the relative feed path).
func (s *Service) rebase(m Member, holderSlug, slug, format string, oci bool) [][2]string {
	if holderSlug == "" || !slugPattern.MatchString(holderSlug) {
		return nil
	}
	if oci {
		if holderSlug == slug {
			return nil
		}
		return [][2]string{{"/v2/" + holderSlug + "/", "/v2/" + slug + "/"}}
	}
	from, to := "/feeds/"+holderSlug+"/"+format+"/", "/feeds/"+slug+"/"+format+"/"
	var out [][2]string
	if m.APIURL != "" && s.cfg.APIURL != "" && m.APIURL != s.cfg.APIURL {
		out = append(out, [2]string{m.APIURL + from, s.cfg.APIURL + from})
	}
	if from != to {
		out = append(out, [2]string{from, to})
	}
	return out
}

func applyRewrites(v string, rules [][2]string) string {
	for _, r := range rules {
		v = strings.ReplaceAll(v, r[0], r[1])
	}
	return v
}

// streamAnswer copies the holder's answer to the client with bounded memory.
func (s *Service) streamAnswer(w http.ResponseWriter, r *http.Request, resp *http.Response, target routeTarget, slug string, m Member, routing FeedRouting, rest string) {
	oci := target.format == "oci"
	rules := s.rebase(m, resp.Header.Get(headerFeedSlug), slug, target.format, oci)
	rewrite := len(rules) > 0 && rewritable(resp.Header, resp.StatusCode)
	dst := w.Header()
	for k, v := range resp.Header {
		if dropResponseHeaders[k] || strings.HasPrefix(k, "X-Ant-Fed-") {
			continue
		}
		dst[k] = v
	}
	for _, k := range []string{"Location", "Link", "Content-Location"} {
		if v := dst.Values(k); len(v) > 0 {
			out := make([]string, len(v))
			for i := range v {
				out[i] = applyRewrites(v[i], rules)
			}
			dst[k] = out
		}
	}
	if rewrite {
		dst.Del("Content-Length")
		dst.Del("Content-MD5")
		dst.Del("Digest")
	}
	dst.Set(headerRoute, "proxy; member="+m.Name)
	var cache *cacheWriter
	if !rewrite && r.Method == http.MethodGet && resp.StatusCode == http.StatusOK && target.localFeed != "" && routing.Cache &&
		r.Header.Get("Range") == "" && resp.Header.Get("Content-Range") == "" && immutableRead(target.format, rest) && resp.ContentLength > 0 {
		cache = s.newCacheWriter(r.Context(), target, rest, m, resp)
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	var src io.Reader = resp.Body
	if rewrite {
		for _, rule := range rules {
			src = newReplacer(src, []byte(rule[0]), []byte(rule[1]))
		}
	}
	if cache != nil {
		src = io.TeeReader(src, cache)
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, err := src.Read(buf)
		if k > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if _, werr := w.Write(buf[:k]); werr != nil {
				if cache != nil {
					cache.abort()
				}
				s.route.proxyBytes.Add(n)
				return
			}
			n += int64(k)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if cache != nil {
				cache.abort()
			}
			s.route.proxyBytes.Add(n)
			// A truncated transfer must never look complete.
			panic(http.ErrAbortHandler)
		}
	}
	s.route.proxyBytes.Add(n)
	if cache != nil {
		cache.finish(n)
	}
}

// replacer rewrites every occurrence of old with new in a stream, holding at
// most len(old)-1 bytes back between reads.
type replacer struct {
	src      io.Reader
	old, new []byte
	pending  []byte // processed output not yet returned
	tail     []byte // unprocessed input that may start a match
	eof      bool
	buf      []byte
}

func newReplacer(src io.Reader, old, new []byte) *replacer {
	return &replacer{src: src, old: old, new: new, buf: make([]byte, 32<<10)}
}

func (x *replacer) Read(p []byte) (int, error) {
	for len(x.pending) == 0 {
		if x.eof {
			if len(x.tail) > 0 {
				x.pending, x.tail = x.tail, nil
				break
			}
			return 0, io.EOF
		}
		n, err := x.src.Read(x.buf)
		data := append(x.tail, x.buf[:n]...)
		x.tail = nil
		if errors.Is(err, io.EOF) {
			x.eof = true
		} else if err != nil {
			return 0, err
		}
		out := bytes.ReplaceAll(data, x.old, x.new)
		if !x.eof {
			// Keep a suffix that could be the start of a match. Rewritten
			// bytes never contain a partial match at the end because a match
			// is replaced as a whole; only the raw suffix is held back.
			keep := 0
			for k := min(len(x.old)-1, len(data)); k > 0; k-- {
				if bytes.HasSuffix(data, x.old[:k]) {
					keep = k
					break
				}
			}
			if keep > 0 && bytes.HasSuffix(out, data[len(data)-keep:]) {
				x.tail = append([]byte(nil), data[len(data)-keep:]...)
				out = out[:len(out)-keep]
			}
		}
		x.pending = out
	}
	n := copy(p, x.pending)
	x.pending = x.pending[n:]
	return n, nil
}
