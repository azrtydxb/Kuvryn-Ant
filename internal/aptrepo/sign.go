package aptrepo

import (
	"bytes"
	"context"
	"crypto"
	_ "crypto/sha256"
	"errors"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/clearsign"
	"golang.org/x/crypto/openpgp/packet"
)

// signingPurpose binds each encrypted key to exactly one feed.
func signingPurpose(feed string) string { return "apt-signing:" + feed }

var errNoKeyring = errors.New("apt signing keyring unavailable")

func (s *Repository) store() (secrets.Store, error) {
	k, _ := s.keys.Load().(*secrets.Keyring)
	if k == nil {
		return secrets.Store{}, errNoKeyring
	}
	return secrets.Store{DB: s.db, Keys: k}, nil
}

func (s *Repository) cfg() *packet.Config {
	return &packet.Config{DefaultHash: crypto.SHA256, Time: s.now}
}

// signingKey returns the feed's key, generating and encrypting it on first use.
// Callers hold the feed writer lock, so creation is serialized; the oldest
// stored record always wins if a previous attempt left more than one.
func (s *Repository) signingKey(ctx context.Context, p identity.Principal, feed string, create bool) (*openpgp.Entity, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	find := func() (string, error) {
		var id string
		e := s.db.QueryRow(ctx, `SELECT id::text FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2 ORDER BY created_at,id LIMIT 1`, p.InstanceID, signingPurpose(feed)).Scan(&id)
		return id, e
	}
	id, err := find()
	if errors.Is(err, pgx.ErrNoRows) && create {
		var entity *openpgp.Entity
		c := s.cfg()
		c.RSABits = 3072
		if entity, err = openpgp.NewEntity("Kuvryn Ant APT "+feed, "repository signing key", "", c); err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err = entity.SerializePrivate(&b, c); err != nil {
			return nil, err
		}
		if _, err = store.Create(ctx, p.InstanceID, p.PrincipalID, signingPurpose(feed), b.Bytes()); err != nil {
			return nil, err
		}
		clear(b.Bytes())
		id, err = find()
	}
	if err != nil {
		return nil, err
	}
	_, plain, err := store.Open(ctx, p.InstanceID, id, signingPurpose(feed))
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	entity, err := openpgp.ReadEntity(packet.NewReader(bytes.NewReader(plain)))
	if err != nil || entity.PrivateKey == nil || entity.PrivateKey.Encrypted {
		return nil, errors.New("apt signing key unreadable")
	}
	return entity, nil
}

// signRelease returns the InRelease (clearsigned) and Release.gpg (detached,
// armored) forms of one Release text.
func (s *Repository) signRelease(e *openpgp.Entity, release []byte) (inRelease, detached []byte, err error) {
	var in bytes.Buffer
	w, err := clearsign.Encode(&in, e.PrivateKey, s.cfg())
	if err != nil {
		return nil, nil, err
	}
	if _, err = w.Write(release); err != nil {
		return nil, nil, err
	}
	if err = w.Close(); err != nil {
		return nil, nil, err
	}
	var d bytes.Buffer
	if err = openpgp.ArmoredDetachSign(&d, e, bytes.NewReader(release), s.cfg()); err != nil {
		return nil, nil, err
	}
	return in.Bytes(), d.Bytes(), nil
}

// publicKey exports the armored public key for apt's signed-by setting.
func publicKey(e *openpgp.Entity, binary bool) ([]byte, error) {
	var b bytes.Buffer
	if binary {
		if err := e.Serialize(&b); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
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
