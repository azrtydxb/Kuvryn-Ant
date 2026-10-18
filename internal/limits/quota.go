package limits

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// ErrQuota is matched by errors.Is for every quota rejection.
var ErrQuota = errors.New("storage quota exceeded")

// QuotaError describes the first quota a publication would exceed.
type QuotaError struct {
	Scope, FeedID, Namespace string
	Dimension                string // bytes or packages
	Limit, Used, Requested   int64
}

func (e *QuotaError) Error() string {
	target := "feed " + e.FeedID
	if e.Scope == ScopeNamespace {
		target += fmt.Sprintf(" namespace %q", e.Namespace)
	}
	return fmt.Sprintf("storage quota exceeded for %s: %s limit %d, used %d, publication adds %d", target, e.Dimension, e.Limit, e.Used, e.Requested)
}
func (e *QuotaError) Is(t error) bool { return t == ErrQuota }

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Usage measures logical content (sum of asset sizes, distinct packages with at
// least one asset) held by the local sources attached to a feed, optionally
// restricted to one namespace. A source attached to several feeds counts in each.
// excludeComponent is left out of the package count so a new version of an
// existing package does not count as another package.
//
// debt: usage is computed on demand and only when a quota applies, so it costs
// O(assets of the scope) per publish. Replace with maintained counters if
// quota-bound scopes grow large.
func Usage(ctx context.Context, q querier, instance, feed, namespace string, wholeFeed bool, excludeComponent string) (bytes, packages int64, err error) {
	err = q.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0)::bigint,count(DISTINCT c.id) FILTER (WHERE c.id<>COALESCE(NULLIF($3,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid))
 FROM feed_sources fs JOIN sources s ON s.id=fs.source_id AND s.instance_id=fs.instance_id AND s.kind='local'
 JOIN components c ON c.source_id=s.id AND c.instance_id=s.instance_id
 JOIN versions v ON v.component_id=c.id JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id
 WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND ($5::boolean OR c.namespace=$4)`, instance, feed, excludeComponent, namespace, wholeFeed).Scan(&bytes, &packages)
	return
}

// loadQuotas returns the quota rules of an instance that apply to a feed.
func loadQuotas(ctx context.Context, q querier, instance, feed string) ([]QuotaRule, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT quotas FROM limits_config WHERE instance_id=$1`, instance).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []QuotaRule
	if err = json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	var out []QuotaRule
	for _, r := range all {
		if strings.EqualFold(r.FeedID, feed) {
			out = append(out, r)
		}
	}
	return out, nil
}

// CheckPublish rejects a publication of size bytes (a new asset in component,
// in namespace of feed) that would exceed a configured quota. It must run inside
// the commit transaction before the asset row is inserted: the rejection then
// rolls back the whole publication, and a per-scope advisory lock serializes
// concurrent publications so usage cannot be oversubscribed. When no quota is
// configured for the feed this is a single indexed point read.
func CheckPublish(ctx context.Context, tx pgx.Tx, instance, feed, namespace, component string, size int64) error {
	rules, err := loadQuotas(ctx, tx, instance, feed)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.Scope == ScopeNamespace && rule.Namespace != namespace {
			continue
		}
		digest := sha256.Sum256([]byte("quota\x00" + instance + "\x00" + rule.key()))
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
			return err
		}
		bytes, packages, err := Usage(ctx, tx, instance, feed, rule.Namespace, rule.Scope == ScopeFeed, component)
		if err != nil {
			return err
		}
		packages++ // the publication's own package
		Default.observeQuota(rule, bytes+size, packages)
		var qe *QuotaError
		switch {
		case rule.MaxBytes != nil && bytes+size > *rule.MaxBytes:
			qe = &QuotaError{rule.Scope, feed, rule.Namespace, "bytes", *rule.MaxBytes, bytes, size}
		case rule.MaxPackages != nil && packages > *rule.MaxPackages:
			qe = &QuotaError{rule.Scope, feed, rule.Namespace, "packages", *rule.MaxPackages, packages - 1, 1}
		}
		if qe != nil {
			Default.quotaRejected(rule.Scope)
			noteQuota(ctx, qe)
			return qe
		}
	}
	return nil
}

// Note carries a quota rejection from the commit path back to the HTTP layer,
// which reports it uniformly (507) whatever status the protocol adapter chose.
type Note struct {
	mu  sync.Mutex
	err *QuotaError
}
type noteKey struct{}

func withNote(ctx context.Context) (context.Context, *Note) {
	n := &Note{}
	return context.WithValue(ctx, noteKey{}, n), n
}
func noteQuota(ctx context.Context, e *QuotaError) {
	if n, ok := ctx.Value(noteKey{}).(*Note); ok {
		n.mu.Lock()
		n.err = e
		n.mu.Unlock()
	}
}
func (n *Note) get() *QuotaError {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.err
}
