package identity

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrAlreadyBootstrapped = errors.New("initial instance already exists")
var ErrBootstrapInput = errors.New("invalid bootstrap name")

type Roles struct{ DB *pgxpool.Pool }

// Resolve loads only database-owned role membership. Caller-supplied token scopes
// cannot add grants; Policy.Authorize intersects these grants with those scopes.
// This returns a request snapshot, not a long-lived authorization cache.
func (r Roles) Resolve(ctx context.Context, p Principal) (Policy, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if p.InstanceID == "" || p.PrincipalID == "" {
		return Policy{}, ErrUnauthenticated
	}
	if p.Anonymous {
		return r.resolveAnonymous(ctx, p)
	}
	// Long artifact transfers re-resolve policy before metadata commit. Recheck
	// machine-token lifetime here so revocation during staging cannot publish.
	if p.TokenID != "" {
		var active bool
		err := r.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM service_tokens WHERE instance_id=$1 AND id=$2 AND principal_id=$3 AND revoked_at IS NULL AND expires_at>clock_timestamp()) OR EXISTS(SELECT 1 FROM personal_access_tokens WHERE instance_id=$1 AND id=$2 AND principal_id=$3 AND revoked_at IS NULL AND expires_at>clock_timestamp())", p.InstanceID, p.TokenID, p.PrincipalID).Scan(&active)
		if err != nil {
			return Policy{}, ErrUnavailable
		}
		if !active {
			return Policy{}, ErrUnauthenticated
		}
	}
	rows, err := r.DB.Query(ctx, `SELECT r.name,g.kind,g.effect,g.feed_id::text,g.source_id::text,g.namespace,g.actions
 FROM principals p JOIN principal_roles pr ON (pr.instance_id,pr.principal_id)=(p.instance_id,p.id)
 JOIN roles r ON (r.instance_id,r.id)=(pr.instance_id,pr.role_id)
 JOIN role_grants g ON (g.instance_id,g.role_id)=(r.instance_id,r.id)
 WHERE p.instance_id=$1 AND p.id=$2 AND NOT p.disabled ORDER BY r.id,g.id`, p.InstanceID, p.PrincipalID)
	if err != nil {
		return Policy{}, ErrUnavailable
	}
	defer rows.Close()
	policy := Policy{}
	indexes := map[string]int{}
	for rows.Next() {
		var name string
		var g Grant
		var feed, source *string
		var actions []string
		if err := rows.Scan(&name, &g.Kind, &g.Effect, &feed, &source, &g.Namespace, &actions); err != nil {
			return Policy{}, ErrUnavailable
		}
		g.InstanceID = p.InstanceID
		if feed != nil {
			g.FeedID = *feed
		}
		if source != nil {
			g.SourceID = *source
		}
		for _, action := range actions {
			g.Actions = append(g.Actions, Action(action))
		}
		i, ok := indexes[name]
		if !ok {
			i = len(policy.Roles)
			indexes[name] = i
			policy.Roles = append(policy.Roles, Role{Name: name, PrincipalIDs: []string{p.PrincipalID}})
		}
		policy.Roles[i].Grants = append(policy.Roles[i].Grants, g)
	}
	if rows.Err() != nil {
		return Policy{}, ErrUnavailable
	}
	return policy, nil
}

type BootstrapIdentity struct{ InstanceID, PrincipalID string }

// Bootstrap is a privileged local setup operation, never a public signup API.
// It creates an initial administrator atomically, without issuing a credential.
// If subsequent token issuance fails, an explicitly authorized local operator can
// call LoadBootstrapIdentity and retry Issue. Never expose that recovery helper
// as an unauthenticated endpoint or automatically grant an external caller access.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, name string) (BootstrapIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") {
		return BootstrapIdentity{}, ErrBootstrapInput
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(741812095)"); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	// Coordinate with ordinary instance inserts as well as other bootstraps.
	if _, err = tx.Exec(ctx, "LOCK TABLE instances IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	var exists bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM instances)").Scan(&exists); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	if exists {
		return BootstrapIdentity{}, ErrAlreadyBootstrapped
	}
	var result BootstrapIdentity
	if err = tx.QueryRow(ctx, "INSERT INTO instances(name) VALUES($1) RETURNING id", name).Scan(&result.InstanceID); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	if err = tx.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','bootstrap-admin') RETURNING id", result.InstanceID).Scan(&result.PrincipalID); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	var role string
	if err = tx.QueryRow(ctx, "INSERT INTO roles(instance_id,name) VALUES($1,'bootstrap-administrator') RETURNING id", result.InstanceID).Scan(&role); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	if _, err = tx.Exec(ctx, "INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)", result.InstanceID, result.PrincipalID, role); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	actions := []string{"read", "publish", "delete", "configure", "replicate", "admin"}
	for _, kind := range []ResourceKind{ArtifactResource, FeedResource, SourceResource, InstanceResource} {
		if _, err = tx.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,$2,$3,'allow',$4)", result.InstanceID, role, string(kind), actions); err != nil {
			return BootstrapIdentity{}, ErrUnavailable
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO bootstrap_state(instance_id,principal_id) VALUES($1,$2)", result.InstanceID, result.PrincipalID); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	var audit int64
	if err = tx.QueryRow(ctx, "INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'instance.bootstrap',$1) RETURNING id", result.InstanceID, result.PrincipalID).Scan(&audit); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	if _, err = tx.Exec(ctx, "INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'instance.bootstrap',jsonb_build_object('instance_id',$2::text,'principal_id',$3::text))", audit, result.InstanceID, result.PrincipalID); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	return result, nil
}

// LoadBootstrapIdentity is restricted to an already-authorized local recovery
// command. Possession of these IDs grants no authentication or authorization.
func LoadBootstrapIdentity(ctx context.Context, pool *pgxpool.Pool) (BootstrapIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result BootstrapIdentity
	err := pool.QueryRow(ctx, `SELECT b.instance_id,b.principal_id FROM bootstrap_state b JOIN principals p ON (p.instance_id,p.id)=(b.instance_id,b.principal_id) WHERE b.singleton AND NOT p.disabled AND p.kind='service'`).Scan(&result.InstanceID, &result.PrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return BootstrapIdentity{}, ErrUnauthenticated
	}
	if err != nil {
		return BootstrapIdentity{}, ErrUnavailable
	}
	return result, nil
}

// AnonymousSubject and AnonymousIssuer name the per-instance internal principal
// that anonymous reads run as (it owns audit rows and provenance only).
const (
	AnonymousSubject = "anonymous-reader"
	AnonymousIssuer  = "ant-internal"
)

// EnsureAnonymous returns the instance's anonymous-reader principal, creating
// it on first use. Disabling that principal turns anonymous reads off.
func (r Roles) EnsureAnonymous(ctx context.Context, instanceID string) (Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id string
	err := r.DB.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer) VALUES($1,'service',$2,$3) ON CONFLICT(instance_id,issuer,subject) DO UPDATE SET subject=excluded.subject RETURNING id::text`, instanceID, AnonymousSubject, AnonymousIssuer).Scan(&id)
	if err != nil {
		return Principal{}, ErrUnavailable
	}
	return Principal{InstanceID: instanceID, PrincipalID: id, Anonymous: true}, nil
}

// resolveAnonymous grants Read on every feed marked anonymous_read, and nothing
// else, while the anonymous-reader principal is enabled.
func (r Roles) resolveAnonymous(ctx context.Context, p Principal) (Policy, error) {
	if p.TokenID != "" || len(p.Scopes) != 0 {
		return Policy{}, ErrUnauthenticated
	}
	rows, err := r.DB.Query(ctx, `SELECT f.id::text FROM feeds f JOIN principals p ON p.instance_id=f.instance_id AND p.id=$2 AND p.subject=$3 AND p.issuer=$4 WHERE f.instance_id=$1 AND f.anonymous_read AND NOT p.disabled`, p.InstanceID, p.PrincipalID, AnonymousSubject, AnonymousIssuer)
	if err != nil {
		return Policy{}, ErrUnavailable
	}
	defer rows.Close()
	role := Role{Name: "anonymous-read", PrincipalIDs: []string{p.PrincipalID}}
	for rows.Next() {
		var feed string
		if rows.Scan(&feed) != nil {
			return Policy{}, ErrUnavailable
		}
		role.Grants = append(role.Grants, Grant{InstanceID: p.InstanceID, Kind: ArtifactResource, Effect: Allow, FeedID: feed, Actions: []Action{Read}})
	}
	if rows.Err() != nil {
		return Policy{}, ErrUnavailable
	}
	return Policy{Roles: []Role{role}}, nil
}
