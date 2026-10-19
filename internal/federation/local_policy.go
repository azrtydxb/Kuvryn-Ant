package federation

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Local administration of how the federation policy applies on this member:
// feed-key bindings, local settings (accept_admin_grants, the optional hard
// staleness limit), principal adoption and the sign-in hook. None of these
// need the authority; they re-materialize the applied policy locally.

// FeedBinding binds a federation feed key to a local feed.
type FeedBinding struct {
	FeedKey   string    `json:"feed_key"`
	FeedID    string    `json:"feed_id"`
	FeedSlug  string    `json:"feed_slug"`
	CreatedAt time.Time `json:"created_at"`
}

// FeedBindings lists this member's feed-key bindings.
func (s *Service) FeedBindings(ctx context.Context) ([]FeedBinding, error) {
	rows, err := s.cfg.DB.Query(ctx, `SELECT b.feed_key,b.feed_id::text,f.slug,b.created_at FROM federated_feed_bindings b JOIN feeds f ON (f.instance_id,f.id)=(b.instance_id,b.feed_id)
WHERE b.instance_id=$1 ORDER BY b.feed_key`, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FeedBinding{}
	for rows.Next() {
		var b FeedBinding
		if err := rows.Scan(&b.FeedKey, &b.FeedID, &b.FeedSlug, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BindFeedTx binds a feed key to a local feed and activates the policy grants
// that reference it.
func (s *Service) BindFeedTx(ctx context.Context, tx pgx.Tx, actor, key, feedID string) (FeedBinding, PolicyReport, error) {
	st, err := lockState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return FeedBinding{}, PolicyReport{}, err
	}
	if st.State != "active" || st.FederationID == "" {
		return FeedBinding{}, PolicyReport{}, ErrNotConfigured
	}
	if !ValidFeedKey(st.Name, key) {
		return FeedBinding{}, PolicyReport{}, invalid("feed_key must be " + st.Name + "/<feed-slug>")
	}
	var out FeedBinding
	var purpose string
	err = tx.QueryRow(ctx, `SELECT slug,purpose FROM feeds WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, feedID).Scan(&out.FeedSlug, &purpose)
	if errors.Is(err, pgx.ErrNoRows) {
		return FeedBinding{}, PolicyReport{}, ErrNotFound
	}
	if err != nil {
		return FeedBinding{}, PolicyReport{}, err
	}
	if purpose == "cache" {
		return FeedBinding{}, PolicyReport{}, invalid("cache feeds are never part of a federation: a feed with purpose cache cannot be bound to a federation feed key")
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT feed_id::text FROM federated_feed_bindings WHERE instance_id=$1 AND feed_key=$2`, s.cfg.InstanceID, key).Scan(&existing)
	switch {
	case err == nil && existing == feedID:
		// idempotent
	case err == nil:
		return FeedBinding{}, PolicyReport{}, ErrConflict
	case !errors.Is(err, pgx.ErrNoRows):
		return FeedBinding{}, PolicyReport{}, err
	default:
		var bound int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM federated_feed_bindings WHERE instance_id=$1 AND feed_id=$2`, s.cfg.InstanceID, feedID).Scan(&bound); err != nil {
			return FeedBinding{}, PolicyReport{}, err
		}
		if bound > 0 {
			return FeedBinding{}, PolicyReport{}, ErrConflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federated_feed_bindings(instance_id,feed_key,feed_id,created_by) VALUES($1,$2,$3,nullif($4,'')::uuid)`, s.cfg.InstanceID, key, feedID, uuidOrEmpty(actor)); err != nil {
			return FeedBinding{}, PolicyReport{}, err
		}
		if err := audit(ctx, tx, s.cfg.InstanceID, actor, "federation.feed.bound", feedID, "success", map[string]any{"feed_key": key}); err != nil {
			return FeedBinding{}, PolicyReport{}, err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT created_at FROM federated_feed_bindings WHERE instance_id=$1 AND feed_key=$2`, s.cfg.InstanceID, key).Scan(&out.CreatedAt); err != nil {
		return FeedBinding{}, PolicyReport{}, err
	}
	out.FeedKey, out.FeedID = key, feedID
	report, err := s.rematerialize(ctx, tx)
	return out, report, err
}

// UnbindFeedTx removes a binding; grants on the key become inert again.
func (s *Service) UnbindFeedTx(ctx context.Context, tx pgx.Tx, actor, key string) error {
	if _, err := lockState(ctx, tx, s.cfg.InstanceID); err != nil {
		return err
	}
	var feed string
	err := tx.QueryRow(ctx, `DELETE FROM federated_feed_bindings WHERE instance_id=$1 AND feed_key=$2 RETURNING feed_id::text`, s.cfg.InstanceID, key).Scan(&feed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := audit(ctx, tx, s.cfg.InstanceID, actor, "federation.feed.unbound", feed, "success", map[string]any{"feed_key": key}); err != nil {
		return err
	}
	_, err = s.rematerialize(ctx, tx)
	return err
}

// ErrConflict reports a binding that conflicts with an existing one.
var ErrConflict = errors.New("federation: conflicting binding")

// Settings are local federation settings; the federation cannot set them.
type Settings struct {
	AcceptAdminGrants   bool   `json:"accept_admin_grants"`
	PolicyMaxAgeSeconds *int32 `json:"policy_max_age_seconds"`
}

// Settings returns the local federation settings.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	var out Settings
	err := s.cfg.DB.QueryRow(ctx, `SELECT federation_accept_admin_grants,federation_policy_max_age_seconds FROM instance_settings WHERE instance_id=$1`, s.cfg.InstanceID).
		Scan(&out.AcceptAdminGrants, &out.PolicyMaxAgeSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, nil
	}
	return out, err
}

// SetSettingsTx stores the local settings and re-materializes the policy.
func (s *Service) SetSettingsTx(ctx context.Context, tx pgx.Tx, actor string, in Settings) (Settings, error) {
	if in.PolicyMaxAgeSeconds != nil && (*in.PolicyMaxAgeSeconds < 300 || *in.PolicyMaxAgeSeconds > 31536000) {
		return Settings{}, invalid("policy_max_age_seconds must be null (off) or between 300 and 31536000")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO instance_settings(instance_id,federation_accept_admin_grants,federation_policy_max_age_seconds) VALUES($1,$2,$3)
ON CONFLICT(instance_id) DO UPDATE SET federation_accept_admin_grants=EXCLUDED.federation_accept_admin_grants,federation_policy_max_age_seconds=EXCLUDED.federation_policy_max_age_seconds,
revision=instance_settings.revision+1,updated_at=clock_timestamp()`, s.cfg.InstanceID, in.AcceptAdminGrants, in.PolicyMaxAgeSeconds); err != nil {
		return Settings{}, err
	}
	if err := audit(ctx, tx, s.cfg.InstanceID, actor, "federation.settings.updated", "", "success", map[string]any{"accept_admin_grants": in.AcceptAdminGrants, "policy_max_age_seconds": in.PolicyMaxAgeSeconds}); err != nil {
		return Settings{}, err
	}
	if _, err := s.rematerialize(ctx, tx); err != nil {
		return Settings{}, err
	}
	return in, nil
}

// FederatedPrincipal is a principal row managed by the federation, or a
// local principal that conflicts with a federated binding.
type FederatedPrincipal struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Issuer      string   `json:"issuer"`
	Subject     string   `json:"subject"`
	Origin      string   `json:"origin"`
	FederatedID string   `json:"federated_id,omitempty"`
	Disabled    bool     `json:"disabled"`
	Roles       []string `json:"roles"`
	LocalRoles  []string `json:"local_roles"`
	Conflict    string   `json:"conflict,omitempty"`
}

// Principals lists federated principals and conflicting local principals.
func (s *Service) Principals(ctx context.Context) ([]FederatedPrincipal, error) {
	rows, err := s.cfg.DB.Query(ctx, `SELECT p.id::text,p.kind,p.issuer,p.subject,p.origin,coalesce(p.federated_id::text,''),p.disabled,
ARRAY(SELECT r.name FROM principal_roles pr JOIN roles r ON (r.instance_id,r.id)=(pr.instance_id,pr.role_id) WHERE pr.instance_id=p.instance_id AND pr.principal_id=p.id AND pr.origin='federation' ORDER BY r.name),
ARRAY(SELECT r.name FROM principal_roles pr JOIN roles r ON (r.instance_id,r.id)=(pr.instance_id,pr.role_id) WHERE pr.instance_id=p.instance_id AND pr.principal_id=p.id AND pr.origin='local' ORDER BY r.name)
FROM principals p WHERE p.instance_id=$1 AND p.origin='federation' ORDER BY p.issuer,p.subject LIMIT 5000`, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederatedPrincipal{}
	for rows.Next() {
		var p FederatedPrincipal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Issuer, &p.Subject, &p.Origin, &p.FederatedID, &p.Disabled, &p.Roles, &p.LocalRoles); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	for _, c := range ps.Report.Conflicts {
		out = append(out, FederatedPrincipal{ID: c.PrincipalID, Kind: "user", Subject: c.Subject, Origin: "local", Roles: []string{}, LocalRoles: []string{}, Conflict: c.Reason, Issuer: c.Domain})
	}
	return out, nil
}

// AdoptPrincipalTx resolves conflict_local_principal (rule 5): the local
// principal that owns a federated identity becomes federation-managed. Its ID,
// tokens and local memberships are kept.
func (s *Service) AdoptPrincipalTx(ctx context.Context, tx pgx.Tx, actor, id string) (PolicyReport, error) {
	st, err := lockState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return PolicyReport{}, err
	}
	if st.State != "active" {
		return PolicyReport{}, ErrNotConfigured
	}
	ps, err := loadPolicyState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return PolicyReport{}, err
	}
	doc, err := loadPolicy(ctx, tx, s.cfg.InstanceID, st.FederationID, ps.Generation)
	if err != nil {
		return PolicyReport{}, err
	}
	var conflict *PrincipalConflict
	for _, c := range ps.Report.Conflicts {
		if c.PrincipalID == id {
			conflict = &c
			break
		}
	}
	if conflict == nil || doc == nil {
		return PolicyReport{}, ErrNotFound
	}
	d, ok := doc.Domain(conflict.Domain)
	if !ok {
		return PolicyReport{}, ErrNotFound
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM principals WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.cfg.InstanceID, id).Scan(&kind); err != nil {
		return PolicyReport{}, err
	}
	if kind != "user" {
		return PolicyReport{}, invalid("only user principals can be adopted")
	}
	if err := enableApply(ctx, tx); err != nil {
		return PolicyReport{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE principals SET origin='federation',issuer=$3,subject=$4,federated_id=$5,revision=revision+1 WHERE instance_id=$1 AND id=$2`,
		s.cfg.InstanceID, id, d.Issuer, conflict.Subject, FederatedID(st.FederationID, d.Issuer, conflict.Subject)); err != nil {
		return PolicyReport{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.cfg.InstanceID, d.Issuer, conflict.Subject, id); err != nil {
		return PolicyReport{}, err
	}
	if err := audit(ctx, tx, s.cfg.InstanceID, actor, "federation.principal.adopted", id, "success", map[string]any{"domain": d.ID, "subject": conflict.Subject}); err != nil {
		return PolicyReport{}, err
	}
	return s.rematerialize(ctx, tx)
}

// PolicyView is GET /api/v1/federation/policy.
type PolicyView struct {
	Status   *PolicyStatus   `json:"status"`
	Document *PolicyDocument `json:"document,omitempty"`
	HasDraft bool            `json:"has_draft"`
	Settings Settings        `json:"settings"`
	History  []PolicyHistory `json:"history"`
}

// PolicyHistory is one generation of the local policy log.
type PolicyHistory struct {
	Generation int64     `json:"generation"`
	Hash       string    `json:"hash"`
	IssuedAt   string    `json:"issued_at"`
	IssuedBy   string    `json:"issued_by"`
	ReceivedAt time.Time `json:"received_at"`
}

// Policy returns the applied policy, status, settings and recent history.
func (s *Service) Policy(ctx context.Context) (PolicyView, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return PolicyView{}, err
	}
	if st.FederationID == "" || st.State != "active" {
		return PolicyView{}, ErrNotConfigured
	}
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return PolicyView{}, err
	}
	out := PolicyView{Status: s.policyStatus(st, ps), History: []PolicyHistory{}}
	if out.Document, err = loadPolicy(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, ps.Generation); err != nil {
		return PolicyView{}, err
	}
	if out.Settings, err = s.Settings(ctx); err != nil {
		return PolicyView{}, err
	}
	if err := s.cfg.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM federation_policy_drafts WHERE instance_id=$1 AND federation_id=$2)`, s.cfg.InstanceID, st.FederationID).Scan(&out.HasDraft); err != nil {
		return PolicyView{}, err
	}
	rows, err := s.cfg.DB.Query(ctx, `SELECT generation,hash,body->>'issued_at',body->>'issued_by',received_at FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='policy' ORDER BY generation DESC LIMIT 50`, s.cfg.InstanceID, st.FederationID)
	if err != nil {
		return PolicyView{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var h PolicyHistory
		if err := rows.Scan(&h.Generation, &h.Hash, &h.IssuedAt, &h.IssuedBy, &h.ReceivedAt); err != nil {
			return PolicyView{}, err
		}
		out.History = append(out.History, h)
	}
	return out, rows.Err()
}

// Diff compares two policy generations; to=0 is the applied generation,
// from=-1 is the generation before to, and toDraft compares against the draft.
func (s *Service) Diff(ctx context.Context, from, to int64, toDraft bool) (PolicyDiff, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return PolicyDiff{}, err
	}
	if st.FederationID == "" {
		return PolicyDiff{}, ErrNotConfigured
	}
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return PolicyDiff{}, err
	}
	var target *PolicyBody
	if toDraft {
		d, err := s.Draft(ctx)
		if err != nil {
			return PolicyDiff{}, err
		}
		target, to = &d.Body, ps.Generation+1
		if from < 0 {
			from = ps.Generation
		}
	} else {
		if to == 0 {
			to = ps.Generation
		}
		if to < 1 || to > ps.Generation {
			return PolicyDiff{}, ErrNotFound
		}
		doc, err := loadPolicy(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, to)
		if errors.Is(err, pgx.ErrNoRows) {
			return PolicyDiff{}, ErrNotFound
		}
		if err != nil {
			return PolicyDiff{}, err
		}
		target = &doc.PolicyBody
		if from < 0 {
			from = to - 1
		}
	}
	var base *PolicyBody
	if from > 0 {
		doc, err := loadPolicy(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, from)
		if errors.Is(err, pgx.ErrNoRows) {
			return PolicyDiff{}, ErrNotFound
		}
		if err != nil {
			return PolicyDiff{}, err
		}
		base = &doc.PolicyBody
	}
	d := DiffPolicy(base, target)
	d.FromGeneration, d.ToGeneration = max(from, 0), to
	return d, nil
}

// --- sign-in hook ---

// ExternalLogin runs before a browser session is issued for an external
// identity (OIDC, SAML, LDAP). When the issuer is a federation identity
// domain it materializes the federated principal (if the identity is bound
// directly or through one of its groups), records the observed groups and
// re-evaluates the principal's federation roles. A local principal owning the
// identity is never taken over. Errors only skip federation; local sign-in
// continues unchanged.
func (s *Service) ExternalLogin(ctx context.Context, issuer, subject string, groups []string) error {
	policy, _, err := s.currentPolicy(ctx, s.cfg.DB)
	if errors.Is(err, ErrNotConfigured) || policy == nil {
		return nil
	}
	if err != nil {
		return err
	}
	d, ok := policy.DomainByIssuer(issuer)
	if !ok || !cleanText(subject, 512) {
		return nil
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, g := range groups {
		if cleanText(g, 512) && !seen[g] && len(clean) < 500 {
			seen[g] = true
			clean = append(clean, g)
		}
	}
	sort.Strings(clean)
	relevant := false
	for _, b := range policy.PrincipalBindings {
		if b.Domain == d.ID && b.Subject == subject {
			relevant = true
		}
	}
	for _, g := range policy.GroupBindings {
		if g.Domain == d.ID && seen[g.GroupID] {
			relevant = true
		}
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil || st.State != "active" {
			return err
		}
		id, origin, err := s.lookupIdentity(ctx, tx, d.Issuer, subject)
		if err != nil || (id != "" && origin != "federation") || (id == "" && !relevant) {
			return err
		}
		if err := enableApply(ctx, tx); err != nil {
			return err
		}
		if id == "" {
			var conflict *PrincipalConflict
			if id, conflict, err = s.ensureFederatedUser(ctx, tx, st.FederationID, d, subject); err != nil || conflict != nil {
				return err
			}
			if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.principal.materialized", id, "success", map[string]any{"domain": d.ID, "subject": subject}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM federation_principal_groups WHERE instance_id=$1 AND principal_id=$2 AND domain=$3`, s.cfg.InstanceID, id, d.ID); err != nil {
			return err
		}
		for _, g := range clean {
			if _, err := tx.Exec(ctx, `INSERT INTO federation_principal_groups(instance_id,principal_id,domain,group_id) VALUES($1,$2,$3,$4)`, s.cfg.InstanceID, id, d.ID, g); err != nil {
				return err
			}
		}
		roleIDs, err := federationRoleIDs(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM principal_roles WHERE instance_id=$1 AND principal_id=$2 AND origin='federation'`, s.cfg.InstanceID, id); err != nil {
			return err
		}
		var p fedPrincipal
		if err := tx.QueryRow(ctx, `SELECT id::text,kind,issuer,subject,disabled FROM principals WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, id).Scan(&p.ID, &p.Kind, &p.Issuer, &p.Subject, &p.Disabled); err != nil {
			return err
		}
		if err := s.writeMemberships(ctx, tx, st.FederationID, &policy.PolicyBody, roleIDs, []fedPrincipal{p}); err != nil {
			return err
		}
		return disableApply(ctx, tx)
	})
}
