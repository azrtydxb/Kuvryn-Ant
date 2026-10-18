package rpmrepo

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
)

// signingPurpose binds each feed's key to its own encrypted-secret purpose, so
// a ciphertext can never be replayed as another feed's (or an upstream trust)
// key. Local signing keys are never shared with upstream trust configuration.
func signingPurpose(feed string) string { return "rpm.repomd-signing-key:" + feed }

func (s *Repository) keyID(ctx context.Context, purpose string) (string, error) {
	var id string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2 ORDER BY created_at,id LIMIT 1`, s.instance, purpose).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", nil
	}
	if e != nil {
		return "", artifact.ErrUnavailable
	}
	return id, nil
}

// signer returns the feed's signing key (nil when signing is not configured, or
// when the key does not exist and create is false). Creation happens only on an
// administrator's publish, under the feed lock.
func (s *Repository) signer(ctx context.Context, p identity.Principal, feed string, create bool) (*openpgp.Entity, error) {
	store := s.secrets.Load()
	if store == nil {
		return nil, nil
	}
	purpose := signingPurpose(feed)
	id, e := s.keyID(ctx, purpose)
	if e != nil {
		return nil, e
	}
	if id == "" {
		if !create {
			return nil, nil
		}
		entity, e := openpgp.NewEntity("Kuvryn Ant feed "+feed, "repomd signing", "", &packet.Config{RSABits: s.keyBits, Time: time.Now})
		if e != nil {
			return nil, artifact.ErrUnavailable
		}
		var b bytes.Buffer
		if e = entity.SerializePrivate(&b, nil); e != nil {
			return nil, artifact.ErrUnavailable
		}
		if _, e = store.Create(ctx, s.instance, p.PrincipalID, purpose, b.Bytes()); e != nil {
			return nil, artifact.ErrUnavailable
		}
		// Oldest record wins if another process raced us to create a key.
		if id, e = s.keyID(ctx, purpose); e != nil || id == "" {
			return nil, artifact.ErrUnavailable
		}
	}
	_, plain, e := store.Open(ctx, s.instance, id, purpose)
	if errors.Is(e, secrets.ErrNotFound) {
		return nil, artifact.ErrUnavailable
	}
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	ring, e := openpgp.ReadKeyRing(bytes.NewReader(plain))
	if e != nil || len(ring) != 1 || ring[0].PrivateKey == nil || ring[0].PrivateKey.Encrypted {
		return nil, artifact.ErrUnavailable
	}
	return ring[0], nil
}

func detachedSignature(e *openpgp.Entity, message []byte) ([]byte, error) {
	var b bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&b, e, bytes.NewReader(message), &packet.Config{Time: time.Now}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func armoredPublicKey(e *openpgp.Entity) ([]byte, error) {
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err = e.Serialize(w); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
