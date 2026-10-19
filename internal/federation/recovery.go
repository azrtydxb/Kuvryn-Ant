package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Recovery-key rotation never sends a recovery private key to a server. The
// operator fetches a rotation template from the authority, signs the rotation
// statement with the current recovery key and the authority certificate with
// the new recovery key offline (`ant federation recovery sign-rotation` and
// `sign-authority-cert`, which need no database or network), and submits
// only the signed statements. The authority verifies both signatures and
// publishes them in the next membership generation.

// ErrRotationStale reports a signed rotation for another generation (the
// membership moved on after the template was fetched): fetch a new template
// and sign again.
var ErrRotationStale = errors.New("federation: the rotation is signed for another membership generation")

// RecoveryTemplate is the public input of the offline signer.
type RecoveryTemplate struct {
	FederationID string `json:"federation_id"`
	// Generation is the membership generation the rotation will be published at.
	Generation int64 `json:"generation"`
	// PreviousKey is the current recovery public key.
	PreviousKey string `json:"previous_key"`
	// Authority is the authority certificate to re-certify (without signature).
	Authority AuthorityCert `json:"authority"`
}

// SignedRecoveryRotation is the request body of a rotation.
type SignedRecoveryRotation struct {
	Rotation             RecoveryRotation `json:"rotation"`
	AuthorityCertificate AuthorityCert    `json:"authority_certificate"`
}

// RecoveryRotationResult reports the published rotation.
type RecoveryRotationResult struct {
	Status            Status `json:"status"`
	RecoveryPublicKey string `json:"recovery_public_key"`
}

// NewRecoveryKey generates a recovery key pair: the private export (keep it
// offline) and the encoded public key.
func NewRecoveryKey() (string, string, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	defer clear(key)
	return exportRecovery(key), encodeKey(pub), nil
}

// RecoveryPublicKey returns the encoded public key of a recovery export.
func RecoveryPublicKey(export string) (string, error) {
	key, err := parseRecovery(export)
	if err != nil {
		return "", err
	}
	defer clear(key)
	return encodeKey(key.Public().(ed25519.PublicKey)), nil
}

func (t RecoveryTemplate) valid() bool {
	_, e1 := decodeKey(t.PreviousKey)
	_, e2 := decodeKey(t.Authority.PublicKey)
	return uuidPattern.MatchString(t.FederationID) && t.Generation > 1 && t.Generation <= maxSafeInteger && e1 == nil && e2 == nil &&
		t.Authority.Epoch >= 1 && t.Authority.Epoch <= maxSafeInteger && uuidPattern.MatchString(t.Authority.InstanceID)
}

// SignRecoveryRotation signs, with the current recovery key, the statement
// that replaces it by newPublicKey. It runs offline.
func SignRecoveryRotation(t RecoveryTemplate, currentKey, newPublicKey string) (RecoveryRotation, error) {
	if !t.valid() {
		return RecoveryRotation{}, invalid("rotation template is incomplete or malformed")
	}
	if _, err := decodeKey(newPublicKey); err != nil || newPublicKey == t.PreviousKey {
		return RecoveryRotation{}, invalid("the new recovery public key is malformed or equals the current key")
	}
	key, err := parseRecovery(currentKey)
	if err != nil {
		return RecoveryRotation{}, err
	}
	defer clear(key)
	if encodeKey(key.Public().(ed25519.PublicKey)) != t.PreviousKey {
		return RecoveryRotation{}, ErrRecoveryMismatch
	}
	r := RecoveryRotation{PreviousKey: t.PreviousKey, NewKey: newPublicKey, Generation: t.Generation}
	r.Signature = sign(key, rotationStatement(t.FederationID, r))
	return r, nil
}

// SignAuthorityCertificate certifies the template's authority key with a
// recovery key (the new key after a rotation). It runs offline.
func SignAuthorityCertificate(t RecoveryTemplate, recoveryKey string) (AuthorityCert, error) {
	if !t.valid() {
		return AuthorityCert{}, invalid("rotation template is incomplete or malformed")
	}
	key, err := parseRecovery(recoveryKey)
	if err != nil {
		return AuthorityCert{}, err
	}
	defer clear(key)
	c := AuthorityCert{Epoch: t.Authority.Epoch, InstanceID: t.Authority.InstanceID, PublicKey: t.Authority.PublicKey, CertifiedBy: "recovery"}
	c.Signature = sign(key, authorityStatement(t.FederationID, c))
	return c, nil
}

// RecoveryTemplate returns the template for rotating the recovery key at the
// next membership generation (authority only).
func (s *Service) RecoveryTemplate(ctx context.Context) (RecoveryTemplate, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return RecoveryTemplate{}, err
	}
	if st.State != "active" || st.FederationID == "" {
		return RecoveryTemplate{}, ErrNotConfigured
	}
	if st.Role != "authority" {
		return RecoveryTemplate{}, ErrNotAuthority
	}
	applied, err := loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
	if err != nil {
		return RecoveryTemplate{}, err
	}
	a := applied.Authority
	return RecoveryTemplate{FederationID: applied.FederationID, Generation: applied.Generation + 1, PreviousKey: applied.RecoveryPublicKey,
		Authority: AuthorityCert{Epoch: a.Epoch, InstanceID: a.InstanceID, PublicKey: a.PublicKey, CertifiedBy: "recovery"}}, nil
}

// RotateRecoveryKey publishes a recovery-key rotation signed offline: the
// rotation statement must be signed by the current recovery key and the
// authority certificate by the new one, for the next membership generation.
func (s *Service) RotateRecoveryKey(ctx context.Context, actor string, in SignedRecoveryRotation) (RecoveryRotationResult, error) {
	var doc *Membership
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		r, c := in.Rotation, in.AuthorityCertificate
		if _, err := decodeKey(r.NewKey); err != nil || r.NewKey == applied.RecoveryPublicKey {
			return invalid("rotation.new_key must be a new ed25519 public key")
		}
		if r.Generation != applied.Generation+1 {
			return ErrRotationStale
		}
		if r.PreviousKey != applied.RecoveryPublicKey || !verify(applied.RecoveryPublicKey, r.Signature, rotationStatement(applied.FederationID, r)) {
			return ErrRecoveryMismatch
		}
		a := applied.Authority
		if c.Epoch != a.Epoch || c.InstanceID != a.InstanceID || c.PublicKey != a.PublicKey || c.CertifiedBy != "recovery" ||
			!verify(r.NewKey, c.Signature, authorityStatement(applied.FederationID, c)) {
			return invalid("authority_certificate must certify the current authority key with the new recovery key")
		}
		doc, err = s.publish(ctx, tx, st, applied, actor, func(next *Membership) error {
			rot := r
			next.RecoveryRotation = &rot
			next.RecoveryPublicKey = r.NewKey
			next.Authority = c
			return nil
		})
		if err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.recovery_key.rotated", st.FederationID, "success", map[string]any{
			"previous_fingerprint": KeyFingerprint(applied.RecoveryPublicKey), "fingerprint": KeyFingerprint(doc.RecoveryPublicKey), "generation": doc.Generation,
			"signed_offline": true, "at": s.now().Format(time.RFC3339)})
	})
	if err != nil {
		return RecoveryRotationResult{}, err
	}
	s.afterPublish(ctx, doc)
	out := RecoveryRotationResult{RecoveryPublicKey: doc.RecoveryPublicKey}
	out.Status, err = s.Status(ctx)
	return out, err
}
