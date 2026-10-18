package remoteconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AllowedCARefs is operator-owned immutable configuration: opaque IDs resolving
// to mounted trust bundles elsewhere. No request can provide a filesystem path.
type Store struct {
	DB            *pgxpool.Pool
	InstanceID    string
	AllowedCARefs []string
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func (s Store) valid(p identity.Principal, source string) bool {
	return s.DB != nil && uuid.MatchString(s.InstanceID) && uuid.MatchString(source) && p.InstanceID == s.InstanceID && uuid.MatchString(p.PrincipalID)
}
func (s Store) authorized(p identity.Principal, policy identity.Policy, source string) bool {
	return policy.Authorize(p, identity.Resource{InstanceID: s.InstanceID, SourceID: source, Kind: identity.SourceResource}, identity.Configure)
}
func (s Store) resolve(ctx context.Context, p identity.Principal, source string) (identity.Policy, error) {
	if !s.valid(p, source) {
		return identity.Policy{}, ErrInput
	}
	policy, err := (identity.Roles{DB: s.DB}).Resolve(ctx, p)
	if errors.Is(err, identity.ErrUnauthenticated) {
		return identity.Policy{}, ErrDenied
	}
	if err != nil {
		return identity.Policy{}, ErrUnavailable
	}
	if !s.authorized(p, policy, source) {
		return identity.Policy{}, ErrDenied
	}
	return policy, nil
}
func (s Store) Get(ctx context.Context, p identity.Principal, source string) (Record, error) {
	if _, err := s.resolve(ctx, p, source); err != nil {
		return Record{}, err
	}
	return s.Load(ctx, source)
}

// Load is for trusted runtime use only, after normal artifact read authorization.
// It is not a management handler. Removed operator CA references remain visible
// here; materializers must fail closed if they cannot resolve an approved CARef.
func (s Store) Load(ctx context.Context, source string) (Record, error) {
	if s.DB == nil || !uuid.MatchString(s.InstanceID) || !uuid.MatchString(source) {
		return Record{}, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var r Record
	var raw []byte
	err := s.DB.QueryRow(ctx, `SELECT s.id::text,s.revision,c.config FROM remote_source_configs c JOIN sources s ON (s.instance_id,s.id)=(c.instance_id,c.source_id) WHERE c.instance_id=$1 AND c.source_id=$2 AND s.kind='remote' AND c.schema_version=1`, s.InstanceID, source).Scan(&r.SourceID, &r.Revision, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, ErrUnavailable
	}
	if len(raw) > 32768 {
		return Record{}, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r.Config) != nil || decoder.Decode(new(any)) != io.EOF {
		return Record{}, ErrUnavailable
	}
	r.Config, err = Normalize(r.Config)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	return r, nil
}
func (s Store) Set(ctx context.Context, p identity.Principal, source string, expected int64, c Config) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	policy, err := s.resolve(ctx, p, source)
	if err != nil {
		return Record{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	defer rollback(tx)
	r, err := s.SetTx(ctx, tx, p, policy, source, expected, c)
	if err != nil {
		return Record{}, err
	}
	if tx.Commit(ctx) != nil {
		return Record{}, ErrUnavailable
	}
	return r, nil
}

// SetTx supports an outer idempotency transaction. Policy must be resolved by
// trusted authorization code for this principal; this method never commits.
func (s Store) SetTx(ctx context.Context, tx pgx.Tx, p identity.Principal, policy identity.Policy, source string, expected int64, c Config) (Record, error) {
	if tx == nil || !s.valid(p, source) || expected < 1 {
		return Record{}, ErrInput
	}
	if !s.authorized(p, policy, source) {
		return Record{}, ErrDenied
	}
	c, err := Normalize(c)
	if err != nil {
		return Record{}, err
	}
	if c.CARef != "" {
		allowed := false
		for _, id := range s.AllowedCARefs {
			if id == c.CARef {
				allowed = true
				break
			}
		}
		if !allowed {
			return Record{}, ErrInput
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var kind string
	var revision int64
	err = tx.QueryRow(ctx, `SELECT kind,revision FROM sources WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.InstanceID, source).Scan(&kind, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, ErrUnavailable
	}
	if kind != "remote" {
		return Record{}, ErrNotFound
	}
	if revision != expected {
		return Record{}, ErrConflict
	}
	var credential any
	if c.CredentialID != "" {
		var purpose string
		err = tx.QueryRow(ctx, `SELECT purpose FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 FOR KEY SHARE`, s.InstanceID, c.CredentialID).Scan(&purpose)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && purpose != CredentialPurpose(source) {
			return Record{}, ErrInput
		}
		if err != nil {
			return Record{}, ErrUnavailable
		}
		credential = c.CredentialID
	}
	raw, err := json.Marshal(c)
	if err != nil || len(raw) > 32768 {
		return Record{}, ErrInput
	}
	_, err = tx.Exec(ctx, `INSERT INTO remote_source_configs(instance_id,source_id,credential_id,config) VALUES($1,$2,$3,$4) ON CONFLICT(instance_id,source_id) DO UPDATE SET credential_id=excluded.credential_id,config=excluded.config,updated_at=clock_timestamp()`, s.InstanceID, source, credential, raw)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	if err = tx.QueryRow(ctx, `UPDATE sources SET revision=revision+1 WHERE instance_id=$1 AND id=$2 RETURNING revision`, s.InstanceID, source).Scan(&revision); err != nil {
		return Record{}, ErrUnavailable
	}
	var audit int64
	if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'source.remote.configure',$3) RETURNING id`, s.InstanceID, p.PrincipalID, source).Scan(&audit); err != nil {
		return Record{}, ErrUnavailable
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'source.remote.configure',jsonb_build_object('source_id',$2::text,'revision',$3::bigint))`, audit, source, revision)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	return Record{SourceID: source, Revision: revision, Config: c}, nil
}
