package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Provenance describes where an asset came from. Uploader, token and policy
// fields are shown only to callers who can configure the feed (or administer
// the instance); the verification result and counts need only read access.
type Provenance struct {
	AssetID    string `json:"asset_id"`
	Coordinate string `json:"coordinate"`
	Version    string `json:"version"`
	AssetKey   string `json:"asset_key"`
	SourceID   string `json:"source_id"`
	FeedID     string `json:"feed_id,omitempty"`
	// Origin is local (published), remote (pull-through cache) or replica.
	Origin             string     `json:"origin"`
	CreatedAt          time.Time  `json:"created_at"`
	UploaderID         string     `json:"uploader_id,omitempty"`
	UploaderSubject    string     `json:"uploader_subject,omitempty"`
	TokenID            string     `json:"token_id,omitempty"`
	MatchedRule        string     `json:"matched_rule,omitempty"`
	Digest             string     `json:"digest"`
	Verification       string     `json:"verification"`
	VerificationDetail string     `json:"verification_detail"`
	Downloads          int64      `json:"downloads"`
	LastDownloadedAt   *time.Time `json:"last_downloaded_at,omitempty"`
}

func (h opsHandlers) provenance(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid asset ID")
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	var out Provenance
	var namespace string
	var uploader, token, rule, feed, origin, subject, finding *string
	err := h.db.QueryRow(ctx, `SELECT a.id::text,c.coordinate,COALESCE(c.namespace,''),v.version,a.asset_key,c.source_id::text,a.created_at,
 a.provenance->>'feed_id',a.provenance->>'principal_id',a.provenance->>'token_id',a.provenance->>'matched_rule',
 CASE WHEN a.provenance->>'origin' IS NOT NULL THEN a.provenance->>'origin' WHEN a.provenance ? 'replication_job' THEN 'replica' ELSE 'local' END,
 pr.subject,b.digest,
 (SELECT f.kind FROM blob_integrity_findings f WHERE f.instance_id=a.instance_id AND f.dedup_domain_id=b.dedup_domain_id AND f.digest=b.digest AND f.resolved_at IS NULL ORDER BY f.id LIMIT 1),
 COALESCE(d.downloads,0),d.last_downloaded_at
 FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN blobs b ON b.id=a.blob_id
 LEFT JOIN principals pr ON pr.instance_id=a.instance_id AND pr.id=CASE WHEN a.provenance->>'principal_id' ~ '^[0-9a-f-]{36}$' THEN (a.provenance->>'principal_id')::uuid END
 LEFT JOIN asset_downloads d ON d.asset_id=a.id
 WHERE a.instance_id=$1 AND a.id=$2`, p.InstanceID, id).Scan(&out.AssetID, &out.Coordinate, &namespace, &out.Version, &out.AssetKey, &out.SourceID, &out.CreatedAt, &feed, &uploader, &token, &rule, &origin, &subject, &out.Digest, &finding, &out.Downloads, &out.LastDownloadedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		Error(w, r, 404, api.ErrorCodeNotFound, "asset not found")
		return
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "provenance unavailable")
		return
	}
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	out.FeedID = str(feed)
	resource := identity.Resource{InstanceID: p.InstanceID, FeedID: out.FeedID, SourceID: out.SourceID, Namespace: namespace, Kind: identity.ArtifactResource}
	if !policy.Authorize(p, resource, identity.Read) {
		Error(w, r, 404, api.ErrorCodeNotFound, "asset not found")
		return
	}
	out.Origin = str(origin)
	out.Digest = "sha256:" + out.Digest
	if finding != nil {
		out.Verification, out.VerificationDetail = "integrity_failure", "open integrity finding: "+*finding
	} else {
		out.Verification, out.VerificationDetail = "verified", "sha256 verified at ingest; no open integrity findings"
	}
	admin := policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin)
	if admin || out.FeedID != "" && feedAllowed(policy, p, out.FeedID, identity.Configure) {
		out.UploaderID, out.UploaderSubject, out.TokenID, out.MatchedRule = str(uploader), str(subject), str(token), str(rule)
	}
	JSON(w, 200, out)
}

// WebhookRate is a delivery aggregate over the trailing seven days. SuccessRate
// is absent until at least one delivery has finished (delivered or dead).
type WebhookRate struct {
	Delivered   int64    `json:"delivered"`
	Failed      int64    `json:"failed"`
	Pending     int64    `json:"pending"`
	SuccessRate *float64 `json:"success_rate,omitempty"`
}
type WebhookStats struct {
	WebhookID string `json:"webhook_id"`
	Endpoint  string `json:"endpoint"`
	WebhookRate
}
type WebhookStatsPage struct {
	WindowDays int            `json:"window_days"`
	Overall    WebhookRate    `json:"overall"`
	Items      []WebhookStats `json:"items"`
	Next       string         `json:"next_after,omitempty"`
}

func rate(d, f, p int64) WebhookRate {
	out := WebhookRate{Delivered: d, Failed: f, Pending: p}
	if d+f > 0 {
		v := float64(d) / float64(d+f)
		out.SuccessRate = &v
	}
	return out
}

func (h opsHandlers) webhookStats(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	after, limit, ok := identityPage(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	out := WebhookStatsPage{WindowDays: 7, Items: []WebhookStats{}}
	var d, f, pe int64
	err := h.db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE d.state='delivered'),count(*) FILTER (WHERE d.state='dead'),count(*) FILTER (WHERE d.state='pending')
 FROM event_deliveries d JOIN event_subscriptions s ON s.id=d.subscription_id JOIN outbox_events e ON e.id=d.event_id WHERE s.instance_id=$1 AND e.created_at>=now()-interval '7 days'`, p.InstanceID).Scan(&d, &f, &pe)
	if err != nil {
		identityError(w, r, err)
		return
	}
	out.Overall = rate(d, f, pe)
	rows, err := h.db.Query(ctx, `SELECT s.id::text,s.endpoint,
 count(d.event_id) FILTER (WHERE d.state='delivered'),count(d.event_id) FILTER (WHERE d.state='dead'),count(d.event_id) FILTER (WHERE d.state='pending')
 FROM event_subscriptions s LEFT JOIN (event_deliveries d JOIN outbox_events e ON e.id=d.event_id AND e.created_at>=now()-interval '7 days') ON d.subscription_id=s.id
 WHERE s.instance_id=$1 AND ($2='' OR s.id>NULLIF($2,'')::uuid) GROUP BY s.id,s.endpoint ORDER BY s.id LIMIT $3`, p.InstanceID, after, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var it WebhookStats
		if err = rows.Scan(&it.WebhookID, &it.Endpoint, &d, &f, &pe); err != nil {
			identityError(w, r, err)
			return
		}
		it.WebhookRate = rate(d, f, pe)
		out.Items = append(out.Items, it)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = out.Items[limit-1].WebhookID
	}
	JSON(w, 200, out)
}

// redeliver serves POST /api/v1/webhooks/{id}/deliveries/{event}:redeliver.
func (h opsHandlers) redeliver(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	sub, valid := feedID(r.PathValue("id"))
	raw, verb, found := strings.Cut(r.PathValue("event"), ":")
	event, err := strconv.ParseInt(raw, 10, 64)
	if !valid || !found || verb != "redeliver" || err != nil || event < 1 {
		Error(w, r, 404, api.ErrorCodeNotFound, "management route not found")
		return
	}
	h.mutate(w, r, p, "webhook.redeliver", sub+"/"+raw, struct{ Subscription, Event string }{sub, raw}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var item WebhookDelivery
		var state string
		// Only finished deliveries may be reset; a pending or leased attempt is already in flight.
		err := tx.QueryRow(ctx, `SELECT d.state FROM event_deliveries d JOIN event_subscriptions s ON s.id=d.subscription_id WHERE s.instance_id=$1 AND s.id=$2 AND d.event_id=$3 FOR UPDATE OF d`, p.InstanceID, sub, event).Scan(&state)
		if err != nil {
			return nil, err
		}
		if state == "pending" {
			return nil, database.ErrConflict
		}
		err = tx.QueryRow(ctx, `UPDATE event_deliveries SET state='pending',attempts=0,available_at=clock_timestamp(),lease_token=NULL,lease_until=NULL,last_failure=NULL,delivered_at=NULL WHERE subscription_id=$1 AND event_id=$2 RETURNING event_id,state,attempts,available_at`, sub, event).Scan(&item.EventID, &item.State, &item.Attempts, &item.AvailableAt)
		if err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, sub, "webhook.redeliver", 0)
	})
}
