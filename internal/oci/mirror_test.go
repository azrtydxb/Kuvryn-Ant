package oci

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// hub is a fake token-authenticated upstream registry (Docker Hub shaped): a
// bearer realm on the same origin, scoped tokens, manifest list + child
// manifest, and blobs served through a redirect to a CDN origin.
type hub struct {
	tokens, blobs, cdnAuthLeaks, manifestHits atomic.Int32
	limit                                     atomic.Bool
	index, child, layer                       string
	cdn                                       *httptest.Server
	up                                        *httptest.Server
}

func newHub(f *remotetest.Fixture, repo string) *hub {
	h := &hub{layer: strings.Repeat("layer-bytes-", 3000)}
	h.child = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` + sha("cfg") + `","size":3},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:` + sha(h.layer) + `","size":` + itoa(len(h.layer)) + `}]}`
	h.index = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + sha(h.child) + `","size":` + itoa(len(h.child)) + `,"platform":{"architecture":"amd64","os":"linux"}}]}`
	h.cdn = f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			h.cdnAuthLeaks.Add(1)
		}
		h.blobs.Add(1)
		_, _ = io.WriteString(w, h.layer)
	}))
	h.up = f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			h.tokens.Add(1)
			_, _ = io.WriteString(w, `{"token":"t:`+r.URL.Query().Get("scope")+`","expires_in":300}`)
			return
		case h.limit.Load():
			w.Header().Set("Retry-After", "11")
			w.WriteHeader(429)
			return
		}
		scope := "repository:" + repo + ":pull"
		if r.Header.Get("Authorization") != "Bearer t:"+scope {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+h.up.URL+`/token",service="hub",scope="`+scope+`"`)
			w.WriteHeader(401)
			return
		}
		base := "/v2/" + repo
		switch r.URL.Path {
		case base + "/manifests/latest", base + "/manifests/sha256:" + sha(h.index):
			h.manifestHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			_, _ = io.WriteString(w, h.index)
		case base + "/manifests/sha256:" + sha(h.child):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = io.WriteString(w, h.child)
		case base + "/blobs/sha256:" + sha(h.layer):
			http.Redirect(w, r, h.cdn.URL+"/blob", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	return h
}

func itoa(n int) string { return strconv.Itoa(n) }

func anonGet(t *testing.T, url string, headers ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func mirrorRegistry(t *testing.T, f *remotetest.Fixture) *Registry {
	t.Helper()
	registry, err := New(f.Pool, f.Service, filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.Close() })
	return registry
}

func TestMirrorUnprefixedAnonymousPullThroughBearerUpstream(t *testing.T) {
	f := remotetest.New(t)
	h := newHub(f, "library/alpine")
	feed := f.AddFeed("dockerhub", "oci")
	f.AddRemote(feed, "oci", "dockerhub", 0, h.up.URL, remotetest.RemoteOptions{RedirectOrigins: []string{h.cdn.URL}})
	if _, err := f.Pool.Exec(context.Background(), `UPDATE feeds SET anonymous_read=true WHERE id=$1`, feed); err != nil {
		t.Fatal(err)
	}
	registry := mirrorRegistry(t, f)
	registry.SetMirror(Mirror{DefaultFeed: "dockerhub"})
	srv := httptest.NewServer(registry)
	defer srv.Close()
	const idx = "application/vnd.oci.image.index.v1+json"

	// A mirror ping carries ?ns= and must not challenge.
	if status, _, _ := anonGet(t, srv.URL+"/v2/?ns=docker.io"); status != 200 {
		t.Fatalf("anonymous mirror ping: %d", status)
	}
	status, body, header := anonGet(t, srv.URL+"/v2/library/alpine/manifests/latest", "Accept", idx)
	if status != 200 || body != h.index || header.Get("Content-Type") != idx {
		t.Fatalf("unprefixed tag: %d %s", status, body)
	}
	// HEAD on a tag (what containerd sends first) is answered with the digest.
	req, _ := http.NewRequest("HEAD", srv.URL+"/v2/library/alpine/manifests/latest", nil)
	req.Header.Set("Accept", idx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Docker-Content-Digest") != "sha256:"+sha(h.index) {
		t.Fatalf("HEAD: %v %v", err, resp)
	}
	resp.Body.Close()
	// Child manifest and blob by digest, concurrently, coalesce into one fetch.
	if status, body, _ = anonGet(t, srv.URL+"/v2/library/alpine/manifests/sha256:"+sha(h.child)); status != 200 || body != h.child {
		t.Fatalf("child manifest: %d", status)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body, _ := anonGet(t, srv.URL+"/v2/library/alpine/blobs/sha256:"+sha(h.layer))
			if status != 200 || body != h.layer {
				t.Errorf("blob: %d", status)
			}
		}()
	}
	wg.Wait()
	if h.blobs.Load() != 1 {
		t.Fatalf("concurrent pulls were not coalesced: %d CDN fetches", h.blobs.Load())
	}
	if h.cdnAuthLeaks.Load() != 0 {
		t.Fatal("Authorization reached the CDN origin")
	}
	if h.tokens.Load() < 1 || h.tokens.Load() > 3 {
		t.Fatalf("token fetches: %d", h.tokens.Load())
	}

	// Anonymous callers can never write, even to an anonymous_read feed.
	post, err := http.Post(srv.URL+"/v2/dockerhub/x/blobs/uploads/", "", nil)
	if err != nil || post.StatusCode != 401 {
		t.Fatalf("anonymous upload: %v %v", err, post)
	}
	post.Body.Close()
	// The prefixed form still works (and is anonymous too on this feed).
	if status, body, _ = anonGet(t, srv.URL+"/v2/dockerhub/library/alpine/manifests/sha256:"+sha(h.child)); status != 200 || body != h.child {
		t.Fatalf("prefixed: %d %s", status, body)
	}
	// Authenticated clients (Bearer token) still work on mirror paths.
	if status, _, _ = f.Get(srv.URL + "/v2/library/alpine/manifests/sha256:" + sha(h.child)); status != 200 {
		t.Fatalf("authenticated mirror pull: %d", status)
	}
	// Mirror routes are read-only for authenticated principals as well.
	preq, _ := http.NewRequest("POST", srv.URL+"/v2/library/alpine/blobs/uploads/", nil)
	preq.Header.Set("Authorization", "Bearer "+f.Token)
	presp, err := http.DefaultClient.Do(preq)
	if err != nil || presp.StatusCode != 405 {
		t.Fatalf("mirror write: %v %v", err, presp)
	}
	presp.Body.Close()
}

func TestMirrorNonAnonymousFeedChallengesAndPrefixedFeedWins(t *testing.T) {
	f := remotetest.New(t)
	h := newHub(f, "team/alpine")
	private := f.AddFeed("dockerhub", "oci")
	f.AddRemote(private, "oci", "dockerhub", 0, h.up.URL, remotetest.RemoteOptions{RedirectOrigins: []string{h.cdn.URL}})
	// A feed whose slug equals the first segment of a mirrored name.
	f.AddLocal(f.AddFeed("library", "oci"), "oci", "lib-local", 0)
	registry := mirrorRegistry(t, f)
	registry.SetMirror(Mirror{DefaultFeed: "dockerhub"})
	srv := httptest.NewServer(registry)
	defer srv.Close()

	status, _, header := anonGet(t, srv.URL+"/v2/")
	if status != 401 || !strings.HasPrefix(header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("non-anonymous mirror must challenge: %d %v", status, header)
	}
	if status, _, _ = anonGet(t, srv.URL+"/v2/library/alpine/manifests/latest"); status != 401 {
		t.Fatalf("unauthenticated pull: %d", status)
	}
	// Authenticated mirror pull works.
	if status, body, _ := f.Get(srv.URL+"/v2/team/alpine/manifests/sha256:"+sha(h.child), "Accept", "application/vnd.oci.image.manifest.v1+json"); status != 200 || body != h.child {
		t.Fatalf("authenticated: %d %s", status, body)
	}
	// "library" is an existing feed slug, so the prefixed route wins: the name
	// alpine/manifests/... is looked up in that (empty, local) feed instead.
	if status, _, _ := f.Get(srv.URL + "/v2/library/alpine/manifests/latest"); status != 404 {
		t.Fatalf("prefixed feed must win over the mirror: %d", status)
	}
}

func TestMirrorNamespaceMapSelectsFeed(t *testing.T) {
	f := remotetest.New(t)
	hubA := newHub(f, "team/app")
	hubB := newHub(f, "team/app")
	hubB.child = strings.Replace(hubB.child, `"size":3`, `"size":4`, 1) // distinct content per upstream
	hubB.index = strings.Replace(hubB.index, sha(hubA.child), sha(hubB.child), 1)
	a := f.AddFeed("ghcr", "oci")
	f.AddRemote(a, "oci", "ghcr", 0, hubA.up.URL, remotetest.RemoteOptions{RedirectOrigins: []string{hubA.cdn.URL}})
	b := f.AddFeed("quay", "oci")
	f.AddRemote(b, "oci", "quay", 0, hubB.up.URL, remotetest.RemoteOptions{RedirectOrigins: []string{hubB.cdn.URL}})
	for _, id := range []string{a, b} {
		if _, err := f.Pool.Exec(context.Background(), `UPDATE feeds SET anonymous_read=true WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	registry := mirrorRegistry(t, f)
	registry.SetMirror(Mirror{Namespaces: map[string]string{"ghcr.io": "ghcr", "quay.io": "quay"}})
	srv := httptest.NewServer(registry)
	defer srv.Close()
	if status, _, _ := anonGet(t, srv.URL+"/v2/?ns=ghcr.io"); status != 200 {
		t.Fatalf("ping ns: %d", status)
	}
	if status, body, _ := anonGet(t, srv.URL+"/v2/team/app/manifests/sha256:"+sha(hubA.child)+"?ns=ghcr.io"); status != 200 || body != hubA.child {
		t.Fatalf("ghcr ns: %d", status)
	}
	if status, body, _ := anonGet(t, srv.URL+"/v2/team/app/manifests/sha256:"+sha(hubB.child)+"?ns=quay.io"); status != 200 || body != hubB.child {
		t.Fatalf("quay ns: %d", status)
	}
	// Each name resolves only through its own upstream.
	if status, _, _ := anonGet(t, srv.URL+"/v2/team/app/manifests/sha256:"+sha(hubB.child)+"?ns=ghcr.io"); status != 404 {
		t.Fatalf("cross-upstream: %d", status)
	}
	// An unmapped namespace, and no ns at all, are refused (no default feed).
	if status, _, _ := anonGet(t, srv.URL+"/v2/team/app/manifests/latest?ns=docker.io"); status != 401 && status != 404 {
		t.Fatalf("unmapped ns: %d", status)
	}
	if status, _, _ := anonGet(t, srv.URL+"/v2/team/app/manifests/latest"); status != 401 && status != 404 {
		t.Fatalf("missing ns: %d", status)
	}
}

func TestMirrorSurfacesUpstreamRateLimit(t *testing.T) {
	f := remotetest.New(t)
	h := newHub(f, "library/alpine")
	feed := f.AddFeed("dockerhub", "oci")
	f.AddRemote(feed, "oci", "dockerhub", 0, h.up.URL, remotetest.RemoteOptions{RedirectOrigins: []string{h.cdn.URL}})
	if _, err := f.Pool.Exec(context.Background(), `UPDATE feeds SET anonymous_read=true WHERE id=$1`, feed); err != nil {
		t.Fatal(err)
	}
	registry := mirrorRegistry(t, f)
	registry.SetMirror(Mirror{DefaultFeed: "dockerhub"})
	srv := httptest.NewServer(registry)
	defer srv.Close()
	h.limit.Store(true)
	status, body, header := anonGet(t, srv.URL+"/v2/library/alpine/blobs/sha256:"+sha(h.layer))
	if status != 429 || header.Get("Retry-After") != "11" || !strings.Contains(body, "TOOMANYREQUESTS") {
		t.Fatalf("blob rate limit: %d %v %s", status, header, body)
	}
	status, _, header = anonGet(t, srv.URL+"/v2/library/alpine/manifests/latest")
	if status != 429 || header.Get("Retry-After") != "11" {
		t.Fatalf("tag rate limit: %d %v", status, header)
	}
	// Once the upstream recovers the very next request succeeds (no sticky
	// negative entry for the throttled or unauthenticated attempts).
	h.limit.Store(false)
	if status, _, _ = anonGet(t, srv.URL+"/v2/library/alpine/blobs/sha256:"+sha(h.layer)); status != 200 {
		t.Fatalf("recovery: %d", status)
	}
}

// TestV2PingChallengesWithoutNamespace covers the CRI-O / containers/image flow:
// the client pings /v2/ and sends credentials only if that answers 401. The bare
// ping must therefore challenge even when the default mirror feed is anonymous,
// while mirror pings (?ns=) and anonymous pulls keep working. Both the registry
// alone and the registry behind the client-authentication middleware are driven.
func TestV2PingChallengesWithoutNamespace(t *testing.T) {
	f := remotetest.New(t)
	h := newHub(f, "team/alpine")
	anon := f.AddFeed("dockerhub", "oci")
	f.AddRemote(anon, "oci", "dockerhub", 0, h.up.URL, remotetest.RemoteOptions{RedirectOrigins: []string{h.cdn.URL}})
	if _, err := f.Pool.Exec(context.Background(), `UPDATE feeds SET anonymous_read=true WHERE id=$1`, anon); err != nil {
		t.Fatal(err)
	}
	private := f.AddFeed("oci-hosted", "oci")
	f.AddRemote(private, "oci", "private-hub", 0, h.up.URL, remotetest.RemoteOptions{RedirectOrigins: []string{h.cdn.URL}})
	registry := mirrorRegistry(t, f)
	registry.SetMirror(Mirror{DefaultFeed: "dockerhub"})
	ca := &identity.ClientAuth{DB: f.Pool, InstanceID: f.Service.Lease().InstanceID, ResolveOCI: registry.ClientAuthTarget, CacheTTL: -1}
	handlers := map[string]http.Handler{"registry": registry, "middleware": ca.Middleware(registry)}
	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			const challenge = `Basic realm="Kuvryn Ant"`
			manifest := "/manifests/sha256:" + sha(h.child)
			const accept = "application/vnd.oci.image.manifest.v1+json"

			for _, method := range []string{http.MethodGet, http.MethodHead} {
				req, _ := http.NewRequest(method, srv.URL+"/v2/", nil)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != challenge {
					t.Fatalf("%s ping without ns: %d %v", method, resp.StatusCode, resp.Header)
				}
			}
			// With credentials the ping succeeds.
			req, _ := http.NewRequest("GET", srv.URL+"/v2/", nil)
			req.SetBasicAuth("service", f.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("authenticated ping: %d", resp.StatusCode)
			}
			// Mirror pings stay anonymous.
			if status, _, _ := anonGet(t, srv.URL+"/v2/?ns=docker.io"); status != 200 {
				t.Fatalf("anonymous mirror ping: %d", status)
			}
			// A client that was challenged and has no credentials continues anonymously.
			if status, body, _ := anonGet(t, srv.URL+"/v2/team/alpine"+manifest, "Accept", accept); status != 200 || body != h.child {
				t.Fatalf("anonymous pull after challenge: %d %s", status, body)
			}
			// A protected feed refuses anonymous reads but serves the credentialed pull.
			if status, _, _ := anonGet(t, srv.URL+"/v2/oci-hosted/team/alpine"+manifest, "Accept", accept); status != 401 {
				t.Fatalf("anonymous protected pull: %d", status)
			}
			preq, _ := http.NewRequest("GET", srv.URL+"/v2/oci-hosted/team/alpine"+manifest, nil)
			preq.SetBasicAuth("service", f.Token)
			preq.Header.Set("Accept", accept)
			presp, err := http.DefaultClient.Do(preq)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(presp.Body)
			presp.Body.Close()
			if presp.StatusCode != 200 || string(data) != h.child {
				t.Fatalf("credentialed protected pull: %d %s", presp.StatusCode, data)
			}
		})
	}
}
