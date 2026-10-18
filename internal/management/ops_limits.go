package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/limits"
	"github.com/jackc/pgx/v5"
)

// LimitsConfig is the stored rate-limit and quota document.
type LimitsConfig struct {
	Revision   int64              `json:"revision"`
	RateLimits []limits.RateRule  `json:"rate_limits"`
	Quotas     []limits.QuotaRule `json:"quotas"`
	UpdatedAt  *time.Time         `json:"updated_at,omitempty"`
}

// PutLimits replaces the whole document.
type PutLimits struct {
	RateLimits []limits.RateRule  `json:"rate_limits"`
	Quotas     []limits.QuotaRule `json:"quotas"`
}

// QuotaUsage is the measured usage of one configured quota.
type QuotaUsage struct {
	Scope        string `json:"scope"`
	FeedID       string `json:"feed_id"`
	Namespace    string `json:"namespace,omitempty"`
	BytesUsed    int64  `json:"bytes_used"`
	PackagesUsed int64  `json:"packages_used"`
	MaxBytes     *int64 `json:"max_bytes,omitempty"`
	MaxPackages  *int64 `json:"max_packages,omitempty"`
}

// LimitsUsage reports quota usage and per-process throttle counters.
type LimitsUsage struct {
	Quotas     []QuotaUsage     `json:"quotas"`
	Throttled  limits.Throttles `json:"throttled"`
	MeasuredAt time.Time        `json:"measured_at"`
}

func (h opsHandlers) loadLimits(ctx context.Context, instance string) (LimitsConfig, error) {
	out := LimitsConfig{Revision: 1, RateLimits: []limits.RateRule{}, Quotas: []limits.QuotaRule{}}
	var rates, quotas []byte
	var updated time.Time
	err := h.db.QueryRow(ctx, `SELECT rate_limits,quotas,revision,updated_at FROM limits_config WHERE instance_id=$1`, instance).Scan(&rates, &quotas, &out.Revision, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.UpdatedAt = &updated
	if err = json.Unmarshal(rates, &out.RateLimits); err != nil {
		return out, err
	}
	return out, json.Unmarshal(quotas, &out.Quotas)
}

func (h opsHandlers) getLimits(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	cfg, err := h.loadLimits(ctx, p.InstanceID)
	if err != nil {
		identityError(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(cfg.Revision, 10)))
	JSON(w, 200, cfg)
}

// referencedExists verifies that every ID exists in the instance's table.
func referencedExists(ctx context.Context, tx pgx.Tx, query, instance string, ids map[string]bool) (string, error) {
	for id := range ids {
		var found bool
		if err := tx.QueryRow(ctx, query, instance, id).Scan(&found); err != nil {
			return "", err
		}
		if !found {
			return id, nil
		}
	}
	return "", nil
}

func (h opsHandlers) putLimits(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	var in PutLimits
	if !sourceJSON(w, r, &in, "rate_limits", "quotas") {
		return
	}
	norm, err := limits.Normalize(limits.Config{RateLimits: in.RateLimits, Quotas: in.Quotas})
	if err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid limits: "+err.Error())
		return
	}
	h.mutateCfg(w, r, p, "limits.put", "limits", struct {
		In       limits.Config
		Revision int64
	}{norm, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		feeds, principals := map[string]bool{}, map[string]bool{}
		for _, rule := range norm.RateLimits {
			switch rule.Scope {
			case limits.ScopeFeed, limits.ScopeNamespace:
				feeds[rule.Subject] = true
			case limits.ScopePrincipal:
				principals[rule.Subject] = true
			}
		}
		for _, q := range norm.Quotas {
			feeds[q.FeedID] = true
		}
		if id, err := referencedExists(ctx, tx, `SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND id=$2)`, p.InstanceID, feeds); err != nil {
			return nil, err
		} else if id != "" {
			return nil, invalidConfig("limits reference unknown feed " + id)
		}
		if id, err := referencedExists(ctx, tx, `SELECT EXISTS(SELECT 1 FROM principals WHERE instance_id=$1 AND id=$2)`, p.InstanceID, principals); err != nil {
			return nil, err
		} else if id != "" {
			return nil, invalidConfig("limits reference unknown principal " + id)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO limits_config(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, p.InstanceID); err != nil {
			return nil, err
		}
		var current int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM limits_config WHERE instance_id=$1 FOR UPDATE`, p.InstanceID).Scan(&current); err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		rates, _ := json.Marshal(norm.RateLimits)
		quotas, _ := json.Marshal(norm.Quotas)
		out := LimitsConfig{RateLimits: norm.RateLimits, Quotas: norm.Quotas}
		var updated time.Time
		if err := tx.QueryRow(ctx, `UPDATE limits_config SET rate_limits=$2,quotas=$3,revision=revision+1,updated_at=now() WHERE instance_id=$1 RETURNING revision,updated_at`, p.InstanceID, rates, quotas).Scan(&out.Revision, &updated); err != nil {
			return nil, err
		}
		out.UpdatedAt = &updated
		return out, audited(ctx, tx, p, p.InstanceID, "limits.put", out.Revision, map[string]int{"rate_limits": len(norm.RateLimits), "quotas": len(norm.Quotas)})
	})
	if h.d.Limits != nil {
		// Apply immediately on this instance; other instances poll.
		ctx, cancel := h.ctx(r)
		defer cancel()
		_ = h.d.Limits.Refresh(ctx)
	}
}

func (h opsHandlers) getLimitsUsage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	cfg, err := h.loadLimits(ctx, p.InstanceID)
	if err != nil {
		identityError(w, r, err)
		return
	}
	out := LimitsUsage{Quotas: []QuotaUsage{}, Throttled: limits.Default.Throttled(), MeasuredAt: time.Now().UTC()}
	for _, q := range cfg.Quotas {
		bytes, packages, err := limits.Usage(ctx, h.db, p.InstanceID, q.FeedID, q.Namespace, q.Scope == limits.ScopeFeed, "")
		if err != nil {
			identityError(w, r, err)
			return
		}
		limits.Default.ObserveUsage(q, bytes, packages)
		out.Quotas = append(out.Quotas, QuotaUsage{Scope: q.Scope, FeedID: q.FeedID, Namespace: q.Namespace, BytesUsed: bytes, PackagesUsed: packages, MaxBytes: q.MaxBytes, MaxPackages: q.MaxPackages})
	}
	JSON(w, 200, out)
}
