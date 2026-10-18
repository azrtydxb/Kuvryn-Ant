package oci

import (
	"net/http"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// ClientAuthTarget resolves the feed a /v2/ request addresses (prefix routing
// or mirror namespace) for the shared client-authentication middleware. It
// reports false when the request does not map to an OCI feed.
func (r *Registry) ClientAuthTarget(req *http.Request) (identity.FeedTarget, bool) {
	if !strings.HasPrefix(req.URL.Path, "/v2") || strings.ContainsAny(req.URL.Path, "\\\x00") {
		return identity.FeedTarget{}, false
	}
	ns := req.URL.Query().Get("ns")
	if req.URL.Path == "/v2/" || req.URL.Path == "/v2" {
		slug := r.mirrorSlug(ns)
		if slug == "" {
			return identity.FeedTarget{}, false
		}
		id, anonymous, err := r.feedBySlug(req.Context(), r.instance, slug)
		return identity.FeedTarget{FeedID: id, AnonymousRead: anonymous}, err == nil
	}
	name, _, _ := splitName(req.URL.Path)
	if name == "" {
		return identity.FeedTarget{}, false
	}
	rep, err := r.route(req.Context(), r.instance, name, ns)
	return identity.FeedTarget{FeedID: rep.feed, AnonymousRead: rep.anonymous}, err == nil
}
