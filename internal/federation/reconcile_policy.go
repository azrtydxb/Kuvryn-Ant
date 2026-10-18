package federation

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Policy materialization (§6.3, §6.4). The applied policy document is turned
// into local rows in one transaction with ant.federation_apply=on:
//
//   - revocations first: federated principals disabled, their tokens and
//     sessions revoked;
//   - roles with origin federation (names are the fed: keys);
//   - grants resolved through federated_feed_bindings; an unbound feed key is
//     stored inert and never matched; federated instance-admin allows and
//     artifact/feed allows without a feed key (every feed) are inert
//     unless the member accepts admin grants locally (rule 4);
//   - federated service accounts and bound principals materialized (never over
//     a local principal with the same identity: rule 5, reported as a
//     conflict until adopted);
//   - federation memberships = principal bindings ∪ service-account roles ∪
//     group bindings over the groups observed at the principal's last sign-in.
//
// Local roles, local memberships and local grants are never touched, so a
// local deny keeps winning over a federated allow (rule 1) and local allows
// add to federated ones (rule 3); a federated deny wins over a local allow
// because the evaluator itself is unchanged (rule 2). Materialization is a
// pure function of the applied document and local bindings/settings, so
// re-applying is idempotent and any ordered prefix of documents ends in the
// state of its last document (rule 6, property-tested).

// PolicyReport summarizes the last materialization for status and the console.
type PolicyReport struct {
	Roles              int                 `json:"roles"`
	Grants             int                 `json:"grants"`
	Principals         int                 `json:"principals"`
	Revoked            int                 `json:"revoked"`
	UnresolvedFeedKeys []string            `json:"unresolved_feed_keys"`
	InertAdminGrants   int                 `json:"inert_admin_grants"`
	Conflicts          []PrincipalConflict `json:"conflicts"`
}

// PrincipalConflict is a binding that matches a local principal (rule 5).
type PrincipalConflict struct {
	Domain      string `json:"domain"`
	Subject     string `json:"subject"`
	PrincipalID string `json:"principal_id"`
	Reason      string `json:"reason"` // conflict_local_principal
}

// grantRow is one planned role_grants row.
type grantRow struct {
	RoleKey   string
	Kind      string
	Effect    string
	FeedID    string
	FeedKey   string
	Namespace string
	Actions   []string
	Inert     string
}

// planGrants resolves the policy grants against local feed bindings and the
// local accept_admin_grants setting. It is pure (property-tested).
func planGrants(body *PolicyBody, bindings map[string]string, acceptAdmin bool) ([]grantRow, []string, int) {
	var rows []grantRow
	unresolved := map[string]bool{}
	inertAdmin := 0
	for _, r := range body.Roles {
		for _, g := range r.Grants {
			row := grantRow{RoleKey: r.Key, Kind: g.Kind, Effect: g.Effect, FeedKey: g.FeedKey, Namespace: g.Namespace, Actions: append([]string(nil), g.Actions...)}
			if g.FeedKey != "" {
				if id, ok := bindings[g.FeedKey]; ok {
					row.FeedID = id
				} else {
					row.Inert = "unresolved_feed"
					unresolved[g.FeedKey] = true
				}
			} else if g.Kind != "instance" && g.Effect == "allow" && !acceptAdmin {
				// An artifact or feed allow without a feed key selects every
				// feed of the member, including feeds that are not federated:
				// gated like instance admin (rule 4). Denies only restrict.
				inertAdmin++
				row.Inert = "wildcard_not_accepted"
				rows = append(rows, row)
				continue
			}
			// Rule 4: a federated instance-admin allow is inert unless accepted
			// locally. Denies stay active: they can only restrict.
			if g.Kind == "instance" && g.Effect == "allow" && !acceptAdmin && contains(g.Actions, "admin") {
				inertAdmin++
				rest := []string{}
				for _, a := range g.Actions {
					if a != "admin" {
						rest = append(rest, a)
					}
				}
				admin := row
				admin.Actions, admin.Inert = []string{"admin"}, "admin_not_accepted"
				rows = append(rows, admin)
				if len(rest) == 0 {
					continue
				}
				row.Actions = rest
			}
			rows = append(rows, row)
		}
	}
	keys := make([]string, 0, len(unresolved))
	for k := range unresolved {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return rows, keys, inertAdmin
}

// desiredRoles computes the federation role keys of one principal.
func desiredRoles(body *PolicyBody, domain, subject string, serviceAccount string, groups map[string]bool) []string {
	set := map[string]bool{}
	if serviceAccount != "" {
		for _, s := range body.ServiceAccounts {
			if s.Name == serviceAccount {
				for _, r := range s.Roles {
					set[r] = true
				}
			}
		}
	} else {
		for _, b := range body.PrincipalBindings {
			if b.Domain == domain && b.Subject == subject {
				for _, r := range b.Roles {
					set[r] = true
				}
			}
		}
		for _, g := range body.GroupBindings {
			if g.Domain == domain && groups[g.GroupID] {
				for _, r := range g.Roles {
					set[r] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// revokedSet returns revoked identities keyed by issuer + "\n" + subject.
func revokedSet(fid string, body *PolicyBody) map[string]bool {
	out := map[string]bool{}
	for _, r := range body.Revocations {
		if r.ServiceAccount != "" {
			out[ServiceAccountIssuer(fid)+"\n"+r.ServiceAccount] = true
		} else if d, ok := body.Domain(r.Domain); ok {
			out[d.Issuer+"\n"+r.Subject] = true
		}
	}
	return out
}

func enableApply(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT set_config('ant.federation_apply','on',true)`)
	return err
}

func disableApply(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT set_config('ant.federation_apply','off',true)`)
	return err
}

type fedPrincipal struct {
	ID, Kind, Issuer, Subject string
	Disabled                  bool
}

func (s *Service) acceptAdmin(ctx context.Context, q queryer) (bool, error) {
	var accept bool
	err := q.QueryRow(ctx, `SELECT federation_accept_admin_grants FROM instance_settings WHERE instance_id=$1`, s.cfg.InstanceID).Scan(&accept)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return accept, err
}

func (s *Service) feedBindings(ctx context.Context, q queryer) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT feed_key,feed_id::text FROM federated_feed_bindings WHERE instance_id=$1`, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			return nil, err
		}
		out[k] = id
	}
	return out, rows.Err()
}

// lookupIdentity finds the principal that owns an external identity, either
// directly (issuer, subject) or through an existing identity binding.
func (s *Service) lookupIdentity(ctx context.Context, tx pgx.Tx, issuer, subject string) (id, origin string, err error) {
	err = tx.QueryRow(ctx, `SELECT p.id::text,p.origin FROM principals p WHERE p.instance_id=$1 AND p.issuer=$2 AND p.subject=$3
UNION ALL SELECT p.id::text,p.origin FROM oidc_identity_bindings b JOIN principals p ON (p.instance_id,p.id)=(b.instance_id,b.principal_id)
WHERE b.instance_id=$1 AND b.issuer=$2 AND b.subject=$3 LIMIT 1`, s.cfg.InstanceID, issuer, subject).Scan(&id, &origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return id, origin, err
}

// ensureFederatedUser materializes a federated user principal and its
// identity binding. It returns "" and a conflict when a local principal
// already owns the identity.
func (s *Service) ensureFederatedUser(ctx context.Context, tx pgx.Tx, fid string, d IdentityDomain, subject string) (string, *PrincipalConflict, error) {
	id, origin, err := s.lookupIdentity(ctx, tx, d.Issuer, subject)
	if err != nil {
		return "", nil, err
	}
	if id != "" && origin != "federation" {
		return "", &PrincipalConflict{Domain: d.ID, Subject: subject, PrincipalID: id, Reason: "conflict_local_principal"}, nil
	}
	if id == "" {
		if err := tx.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer,origin,federated_id) VALUES($1,'user',$2,$3,'federation',$4) RETURNING id::text`,
			s.cfg.InstanceID, subject, d.Issuer, FederatedID(fid, d.Issuer, subject)).Scan(&id); err != nil {
			return "", nil, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.cfg.InstanceID, d.Issuer, subject, id); err != nil {
		return "", nil, err
	}
	return id, nil, nil
}

// materialize applies a policy body to the local identity tables. The caller
// holds the federation state lock and commits.
func (s *Service) materialize(ctx context.Context, tx pgx.Tx, fid string, doc *PolicyDocument) (PolicyReport, error) {
	report := PolicyReport{UnresolvedFeedKeys: []string{}, Conflicts: []PrincipalConflict{}}
	if err := enableApply(ctx, tx); err != nil {
		return report, err
	}
	body := &doc.PolicyBody
	bindings, err := s.feedBindings(ctx, tx)
	if err != nil {
		return report, err
	}
	accept, err := s.acceptAdmin(ctx, tx)
	if err != nil {
		return report, err
	}

	// 1. Service accounts and bound principals exist before revocations are
	// applied, so a revocation in the same document disables them at once.
	saIssuer := ServiceAccountIssuer(fid)
	for _, sa := range body.ServiceAccounts {
		if _, err := tx.Exec(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer,origin,federated_id) VALUES($1,'service',$2,$3,'federation',$4) ON CONFLICT(instance_id,issuer,subject) DO NOTHING`,
			s.cfg.InstanceID, sa.Name, saIssuer, FederatedID(fid, saIssuer, sa.Name)); err != nil {
			return report, err
		}
	}
	for _, b := range body.PrincipalBindings {
		d, _ := body.Domain(b.Domain)
		_, conflict, err := s.ensureFederatedUser(ctx, tx, fid, d, b.Subject)
		if err != nil {
			return report, err
		}
		if conflict != nil {
			report.Conflicts = append(report.Conflicts, *conflict)
		}
	}

	// 2. Revocations (before any role change). A federated principal is
	// disabled while revoked, and also a service account removed from policy.
	revoked := revokedSet(fid, body)
	accounts := map[string]bool{}
	for _, sa := range body.ServiceAccounts {
		accounts[sa.Name] = true
	}
	principals, err := loadFederatedPrincipals(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return report, err
	}
	for _, p := range principals {
		disable := revoked[p.Issuer+"\n"+p.Subject] || (p.Issuer == saIssuer && !accounts[p.Subject]) ||
			(p.Issuer != saIssuer && p.Kind == "service")
		if disable {
			report.Revoked++
		}
		if disable == p.Disabled {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE principals SET disabled=$3,revision=revision+1 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, p.ID, disable); err != nil {
			return report, err
		}
		if disable {
			for _, q := range []string{
				`UPDATE service_tokens SET revoked_at=clock_timestamp() WHERE instance_id=$1 AND principal_id=$2 AND revoked_at IS NULL`,
				`UPDATE personal_access_tokens SET revoked_at=clock_timestamp() WHERE instance_id=$1 AND principal_id=$2 AND revoked_at IS NULL`,
				`DELETE FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2`,
			} {
				if _, err := tx.Exec(ctx, q, s.cfg.InstanceID, p.ID); err != nil {
					return report, err
				}
			}
			if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.principal.revoked", p.ID, "success", map[string]any{"issuer": p.Issuer, "subject": p.Subject, "generation": doc.Generation}); err != nil {
				return report, err
			}
		} else if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.principal.restored", p.ID, "success", map[string]any{"issuer": p.Issuer, "subject": p.Subject, "generation": doc.Generation}); err != nil {
			return report, err
		}
	}

	// 3. Roles.
	roleIDs := map[string]string{}
	for _, r := range body.Roles {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO roles(instance_id,name,origin,federation_key,federation_generation) VALUES($1,$2,'federation',$2,$3)
ON CONFLICT(instance_id,name) DO UPDATE SET federation_generation=EXCLUDED.federation_generation,revision=roles.revision+1 RETURNING id::text`,
			s.cfg.InstanceID, r.Key, doc.Generation).Scan(&id); err != nil {
			return report, err
		}
		roleIDs[r.Key] = id
	}
	report.Roles = len(roleIDs)
	if _, err := tx.Exec(ctx, `DELETE FROM role_grants WHERE instance_id=$1 AND origin='federation'`, s.cfg.InstanceID); err != nil {
		return report, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM principal_roles WHERE instance_id=$1 AND origin='federation'`, s.cfg.InstanceID); err != nil {
		return report, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM federation_group_bindings WHERE instance_id=$1`, s.cfg.InstanceID); err != nil {
		return report, err
	}
	keep := make([]string, 0, len(roleIDs))
	for _, id := range roleIDs {
		keep = append(keep, id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM roles WHERE instance_id=$1 AND origin='federation' AND NOT (id::text = ANY($2::text[]))`, s.cfg.InstanceID, keep); err != nil {
		return report, err
	}

	// 4. Grants.
	rows, unresolved, inertAdmin := planGrants(body, bindings, accept)
	for _, g := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,namespace,actions,origin,feed_key,inert) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,'federation',NULLIF($8,''),NULLIF($9,''))`,
			s.cfg.InstanceID, roleIDs[g.RoleKey], g.Kind, g.Effect, g.FeedID, g.Namespace, g.Actions, g.FeedKey, g.Inert); err != nil {
			return report, err
		}
	}
	report.Grants, report.UnresolvedFeedKeys, report.InertAdminGrants = len(rows), unresolved, inertAdmin

	// 5. Group bindings.
	for _, g := range body.GroupBindings {
		for _, r := range g.Roles {
			if _, err := tx.Exec(ctx, `INSERT INTO federation_group_bindings(instance_id,domain,group_id,role_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
				s.cfg.InstanceID, g.Domain, g.GroupID, roleIDs[r]); err != nil {
				return report, err
			}
		}
	}

	// 6. Memberships of every federated principal.
	if principals, err = loadFederatedPrincipals(ctx, tx, s.cfg.InstanceID); err != nil {
		return report, err
	}
	report.Principals = len(principals)
	if err := s.writeMemberships(ctx, tx, fid, body, roleIDs, principals); err != nil {
		return report, err
	}
	return report, disableApply(ctx, tx)
}

func loadFederatedPrincipals(ctx context.Context, tx pgx.Tx, instance string) ([]fedPrincipal, error) {
	rows, err := tx.Query(ctx, `SELECT id::text,kind,issuer,subject,disabled FROM principals WHERE instance_id=$1 AND origin='federation' ORDER BY id`, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []fedPrincipal{}
	for rows.Next() {
		var p fedPrincipal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Issuer, &p.Subject, &p.Disabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// writeMemberships inserts the federation memberships of the given principals
// (their federation rows must already be deleted).
func (s *Service) writeMemberships(ctx context.Context, tx pgx.Tx, fid string, body *PolicyBody, roleIDs map[string]string, principals []fedPrincipal) error {
	saIssuer := ServiceAccountIssuer(fid)
	for _, p := range principals {
		var keys []string
		if p.Issuer == saIssuer {
			keys = desiredRoles(body, "", "", p.Subject, nil)
		} else {
			d, ok := body.DomainByIssuer(p.Issuer)
			if !ok {
				continue
			}
			groups := map[string]bool{}
			rows, err := tx.Query(ctx, `SELECT group_id FROM federation_principal_groups WHERE instance_id=$1 AND principal_id=$2 AND domain=$3`, s.cfg.InstanceID, p.ID, d.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var g string
				if err := rows.Scan(&g); err != nil {
					rows.Close()
					return err
				}
				groups[g] = true
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			keys = desiredRoles(body, d.ID, p.Subject, "", groups)
		}
		for _, k := range keys {
			if id := roleIDs[k]; id != "" {
				if _, err := tx.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id,origin) VALUES($1,$2,$3,'federation') ON CONFLICT DO NOTHING`, s.cfg.InstanceID, p.ID, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func federationRoleIDs(ctx context.Context, tx pgx.Tx, instance string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT federation_key,id::text FROM roles WHERE instance_id=$1 AND origin='federation'`, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			return nil, err
		}
		out[k] = id
	}
	return out, rows.Err()
}

// dropFederatedIdentity removes federation-managed roles, grants, memberships
// and group bindings (leaving or removal, §4.3). Federated principals become
// local principals with only their local memberships; content is untouched.
func dropFederatedIdentity(ctx context.Context, tx pgx.Tx, instance string) error {
	if err := enableApply(ctx, tx); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM role_grants WHERE instance_id=$1 AND origin='federation'`,
		`DELETE FROM principal_roles WHERE instance_id=$1 AND origin='federation'`,
		`DELETE FROM federation_group_bindings WHERE instance_id=$1`,
		`DELETE FROM roles WHERE instance_id=$1 AND origin='federation'`,
		`DELETE FROM federation_principal_groups WHERE instance_id=$1`,
		`UPDATE principals SET origin='local' WHERE instance_id=$1 AND origin='federation'`,
		`UPDATE federation_state SET policy_generation=0,policy_hash='',policy_applied_at=NULL,policy_confirmed_at=NULL,policy_error='',policy_error_at=NULL,policy_report='{}'::jsonb,alert_state='{}'::jsonb WHERE instance_id=$1`,
		`DELETE FROM federation_policy_drafts WHERE instance_id=$1`,
	} {
		if _, err := tx.Exec(ctx, q, instance); err != nil {
			return err
		}
	}
	if err := dropFederatedReplication(ctx, tx, instance); err != nil {
		return err
	}
	return disableApply(ctx, tx)
}

// --- policy state and storage ---

// PolicyState is the applied policy head of this instance.
type PolicyState struct {
	Generation  int64
	Hash        string
	AppliedAt   *time.Time
	ConfirmedAt *time.Time
	Error       string
	ErrorAt     *time.Time
	Report      PolicyReport
	Alerts      map[string]string
}

func loadPolicyState(ctx context.Context, q queryer, instance string) (PolicyState, error) {
	var p PolicyState
	var report, alerts []byte
	err := q.QueryRow(ctx, `SELECT policy_generation,policy_hash,policy_applied_at,policy_confirmed_at,policy_error,policy_error_at,policy_report,alert_state FROM federation_state WHERE instance_id=$1`, instance).
		Scan(&p.Generation, &p.Hash, &p.AppliedAt, &p.ConfirmedAt, &p.Error, &p.ErrorAt, &report, &alerts)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyState{Alerts: map[string]string{}}, nil
	}
	if err != nil {
		return p, err
	}
	_ = json.Unmarshal(report, &p.Report)
	p.Alerts = map[string]string{}
	_ = json.Unmarshal(alerts, &p.Alerts)
	if p.Report.UnresolvedFeedKeys == nil {
		p.Report.UnresolvedFeedKeys = []string{}
	}
	if p.Report.Conflicts == nil {
		p.Report.Conflicts = []PrincipalConflict{}
	}
	return p, nil
}

func loadPolicy(ctx context.Context, q queryer, instance, federation string, generation int64) (*PolicyDocument, error) {
	if generation == 0 {
		return nil, nil
	}
	var body []byte
	if err := q.QueryRow(ctx, `SELECT body FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='policy' AND generation=$3`, instance, federation, generation).Scan(&body); err != nil {
		return nil, err
	}
	var d PolicyDocument
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func policyDocumentsAfter(ctx context.Context, q queryer, instance, federation string, after int64, limit int) ([]PolicyDocument, error) {
	rows, err := q.Query(ctx, `SELECT body FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='policy' AND generation>$3 ORDER BY generation LIMIT $4`, instance, federation, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PolicyDocument{}
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var d PolicyDocument
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// storePolicy appends a verified policy document, materializes it and moves
// the policy head. confirmed is when the document was last known current.
func (s *Service) storePolicy(ctx context.Context, tx pgx.Tx, fid string, doc *PolicyDocument, confirmed time.Time) (PolicyReport, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return PolicyReport{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO federation_documents(instance_id,federation_id,kind,generation,authority_epoch,hash,prev_hash,body) VALUES($1,$2,'policy',$3,$4,$5,$6,$7)
ON CONFLICT(instance_id,federation_id,kind,generation) DO NOTHING`, s.cfg.InstanceID, fid, doc.Generation, doc.AuthorityEpoch, PolicyHash(doc), doc.PrevHash, body); err != nil {
		return PolicyReport{}, err
	}
	report, err := s.materialize(ctx, tx, fid, doc)
	if err != nil {
		return report, err
	}
	raw, _ := json.Marshal(report)
	_, err = tx.Exec(ctx, `UPDATE federation_state SET policy_generation=$2,policy_hash=$3,policy_applied_at=$4,policy_confirmed_at=$5,policy_error='',policy_error_at=NULL,policy_report=$6,
revision=revision+1,updated_at=clock_timestamp() WHERE instance_id=$1`, s.cfg.InstanceID, doc.Generation, PolicyHash(doc), s.now(), confirmed, raw)
	return report, err
}

// rematerialize re-applies the current policy after a local change (feed
// binding, settings, adopt, sign-in groups). It is a no-op without a policy.
func (s *Service) rematerialize(ctx context.Context, tx pgx.Tx) (PolicyReport, error) {
	st, err := lockState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return PolicyReport{}, err
	}
	ps, err := loadPolicyState(ctx, tx, s.cfg.InstanceID)
	if err != nil || st.FederationID == "" || st.State != "active" || ps.Generation == 0 {
		return ps.Report, err
	}
	doc, err := loadPolicy(ctx, tx, s.cfg.InstanceID, st.FederationID, ps.Generation)
	if err != nil {
		return PolicyReport{}, err
	}
	report, err := s.materialize(ctx, tx, st.FederationID, doc)
	if err != nil {
		return report, err
	}
	raw, _ := json.Marshal(report)
	_, err = tx.Exec(ctx, `UPDATE federation_state SET policy_report=$2 WHERE instance_id=$1`, s.cfg.InstanceID, raw)
	return report, err
}
