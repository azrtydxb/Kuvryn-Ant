package idpadmin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrLastProvider = errors.New("cannot remove the last enabled sign-in provider while SSO is mandatory")

// Check builds the runtime provider for a candidate configuration without
// publishing it. It is invoked before any enabled configuration is stored, so a
// broken configuration is rejected rather than saved.
type Check func(context.Context, Loaded) error

type Store struct {
	DB         *pgxpool.Pool
	Keys       *secrets.Keyring
	InstanceID string
	// FileKinds lists protocols configured from read-only files; they count as
	// active providers for the SSO-mandatory safety guard.
	FileKinds []Kind
}

const purposePrefix = "identity-provider:"

const selectCols = `id::text,kind,name,enabled,config,secret_id IS NOT NULL,revision,COALESCE(last_error,''),created_at,updated_at`

func scan(row pgx.Row) (Provider, error) {
	var p Provider
	var cfg []byte
	err := row.Scan(&p.ID, &p.Kind, &p.Name, &p.Enabled, &cfg, &p.HasSecret, &p.Revision, &p.LastError, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	p.Config = cfg
	return p, err
}

func (s Store) List(ctx context.Context) ([]Provider, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+selectCols+` FROM identity_providers WHERE instance_id=$1 ORDER BY kind,name`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s Store) Get(ctx context.Context, id string) (Provider, error) {
	return scan(s.DB.QueryRow(ctx, `SELECT `+selectCols+` FROM identity_providers WHERE instance_id=$1 AND id::text=$2`, s.InstanceID, strings.ToLower(id)))
}

// Load returns the provider with its decrypted secret.
func (s Store) Load(ctx context.Context, id string) (Loaded, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return Loaded{}, err
	}
	return s.attach(ctx, p)
}

func (s Store) attach(ctx context.Context, p Provider) (Loaded, error) {
	l := Loaded{Provider: p}
	if !p.HasSecret {
		return l, nil
	}
	var secretID string
	if err := s.DB.QueryRow(ctx, `SELECT secret_id::text FROM identity_providers WHERE instance_id=$1 AND id=$2`, s.InstanceID, p.ID).Scan(&secretID); err != nil {
		return Loaded{}, err
	}
	_, plain, err := (secrets.Store{DB: s.DB, Keys: s.Keys}).Open(ctx, s.InstanceID, secretID, purposePrefix+p.ID)
	if err != nil {
		return Loaded{}, err
	}
	l.Secret = plain
	return l, nil
}

// LoadEnabled returns every enabled provider with secrets. A provider whose
// secret cannot be opened is returned with a nil secret and recorded error so
// one bad row cannot block loading the others.
func (s Store) LoadEnabled(ctx context.Context) ([]Loaded, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+selectCols+` FROM identity_providers WHERE instance_id=$1 AND enabled ORDER BY kind`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	var ps []Provider
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ps = append(ps, p)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	out := make([]Loaded, 0, len(ps))
	for _, p := range ps {
		l, err := s.attach(ctx, p)
		if err != nil {
			l = Loaded{Provider: p}
			l.LastError = "secret unavailable"
		}
		out = append(out, l)
	}
	return out, nil
}

func (s Store) RecordError(ctx context.Context, id, msg string) {
	if len(msg) > 250 {
		msg = msg[:250]
	}
	_, _ = s.DB.Exec(ctx, `UPDATE identity_providers SET last_error=NULLIF($3,'') WHERE instance_id=$1 AND id::text=$2`, s.InstanceID, id, msg)
}

func (s Store) audit(ctx context.Context, tx pgx.Tx, actor, action string, p Provider) error {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,resource_id,action,details) VALUES($1,NULLIF($2,'')::uuid,$3,$4,jsonb_build_object('kind',$5::text,'name',$6::text,'enabled',$7::boolean,'revision',$8::bigint)) RETURNING id`,
		s.InstanceID, actor, p.ID, action, string(p.Kind), p.Name, p.Enabled, p.Revision).Scan(&id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('provider_id',$3::text,'kind',$4::text))`, id, action, p.ID, string(p.Kind))
	return err
}

// GenerateSPKeyPair creates a 2048-bit RSA key and self-signed certificate for
// the SAML service provider, valid for two years.
func GenerateSPKeyPair(entityID string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	cn := entityID
	if len(cn) > 60 {
		cn = cn[:60]
	}
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn, Organization: []string{"Kuvryn Ant"}}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().AddDate(2, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// prepare normalizes input into the effective config and secret. existing may
// be nil on create.
func prepare(in Input, existing *Loaded) (cfg json.RawMessage, secret []byte, err error) {
	if err = Validate(in.Kind, in.Config); err != nil {
		return nil, nil, err
	}
	cfg = in.Config
	if in.Secret != nil {
		secret = []byte(*in.Secret)
	}
	switch in.Kind {
	case SAML:
		var c SAMLConfig
		_ = strict(in.Config, &c)
		secret = nil
		switch {
		case existing != nil && !in.RotateSPCertificate:
			var old SAMLConfig
			_ = json.Unmarshal(existing.Config, &old)
			c.SPCertificatePEM, secret = old.SPCertificatePEM, existing.Secret
		default:
			cert, key, e := GenerateSPKeyPair(c.EntityID)
			if e != nil {
				return nil, nil, e
			}
			c.SPCertificatePEM, secret = string(cert), key
		}
		cfg, _ = json.Marshal(c)
	case LDAP:
		if secret == nil && existing != nil {
			secret = existing.Secret
		}
		if len(secret) == 0 || len(secret) > 4096 {
			return nil, nil, ErrInvalid
		}
	case OIDC:
		if in.Secret == nil && existing != nil {
			secret = existing.Secret
		}
		if len(secret) > 4096 || strings.ContainsAny(string(secret), "\r\n\x00") {
			return nil, nil, ErrInvalid
		}
	}
	return cfg, secret, nil
}

func (s Store) guard(ctx context.Context, tx pgx.Tx, excludeID string, willBeEnabled bool, kind Kind) error {
	if willBeEnabled {
		return nil
	}
	var mandatory bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT sso_mandatory FROM identity_policy WHERE instance_id=$1),false)`, s.InstanceID).Scan(&mandatory); err != nil {
		return err
	}
	if !mandatory {
		return nil
	}
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_providers WHERE instance_id=$1 AND enabled AND id::text<>$2`, s.InstanceID, excludeID).Scan(&others); err != nil {
		return err
	}
	if others+len(s.FileKinds) == 0 {
		return ErrLastProvider
	}
	return nil
}

func mapErr(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return ErrConflict
	}
	return err
}

func (s Store) Create(ctx context.Context, actor string, in Input, check Check) (Provider, error) {
	if !in.Kind.Valid() || !validName(in.Name) {
		return Provider{}, ErrInvalid
	}
	cfg, secret, err := prepare(in, nil)
	if err != nil {
		return Provider{}, err
	}
	defer clear(secret)
	var id string
	if err = s.DB.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return Provider{}, err
	}
	if in.Enabled && check != nil {
		if err = check(ctx, Loaded{Provider: Provider{ID: id, Kind: in.Kind, Name: in.Name, Enabled: true, Config: cfg}, Secret: secret}); err != nil {
			return Provider{}, err
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Provider{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var secretID *string
	if len(secret) > 0 {
		rec, err := (secrets.Store{Keys: s.Keys}).CreateTx(ctx, tx, s.InstanceID, actor, purposePrefix+id, secret)
		if err != nil {
			return Provider{}, err
		}
		secretID = &rec.ID
	}
	if in.Enabled {
		if _, err = tx.Exec(ctx, `UPDATE identity_providers SET enabled=false,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND kind=$2 AND enabled`, s.InstanceID, string(in.Kind)); err != nil {
			return Provider{}, err
		}
	}
	p, err := scan(tx.QueryRow(ctx, `INSERT INTO identity_providers(id,instance_id,kind,name,enabled,config,secret_id) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+selectCols, id, s.InstanceID, string(in.Kind), in.Name, in.Enabled, []byte(cfg), secretID))
	if err != nil {
		return Provider{}, mapErr(err)
	}
	if err = s.audit(ctx, tx, actor, "identity_provider.create", p); err != nil {
		return Provider{}, err
	}
	return p, tx.Commit(ctx)
}

// Update replaces a provider at an exact revision. Kind is immutable.
func (s Store) Update(ctx context.Context, actor, id string, revision int64, in Input, check Check) (Provider, error) {
	if !validName(in.Name) {
		return Provider{}, ErrInvalid
	}
	cur, err := s.Load(ctx, id)
	if err != nil {
		return Provider{}, err
	}
	defer clear(cur.Secret)
	if cur.Revision != revision {
		return Provider{}, ErrRevision
	}
	in.Kind = cur.Kind
	cfg, secret, err := prepare(in, &cur)
	if err != nil {
		return Provider{}, err
	}
	defer clear(secret)
	if in.Enabled && check != nil {
		if err = check(ctx, Loaded{Provider: Provider{ID: cur.ID, Kind: cur.Kind, Name: in.Name, Enabled: true, Config: cfg}, Secret: secret}); err != nil {
			return Provider{}, err
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Provider{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var rev int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM identity_providers WHERE instance_id=$1 AND id::text=$2 FOR UPDATE`, s.InstanceID, cur.ID).Scan(&rev); err != nil || rev != revision {
		return Provider{}, ErrRevision
	}
	if err = s.guard(ctx, tx, cur.ID, in.Enabled, cur.Kind); err != nil {
		return Provider{}, err
	}
	var secretID, existingSecret *string
	if err = tx.QueryRow(ctx, `SELECT secret_id::text FROM identity_providers WHERE instance_id=$1 AND id::text=$2`, s.InstanceID, cur.ID).Scan(&existingSecret); err != nil {
		return Provider{}, err
	}
	if len(secret) > 0 {
		changed := in.Secret != nil || in.RotateSPCertificate
		switch {
		case existingSecret == nil:
			rec, err := (secrets.Store{Keys: s.Keys}).CreateTx(ctx, tx, s.InstanceID, actor, purposePrefix+cur.ID, secret)
			if err != nil {
				return Provider{}, err
			}
			secretID = &rec.ID
		case changed:
			var secretRev int64
			if err = tx.QueryRow(ctx, `SELECT revision FROM encrypted_secrets WHERE instance_id=$1 AND id::text=$2`, s.InstanceID, *existingSecret).Scan(&secretRev); err != nil {
				return Provider{}, err
			}
			if _, err = (secrets.Store{Keys: s.Keys}).UpdateTx(ctx, tx, s.InstanceID, actor, *existingSecret, purposePrefix+cur.ID, secretRev, secret); err != nil {
				return Provider{}, err
			}
			secretID = existingSecret
		default:
			secretID = existingSecret
		}
	}
	if in.Enabled {
		if _, err = tx.Exec(ctx, `UPDATE identity_providers SET enabled=false,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND kind=$2 AND enabled AND id::text<>$3`, s.InstanceID, string(cur.Kind), cur.ID); err != nil {
			return Provider{}, err
		}
	}
	p, err := scan(tx.QueryRow(ctx, `UPDATE identity_providers SET name=$3,enabled=$4,config=$5,secret_id=$6::uuid,last_error=NULL,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id::text=$2 RETURNING `+selectCols, s.InstanceID, cur.ID, in.Name, in.Enabled, []byte(cfg), secretID))
	if err != nil {
		return Provider{}, mapErr(err)
	}
	if secretID == nil && existingSecret != nil {
		if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id::text=$2`, s.InstanceID, *existingSecret); err != nil {
			return Provider{}, err
		}
	}
	if err = s.audit(ctx, tx, actor, "identity_provider.update", p); err != nil {
		return Provider{}, err
	}
	return p, tx.Commit(ctx)
}

// SetEnabled toggles a provider; enabling is checked first and swaps out any
// other enabled provider of the same protocol.
func (s Store) SetEnabled(ctx context.Context, actor, id string, revision int64, enabled bool, check Check) (Provider, error) {
	cur, err := s.Load(ctx, id)
	if err != nil {
		return Provider{}, err
	}
	defer clear(cur.Secret)
	if cur.Revision != revision {
		return Provider{}, ErrRevision
	}
	if enabled && check != nil {
		cur.Enabled = true
		if err = check(ctx, cur); err != nil {
			return Provider{}, err
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Provider{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var rev int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM identity_providers WHERE instance_id=$1 AND id::text=$2 FOR UPDATE`, s.InstanceID, cur.ID).Scan(&rev); err != nil || rev != revision {
		return Provider{}, ErrRevision
	}
	if err = s.guard(ctx, tx, cur.ID, enabled, cur.Kind); err != nil {
		return Provider{}, err
	}
	if enabled {
		if _, err = tx.Exec(ctx, `UPDATE identity_providers SET enabled=false,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND kind=$2 AND enabled AND id::text<>$3`, s.InstanceID, string(cur.Kind), cur.ID); err != nil {
			return Provider{}, err
		}
	}
	p, err := scan(tx.QueryRow(ctx, `UPDATE identity_providers SET enabled=$3,last_error=NULL,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id::text=$2 RETURNING `+selectCols, s.InstanceID, cur.ID, enabled))
	if err != nil {
		return Provider{}, mapErr(err)
	}
	action := "identity_provider.disable"
	if enabled {
		action = "identity_provider.enable"
	}
	if err = s.audit(ctx, tx, actor, action, p); err != nil {
		return Provider{}, err
	}
	return p, tx.Commit(ctx)
}

func (s Store) Delete(ctx context.Context, actor, id string, revision int64) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	p, err := scan(tx.QueryRow(ctx, `SELECT `+selectCols+` FROM identity_providers WHERE instance_id=$1 AND id::text=$2 FOR UPDATE`, s.InstanceID, strings.ToLower(id)))
	if err != nil {
		return err
	}
	if p.Revision != revision {
		return ErrRevision
	}
	if err = s.guard(ctx, tx, p.ID, false, p.Kind); err != nil {
		return err
	}
	var secretID *string
	if err = tx.QueryRow(ctx, `SELECT secret_id::text FROM identity_providers WHERE instance_id=$1 AND id::text=$2`, s.InstanceID, p.ID).Scan(&secretID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM identity_providers WHERE instance_id=$1 AND id::text=$2`, s.InstanceID, p.ID); err != nil {
		return err
	}
	if secretID != nil {
		if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id::text=$2`, s.InstanceID, *secretID); err != nil {
			return err
		}
	}
	if err = s.audit(ctx, tx, actor, "identity_provider.delete", p); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetSSOMandatory updates the policy. Enabling requires at least one enabled
// provider (database or file) and an existing break-glass account, so the
// policy cannot lock every administrator out.
func (s Store) SetSSOMandatory(ctx context.Context, actor string, mandatory bool) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if mandatory {
		var providers, breakGlass int
		if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM identity_providers WHERE instance_id=$1 AND enabled),(SELECT count(*) FROM local_accounts WHERE instance_id=$1 AND break_glass)`, s.InstanceID).Scan(&providers, &breakGlass); err != nil {
			return err
		}
		if providers+len(s.FileKinds) == 0 {
			return ErrNoProvider
		}
		if breakGlass == 0 {
			return ErrNoBreakGlass
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_policy(instance_id,sso_mandatory) VALUES($1,$2) ON CONFLICT(instance_id) DO UPDATE SET sso_mandatory=$2,revision=identity_policy.revision+1,updated_at=now()`, s.InstanceID, mandatory); err != nil {
		return err
	}
	action := "identity_policy.sso_mandatory_disabled"
	if mandatory {
		action = "identity_policy.sso_mandatory_enabled"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action) VALUES($1,NULLIF($2,'')::uuid,$3)`, s.InstanceID, actor, action); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var (
	ErrNoProvider   = errors.New("enable a sign-in provider before making SSO mandatory")
	ErrNoBreakGlass = errors.New("create the break-glass account before making SSO mandatory (ant admin break-glass)")
)

func (s Store) SSOMandatory(ctx context.Context) (bool, error) {
	var v bool
	err := s.DB.QueryRow(ctx, `SELECT COALESCE((SELECT sso_mandatory FROM identity_policy WHERE instance_id=$1),false)`, s.InstanceID).Scan(&v)
	return v, err
}
