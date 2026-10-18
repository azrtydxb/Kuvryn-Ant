// Package secrets encrypts secret records using externally supplied, versioned
// root keys. It does not load keys, persist records, or run rotation jobs.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"regexp"
	"unicode/utf8"
)

const (
	envelopeVersion = 1
	maxSecretBytes  = 1 << 20
)

var (
	ErrKeyConfiguration = errors.New("invalid root key configuration")
	ErrUnknownVersion   = errors.New("required root key version unavailable")
	ErrInvalidContext   = errors.New("invalid secret record context")
	ErrInvalidEnvelope  = errors.New("invalid encrypted secret envelope")
	ErrAuthentication   = errors.New("encrypted secret authentication failed")
	ErrSecretTooLarge   = errors.New("secret exceeds maximum size")
	ErrEncryption       = errors.New("secret encryption failed")
)
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Context must be supplied from trusted record metadata on every operation.
// Never trust record identity or purpose taken from an attacker-provided envelope.
// Include the instance/tenant identity in RecordID when IDs are not globally unique.
type Context struct{ RecordID, Purpose string }

// Envelope contains encrypted data and an encrypted per-record data key, never
// plaintext key material. JSON encoding uses base64 for the byte slices.
type Envelope struct {
	Version    int    `json:"version"`
	KeyVersion string `json:"key_version"`
	WrappedKey []byte `json:"wrapped_key"`
	WrapNonce  []byte `json:"wrap_nonce"`
	Ciphertext []byte `json:"ciphertext"`
	Nonce      []byte `json:"nonce"`
}

// Keyring is immutable after construction and safe for concurrent use. Keys must
// come from external key configuration, not from the database holding envelopes.
// Build a new keyring with active and previous versions for staged rotation;
// remove a previous version only after all dependent records are rewrapped.
type Keyring struct {
	active string
	roots  map[string][32]byte
}

// String and GoString prevent ordinary diagnostic formatting from exposing keys.
func (Keyring) String() string   { return "secrets.Keyring{redacted}" }
func (Keyring) GoString() string { return "secrets.Keyring{redacted}" }

func New(active string, keys map[string][]byte) (*Keyring, error) {
	if !versionPattern.MatchString(active) || len(keys) == 0 {
		return nil, ErrKeyConfiguration
	}
	ring := &Keyring{active: active, roots: make(map[string][32]byte, len(keys))}
	for version, key := range keys {
		if !versionPattern.MatchString(version) || len(key) != 32 {
			return nil, ErrKeyConfiguration
		}
		var copyKey [32]byte
		copy(copyKey[:], key)
		ring.roots[version] = copyKey
	}
	if _, ok := ring.roots[active]; !ok {
		return nil, ErrKeyConfiguration
	}
	return ring, nil
}
func validContext(ctx Context) bool {
	return utf8.ValidString(ctx.RecordID) && utf8.ValidString(ctx.Purpose) && len(ctx.RecordID) > 0 && len(ctx.RecordID) <= 1024 && len(ctx.Purpose) > 0 && len(ctx.Purpose) <= 256
}
func associatedData(ctx Context, domain, version string) []byte {
	// JSON array boundaries avoid ambiguous concatenation. Separate domains keep
	// wrapped-key and payload authentication independent. The root version binds
	// the wrapped key, allowing rewrapping without re-encrypting payload bytes.
	encoded, _ := json.Marshal([]string{"kuvryn-ant.secret.v1", domain, ctx.RecordID, ctx.Purpose, version})
	return encoded
}
func aead(key []byte) cipher.AEAD {
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	return gcm
}
func encrypt(key, plain, aad []byte) (nonce, ciphertext []byte, err error) {
	gcm := aead(key)
	nonce = make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, ErrEncryption
	}
	return nonce, gcm.Seal(nil, nonce, plain, aad), nil
}
func (k *Keyring) root(version string) ([32]byte, error) {
	if k == nil {
		return [32]byte{}, ErrKeyConfiguration
	}
	root, ok := k.roots[version]
	if !ok {
		return [32]byte{}, ErrUnknownVersion
	}
	return root, nil
}

// Seal creates a fresh random 256-bit data key and distinct random GCM nonces.
// Secrets are limited to 1 MiB. It does not retain plaintext or mutate the caller's plaintext slice.
func (k *Keyring) Seal(ctx Context, plaintext []byte) (Envelope, error) {
	if !validContext(ctx) {
		return Envelope{}, ErrInvalidContext
	}
	if len(plaintext) > maxSecretBytes {
		return Envelope{}, ErrSecretTooLarge
	}
	if k == nil {
		return Envelope{}, ErrKeyConfiguration
	}
	root, err := k.root(k.active)
	if err != nil {
		return Envelope{}, err
	}
	defer clear(root[:])
	var dataKey [32]byte
	if _, err = rand.Read(dataKey[:]); err != nil {
		return Envelope{}, ErrEncryption
	}
	defer clear(dataKey[:])
	result := Envelope{Version: envelopeVersion, KeyVersion: k.active}
	result.Nonce, result.Ciphertext, err = encrypt(dataKey[:], plaintext, associatedData(ctx, "payload", ""))
	if err != nil {
		return Envelope{}, err
	}
	result.WrapNonce, result.WrappedKey, err = encrypt(root[:], dataKey[:], associatedData(ctx, "data-key", k.active))
	if err != nil {
		return Envelope{}, err
	}
	return result, nil
}
func (k *Keyring) unwrap(ctx Context, e Envelope) ([]byte, error) {
	if !validContext(ctx) {
		return nil, ErrInvalidContext
	}
	if e.Version != envelopeVersion || !versionPattern.MatchString(e.KeyVersion) || len(e.WrapNonce) != 12 || len(e.Nonce) != 12 || len(e.WrappedKey) != 48 || len(e.Ciphertext) < 16 || len(e.Ciphertext) > maxSecretBytes+16 {
		return nil, ErrInvalidEnvelope
	}
	root, err := k.root(e.KeyVersion)
	if err != nil {
		return nil, err
	}
	defer clear(root[:])
	key, err := aead(root[:]).Open(nil, e.WrapNonce, e.WrappedKey, associatedData(ctx, "data-key", e.KeyVersion))
	if err != nil {
		return nil, ErrAuthentication
	}
	return key, nil
}

// Open authenticates both context and ciphertext before returning plaintext.
// The caller owns the returned plaintext and should clear it when finished.
func (k *Keyring) Open(ctx Context, e Envelope) ([]byte, error) {
	key, err := k.unwrap(ctx, e)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	plaintext, err := aead(key).Open(nil, e.Nonce, e.Ciphertext, associatedData(ctx, "payload", ""))
	if err != nil {
		return nil, ErrAuthentication
	}
	return plaintext, nil
}

// Rewrap authenticates the complete record and encrypts its data key under the
// active root version. Payload ciphertext is unchanged. Persist the returned
// envelope atomically with a revision check; this package performs no storage.
func (k *Keyring) Rewrap(ctx Context, e Envelope) (Envelope, error) {
	key, err := k.unwrap(ctx, e)
	if err != nil {
		return Envelope{}, err
	}
	defer clear(key)
	plain, err := aead(key).Open(nil, e.Nonce, e.Ciphertext, associatedData(ctx, "payload", ""))
	if err != nil {
		return Envelope{}, ErrAuthentication
	}
	clear(plain)
	root, err := k.root(k.active)
	if err != nil {
		return Envelope{}, err
	}
	defer clear(root[:])
	nonce, wrapped, err := encrypt(root[:], key, associatedData(ctx, "data-key", k.active))
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Version: envelopeVersion, KeyVersion: k.active, WrapNonce: nonce, WrappedKey: wrapped, Nonce: append([]byte(nil), e.Nonce...), Ciphertext: append([]byte(nil), e.Ciphertext...)}, nil
}
