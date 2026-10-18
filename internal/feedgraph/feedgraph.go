// Package feedgraph rejects configuration that would make feed resolution
// recursive (spec 6.3). A feed depends on another feed of this same instance
// when a remote source attached to it uses this instance as its origin, or when
// a replication job fills one of its replicas from this instance. Any cycle in
// that graph would make a read or a replication run chase its own tail.
package feedgraph

import (
	"context"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

var selfOrigins atomic.Pointer[[]string]

// SetSelfOrigins declares the origins (scheme://host[:port]) under which this
// instance is reachable: its public URL and its listen address. Call before
// serving starts.
func SetSelfOrigins(origins ...string) {
	var normalized []string
	for _, o := range origins {
		if n, _, ok := normalize(o); ok {
			normalized = append(normalized, n)
		}
	}
	selfOrigins.Store(&normalized)
}

// normalize returns scheme://host[:port] (default ports removed, lowercase)
// and the URL path.
func normalize(raw string) (origin, path string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" || port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
		return u.Scheme + "://" + host, u.Path, true
	}
	return u.Scheme + "://" + host + ":" + port, u.Path, true
}

// isSelf reports whether origin is this instance.
func isSelf(origin string) bool {
	p := selfOrigins.Load()
	if p == nil {
		return false
	}
	for _, s := range *p {
		if s == origin {
			return true
		}
	}
	return false
}

// feedSlug extracts the feed slug from a package-protocol path.
func feedSlug(path string) string {
	for _, prefix := range []string{"/feeds/", "/v2/"} {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			slug, _, _ := strings.Cut(rest, "/")
			return slug
		}
	}
	return ""
}

// LoopError names a resolution cycle by feed slug.
type LoopError struct{ Path []string }

func (e *LoopError) Error() string {
	return "feed resolution loop: " + strings.Join(e.Path, " -> ") + " (a feed must not resolve, directly or through remote sources or replicas, to itself)"
}

// Queryer is satisfied by pgx.Tx and *pgxpool.Pool.
type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Check loads the feed dependency graph of an instance and returns a
// *LoopError when it contains a cycle. Run it inside the configuration
// transaction after applying the change, so a rejection rolls the change back.
func Check(ctx context.Context, q Queryer, instance string) error {
	if p := selfOrigins.Load(); p == nil || len(*p) == 0 {
		return nil
	}
	slugs := map[string]string{} // slug -> feed id
	names := map[string]string{} // feed id -> slug
	rows, err := q.Query(ctx, `SELECT id::text,slug FROM feeds WHERE instance_id=$1`, instance)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, slug string
		if err = rows.Scan(&id, &slug); err != nil {
			rows.Close()
			return err
		}
		slugs[slug], names[id] = id, slug
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	edges := map[string][]string{}
	add := func(from, to string) { edges[from] = append(edges[from], to) }
	rows, err = q.Query(ctx, `SELECT fs.feed_id::text,rc.config->>'origin' FROM feed_sources fs JOIN remote_source_configs rc ON rc.instance_id=fs.instance_id AND rc.source_id=fs.source_id WHERE fs.instance_id=$1`, instance)
	if err != nil {
		return err
	}
	for rows.Next() {
		var feed string
		var origin *string
		if err = rows.Scan(&feed, &origin); err != nil {
			rows.Close()
			return err
		}
		if origin == nil {
			continue
		}
		if o, path, ok := normalize(*origin); ok && isSelf(o) {
			if target, found := slugs[feedSlug(path)]; found {
				add(feed, target)
			}
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	rows, err = q.Query(ctx, `SELECT j.dest_feed_id::text,j.remote_feed_id::text,p.url FROM replication_jobs j JOIN replication_peers p ON p.instance_id=j.instance_id AND p.id=j.peer_id WHERE j.instance_id=$1`, instance)
	if err != nil {
		return err
	}
	for rows.Next() {
		var dest, remote, peer string
		if err = rows.Scan(&dest, &remote, &peer); err != nil {
			rows.Close()
			return err
		}
		if o, _, ok := normalize(peer); ok && isSelf(o) {
			if _, known := names[remote]; known {
				add(dest, remote)
			}
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	if cycle := routing.FindCycle(edges); cycle != nil {
		path := make([]string, len(cycle))
		for i, id := range cycle {
			path[i] = names[id]
		}
		return &LoopError{Path: path}
	}
	return nil
}
