package replication

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Outgoing replication visibility. Replication is pull based, so an authority
// only learns about its consumers from their export API calls. ConsumerTracker
// accumulates those calls in memory (one short mutex-guarded map update per
// request) and a background flusher upserts them per (consumer, feed) into
// replication_consumers, the same way observability.Traffic batches counters.

// jobHeader optionally names the destination job on every export request.
const jobHeader = "X-Replication-Job"

// Consumer kinds.
const (
	ConsumerPrincipal = "principal" // bearer token of a local principal (token mode)
	ConsumerMember    = "member"    // federation member instance (federation mode)
)

// StaleAfter is the silence after which an outgoing consumer is reported stale.
const StaleAfter = time.Hour

const (
	maxConsumerKeys   = 5000
	sampleBucket      = 5 * time.Minute
	sampleRetention   = 7 * 24 * time.Hour
	consumerRetention = 90 * 24 * time.Hour
	maxJobNameBytes   = 128
)

// consumer identifies who pulled.
type consumer struct{ kind, id string }

type consumerKey struct {
	instance, feed, kind, id string
}

// pullEvent is one export request outcome.
type pullEvent struct {
	bytes  int64
	served *int64 // change sequence served up to (changes pages only)
	head   *int64 // authority head observed, when the request reads it
	job    string
	err    string // non-empty for a failed request
}

type consumerAcc struct {
	first, last time.Time
	pulls       int64
	bytes       int64
	served      *int64
	head        *int64
	job         string
	failed      bool
	errAt       time.Time
	errText     string
}

// ConsumerTracker batches export-API activity per consumer and feed. The zero
// value is not usable; use NewConsumerTracker. A nil tracker records nothing.
type ConsumerTracker struct {
	mu        sync.Mutex
	acc       map[consumerKey]*consumerAcc
	now       func() time.Time
	lastPrune time.Time
}

func NewConsumerTracker() *ConsumerTracker {
	return &ConsumerTracker{acc: map[consumerKey]*consumerAcc{}, now: time.Now}
}

// sanitizeJob keeps the optional job header short and printable.
func sanitizeJob(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > maxJobNameBytes {
		v = v[:maxJobNameBytes]
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return v
}

func (t *ConsumerTracker) record(instance, feed string, c consumer, ev pullEvent) {
	if t == nil || c.id == "" {
		return
	}
	k := consumerKey{instance, feed, c.kind, c.id}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.acc[k]
	if !ok {
		if len(t.acc) >= maxConsumerKeys {
			return
		}
		a = &consumerAcc{first: now}
		t.acc[k] = a
	}
	a.last = now
	a.pulls++
	a.bytes += ev.bytes
	if ev.served != nil {
		a.served = ev.served
	}
	if ev.head != nil {
		a.head = ev.head
	}
	if ev.job != "" {
		a.job = ev.job
	}
	if ev.err != "" {
		a.failed, a.errAt, a.errText = true, now, ev.err
	} else {
		a.failed = false
	}
}

const upsertConsumer = `
INSERT INTO replication_consumers(instance_id,feed_id,consumer_kind,consumer_id,first_seen,last_seen,pulls,bytes_served,last_sequence,head_sequence,job_name,last_result,last_error_at,last_error)
SELECT f.instance_id,f.id,$3,$4,$5,$5,$6,$7,$8,$9,$10,$11,$12,$13 FROM feeds f WHERE f.instance_id=$1 AND f.id=$2
ON CONFLICT(instance_id,feed_id,consumer_kind,consumer_id) DO UPDATE SET
 last_seen=GREATEST(replication_consumers.last_seen,EXCLUDED.last_seen),
 pulls=replication_consumers.pulls+EXCLUDED.pulls,
 bytes_served=replication_consumers.bytes_served+EXCLUDED.bytes_served,
 last_sequence=COALESCE(EXCLUDED.last_sequence,replication_consumers.last_sequence),
 head_sequence=COALESCE(EXCLUDED.head_sequence,replication_consumers.head_sequence),
 job_name=COALESCE(NULLIF(EXCLUDED.job_name,''),replication_consumers.job_name),
 last_result=EXCLUDED.last_result,
 last_error_at=COALESCE(EXCLUDED.last_error_at,replication_consumers.last_error_at),
 last_error=CASE WHEN EXCLUDED.last_error_at IS NULL THEN replication_consumers.last_error ELSE EXCLUDED.last_error END`

const upsertSample = `
INSERT INTO replication_consumer_samples(instance_id,feed_id,consumer_kind,consumer_id,bucket,last_sequence,head_sequence)
SELECT instance_id,feed_id,consumer_kind,consumer_id,$5,$6,$7 FROM replication_consumers WHERE instance_id=$1 AND feed_id=$2 AND consumer_kind=$3 AND consumer_id=$4
ON CONFLICT(instance_id,feed_id,consumer_kind,consumer_id,bucket) DO UPDATE SET last_sequence=EXCLUDED.last_sequence,head_sequence=EXCLUDED.head_sequence`

// Flush persists and clears the accumulated activity. Entries that fail to
// persist are merged back and retried by the next flush.
func (t *ConsumerTracker) Flush(ctx context.Context, db *pgxpool.Pool) error {
	if t == nil || db == nil {
		return nil
	}
	t.mu.Lock()
	batch := t.acc
	t.acc = map[consumerKey]*consumerAcc{}
	prune := t.now().Sub(t.lastPrune) > time.Hour
	if prune {
		t.lastPrune = t.now()
	}
	t.mu.Unlock()
	var first error
	for k, a := range batch {
		if err := t.persist(ctx, db, k, a); err != nil {
			if first == nil {
				first = err
			}
			t.merge(k, a)
		}
	}
	if prune {
		_, _ = db.Exec(ctx, `DELETE FROM replication_consumer_samples WHERE bucket<$1`, t.now().Add(-sampleRetention))
		_, _ = db.Exec(ctx, `DELETE FROM replication_consumers WHERE last_seen<$1`, t.now().Add(-consumerRetention))
	}
	return first
}

func (t *ConsumerTracker) persist(ctx context.Context, db *pgxpool.Pool, k consumerKey, a *consumerAcc) error {
	result := "ok"
	var errAt *time.Time
	if a.failed {
		result = "error"
	}
	if !a.errAt.IsZero() {
		errAt = &a.errAt
	}
	if _, err := db.Exec(ctx, upsertConsumer, k.instance, k.feed, k.kind, k.id, a.first, a.pulls, a.bytes, a.served, a.head, a.job, result, errAt, a.errText); err != nil {
		return err
	}
	if a.served != nil && a.head != nil {
		bucket := a.last.Truncate(sampleBucket)
		if _, err := db.Exec(ctx, upsertSample, k.instance, k.feed, k.kind, k.id, bucket, *a.served, *a.head); err != nil {
			return err
		}
	}
	return nil
}

func (t *ConsumerTracker) merge(k consumerKey, a *consumerAcc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.acc[k]
	if !ok {
		if len(t.acc) < maxConsumerKeys {
			t.acc[k] = a
		}
		return
	}
	cur.first = a.first
	cur.pulls += a.pulls
	cur.bytes += a.bytes
	if cur.served == nil {
		cur.served = a.served
	}
	if cur.head == nil {
		cur.head = a.head
	}
	if cur.job == "" {
		cur.job = a.job
	}
	if cur.errAt.IsZero() {
		cur.errAt, cur.errText = a.errAt, a.errText
	}
}

// Run flushes every interval until ctx ends, then once more.
func (t *ConsumerTracker) Run(ctx context.Context, db *pgxpool.Pool, interval time.Duration) {
	if t == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = t.Flush(flush, db)
			cancel()
			return
		case <-ticker.C:
			flush, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = t.Flush(flush, db)
			cancel()
		}
	}
}

// LagSample is one point of a consumer's lag history.
type LagSample struct {
	At           time.Time `json:"at"`
	LastSequence int64     `json:"last_sequence"`
	HeadSequence int64     `json:"head_sequence"`
	LagChanges   int64     `json:"lag_changes"`
}

// Outgoing is one consumer pulling one feed of this instance.
type Outgoing struct {
	ID string `json:"id"`
	// ConsumerKind is "member" (federation) or "principal" (peer token).
	ConsumerKind     string `json:"consumer_kind"`
	ConsumerName     string `json:"consumer_name"`
	MemberInstanceID string `json:"member_instance_id,omitempty"`
	PrincipalID      string `json:"principal_id,omitempty"`
	FeedID           string `json:"feed_id"`
	FeedSlug         string `json:"feed_slug"`
	FeedFormat       string `json:"feed_format"`
	// RemoteJob is the destination job name as the consumer reported it; empty
	// when the consumer did not say.
	RemoteJob string `json:"remote_job,omitempty"`
	// RuleID is the federation replication rule that targets this consumer for
	// the feed, when there is one.
	RuleID        string      `json:"rule_id,omitempty"`
	FirstSeen     time.Time   `json:"first_seen"`
	LastSeen      time.Time   `json:"last_seen"`
	Pulls         int64       `json:"pulls"`
	BytesServed   int64       `json:"bytes_served"`
	LastSequence  *int64      `json:"last_sequence,omitempty"`
	HeadSequence  int64       `json:"head_sequence"`
	LagChanges    int64       `json:"lag_changes"`
	LagSeconds    int64       `json:"lag_seconds"`
	Status        string      `json:"status"` // active | stale | error
	LastError     string      `json:"last_error,omitempty"`
	LastErrorAt   *time.Time  `json:"last_error_at,omitempty"`
	History       []LagSample `json:"history"`
	StaleAfterSec int64       `json:"stale_after_seconds"`
}

// ListOutgoing returns the consumers that pulled from this instance, newest
// first. canSee filters by feed (the caller's replicate/configure right).
func (s *Service) ListOutgoing(ctx context.Context, canSee func(feed string) bool) ([]Outgoing, error) {
	rows, err := s.cfg.DB.Query(ctx, `
SELECT c.feed_id::text,f.slug,f.format,c.consumer_kind,c.consumer_id,COALESCE(pr.subject,''),c.first_seen,c.last_seen,c.pulls,c.bytes_served,
 c.last_sequence,c.job_name,c.last_result,c.last_error_at,c.last_error,COALESCE(h.seq,0),clock_timestamp(),
 CASE WHEN c.last_sequence IS NULL OR COALESCE(h.seq,0)<=c.last_sequence THEN NULL
  ELSE COALESCE((SELECT created_at FROM replication_changes rc WHERE rc.instance_id=c.instance_id AND rc.seq=c.last_sequence+1),h.changed_at) END
FROM replication_consumers c
JOIN feeds f ON f.instance_id=c.instance_id AND f.id=c.feed_id
LEFT JOIN replication_heads h ON h.instance_id=c.instance_id
LEFT JOIN principals pr ON c.consumer_kind='principal' AND pr.instance_id=c.instance_id AND pr.id::text=c.consumer_id
WHERE c.instance_id=$1 ORDER BY c.last_seen DESC,f.slug,c.consumer_id LIMIT 1000`, s.cfg.InstanceID)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	var out []Outgoing
	var members *federation.Membership
	var topo *federation.TopologyDocument
	fed := s.fed.Load()
	if fed != nil {
		members = fed.Membership()
		topo, _ = fed.Topology(ctx)
	}
	for rows.Next() {
		var o Outgoing
		var lastResult, subject string
		var now time.Time
		var oldest *time.Time
		var consumerID string
		if err := rows.Scan(&o.FeedID, &o.FeedSlug, &o.FeedFormat, &o.ConsumerKind, &consumerID, &subject, &o.FirstSeen, &o.LastSeen, &o.Pulls, &o.BytesServed,
			&o.LastSequence, &o.RemoteJob, &lastResult, &o.LastErrorAt, &o.LastError, &o.HeadSequence, &now, &oldest); err != nil {
			return nil, ErrUnavailable
		}
		if canSee != nil && !canSee(o.FeedID) {
			continue
		}
		o.ID = o.FeedID + ":" + o.ConsumerKind + ":" + consumerID
		o.StaleAfterSec = int64(StaleAfter / time.Second)
		switch o.ConsumerKind {
		case ConsumerMember:
			o.MemberInstanceID = consumerID
			o.ConsumerName = memberLabel(members, consumerID)
		default:
			o.PrincipalID = consumerID
			o.ConsumerName = subject
			if o.ConsumerName == "" {
				o.ConsumerName = consumerID
			}
		}
		if o.LastSequence != nil && o.HeadSequence > *o.LastSequence {
			o.LagChanges = o.HeadSequence - *o.LastSequence
		}
		if oldest != nil && now.After(*oldest) {
			o.LagSeconds = int64(now.Sub(*oldest) / time.Second)
		}
		switch {
		case lastResult == "error":
			o.Status = "error"
		case now.Sub(o.LastSeen) > StaleAfter:
			o.Status = "stale"
		default:
			o.Status = "active"
		}
		if o.ConsumerKind == ConsumerMember && topo != nil {
			o.RuleID = s.ruleFor(ctx, topo, members, o.FeedID, consumerID)
		}
		o.History = []LagSample{}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrUnavailable
	}
	rows.Close()
	for i := range out {
		out[i].History = s.lagHistory(ctx, out[i])
	}
	if out == nil {
		out = []Outgoing{}
	}
	return out, nil
}

// ruleFor finds the federation rule that makes member a replica of the feed.
func (s *Service) ruleFor(ctx context.Context, topo *federation.TopologyDocument, members *federation.Membership, feed, member string) string {
	var key string
	if err := s.cfg.DB.QueryRow(ctx, `SELECT feed_key FROM federated_feed_bindings WHERE instance_id=$1 AND feed_id=$2`, s.cfg.InstanceID, feed).Scan(&key); err != nil {
		return ""
	}
	m, ok := members.Member(member)
	if !ok {
		return ""
	}
	return matchRule(&topo.TopologyBody, key, m)
}

// matchRule is the rule of a feed key that selects the member.
func matchRule(topo *federation.TopologyBody, key string, m federation.Member) string {
	for i := range topo.Rules {
		if topo.Rules[i].FeedKey == key && topo.Rules[i].Selects(m.InstanceID, m.Labels) {
			return topo.Rules[i].ID
		}
	}
	return ""
}

// memberLabel is the federation member name, or the instance id when unknown.
func memberLabel(members *federation.Membership, id string) string {
	if m, ok := members.Member(id); ok && m.Name != "" {
		return m.Name
	}
	return id
}

// lagHistory returns the most recent samples, oldest first.
func (s *Service) lagHistory(ctx context.Context, o Outgoing) []LagSample {
	kind, id, _ := strings.Cut(strings.TrimPrefix(o.ID, o.FeedID+":"), ":")
	rows, err := s.cfg.DB.Query(ctx, `SELECT bucket,last_sequence,head_sequence FROM (
 SELECT bucket,last_sequence,head_sequence FROM replication_consumer_samples WHERE instance_id=$1 AND feed_id=$2 AND consumer_kind=$3 AND consumer_id=$4 ORDER BY bucket DESC LIMIT 48) x ORDER BY bucket`,
		s.cfg.InstanceID, o.FeedID, kind, id)
	if err != nil {
		return []LagSample{}
	}
	defer rows.Close()
	out := []LagSample{}
	for rows.Next() {
		var l LagSample
		if rows.Scan(&l.At, &l.LastSequence, &l.HeadSequence) != nil {
			break
		}
		if l.HeadSequence > l.LastSequence {
			l.LagChanges = l.HeadSequence - l.LastSequence
		}
		out = append(out, l)
	}
	return out
}

// listOutgoing serves GET /api/v1/replication/outgoing. Like the jobs list it
// needs no extra grant beyond replicate or configure on each feed shown.
func (h handlers) listOutgoing(w http.ResponseWriter, r *http.Request) {
	a, ok := h.begin(w, r)
	if !ok {
		return
	}
	items, err := h.s.ListOutgoing(r.Context(), a.inspect)
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, struct {
		Items []Outgoing `json:"items"`
	}{items})
}
