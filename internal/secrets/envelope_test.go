package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func ring(t *testing.T, active string, keys map[string][]byte) *Keyring {
	t.Helper()
	k, err := New(active, keys)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
func clone(e Envelope) Envelope {
	e.WrappedKey = bytes.Clone(e.WrappedKey)
	e.WrapNonce = bytes.Clone(e.WrapNonce)
	e.Ciphertext = bytes.Clone(e.Ciphertext)
	e.Nonce = bytes.Clone(e.Nonce)
	return e
}

func TestEnvelopeRoundTripAndRandomness(t *testing.T) {
	root := bytes.Repeat([]byte{9}, 32)
	k := ring(t, "root-1", map[string][]byte{"root-1": root})
	// Construction copies external keys; caller ownership is retained.
	clear(root)
	ctx := Context{RecordID: "instance/record", Purpose: "upstream-password"}
	secret := []byte("sensitive-password-keep-private")
	first, err := k.Seal(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := k.Seal(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Ciphertext, second.Ciphertext) || bytes.Equal(first.WrappedKey, second.WrappedKey) || bytes.Equal(first.Nonce, second.Nonce) || bytes.Equal(first.WrapNonce, second.WrapNonce) {
		t.Fatal("reused encrypted material")
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, secret) || bytes.Contains(first.Ciphertext, secret) || bytes.Contains(first.WrappedKey, secret) {
		t.Fatal("plaintext stored")
	}
	var restored Envelope
	if err = json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	actual, err := k.Open(ctx, restored)
	if err != nil || !bytes.Equal(actual, secret) {
		t.Fatalf("roundtrip failed: %v", err)
	}
	clear(actual)
	if string(secret) != "sensitive-password-keep-private" {
		t.Fatal("caller plaintext modified")
	}
	empty, err := k.Seal(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	actual, err = k.Open(ctx, empty)
	if err != nil || len(actual) != 0 {
		t.Fatal("empty secret roundtrip failed")
	}
}

func TestEnvelopeAuthenticatesEveryFieldAndContext(t *testing.T) {
	k := ring(t, "one", map[string][]byte{"one": bytes.Repeat([]byte{1}, 32), "alias": bytes.Repeat([]byte{1}, 32)})
	ctx := Context{"record", "purpose"}
	sealed, err := k.Seal(ctx, []byte("hidden-secret"))
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func(*Envelope){
		"ciphertext":             func(e *Envelope) { e.Ciphertext[0] ^= 1 },
		"data nonce":             func(e *Envelope) { e.Nonce[0] ^= 1 },
		"wrapped key":            func(e *Envelope) { e.WrappedKey[0] ^= 1 },
		"wrap nonce":             func(e *Envelope) { e.WrapNonce[0] ^= 1 },
		"same-key version alias": func(e *Envelope) { e.KeyVersion = "alias" },
		"unknown version":        func(e *Envelope) { e.KeyVersion = "missing" },
		"format version":         func(e *Envelope) { e.Version++ },
		"short nonce":            func(e *Envelope) { e.Nonce = e.Nonce[:1] },
		"short wrap nonce":       func(e *Envelope) { e.WrapNonce = e.WrapNonce[:1] },
		"short wrapped key":      func(e *Envelope) { e.WrappedKey = e.WrappedKey[:1] },
		"short ciphertext":       func(e *Envelope) { e.Ciphertext = e.Ciphertext[:1] },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			e := clone(sealed)
			change(&e)
			if plain, err := k.Open(ctx, e); err == nil || len(plain) != 0 {
				t.Fatal("tampered envelope decrypted")
			}
			if _, err := k.Rewrap(ctx, e); err == nil {
				t.Fatal("tampered envelope rotated")
			}
		})
	}
	for _, other := range []Context{{"other", "purpose"}, {"record", "other"}, {"recordpurpose", "x"}, {"rec", "ordpurpose"}} {
		if _, err = k.Open(other, sealed); !errors.Is(err, ErrAuthentication) {
			t.Fatalf("swapped context: %v", err)
		}
	}
	wrong := ring(t, "one", map[string][]byte{"one": bytes.Repeat([]byte{2}, 32)})
	if _, err = wrong.Open(ctx, sealed); !errors.Is(err, ErrAuthentication) {
		t.Fatal("wrong root accepted")
	}
	// Swapping complete encrypted payload parts across records remains forbidden.
	other, err := k.Seal(Context{"other-record", "purpose"}, []byte("other-secret"))
	if err != nil {
		t.Fatal(err)
	}
	swapped := clone(sealed)
	swapped.Nonce = other.Nonce
	swapped.Ciphertext = other.Ciphertext
	if _, err = k.Open(ctx, swapped); !errors.Is(err, ErrAuthentication) {
		t.Fatal("swapped payload accepted")
	}
}

func TestEnvelopeRootRotation(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	old := ring(t, "old", map[string][]byte{"old": oldKey})
	rotating := ring(t, "new", map[string][]byte{"old": oldKey, "new": newKey})
	retired := ring(t, "new", map[string][]byte{"new": newKey})
	ctx := Context{"record", "purpose"}
	original, err := old.Seal(ctx, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := rotating.Rewrap(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.KeyVersion != "new" || !bytes.Equal(original.Ciphertext, rotated.Ciphertext) || !bytes.Equal(original.Nonce, rotated.Nonce) || bytes.Equal(original.WrappedKey, rotated.WrappedKey) {
		t.Fatal("invalid rotation")
	}
	plain, err := retired.Open(ctx, rotated)
	if err != nil || string(plain) != "secret" {
		t.Fatal("new key cannot decrypt rotated record", err)
	}
	clear(plain)
	if _, err = retired.Open(ctx, original); !errors.Is(err, ErrUnknownVersion) {
		t.Fatal("retired root still available")
	}
	if _, err = old.Open(ctx, rotated); !errors.Is(err, ErrUnknownVersion) {
		t.Fatal("old ring decrypted new root")
	}
	// Outputs own their buffers; caller edits must not mutate the original.
	rotated.Ciphertext[0] ^= 1
	if _, err = old.Open(ctx, original); err != nil {
		t.Fatal("rotation aliased caller envelope")
	}
}

func TestEnvelopeRejectsBadInputs(t *testing.T) {
	for _, keys := range []map[string][]byte{nil, {"v": make([]byte, 16)}, {"v": make([]byte, 33)}, {"bad version": make([]byte, 32)}, {"other": make([]byte, 32)}} {
		if _, err := New("v", keys); !errors.Is(err, ErrKeyConfiguration) {
			t.Fatal("invalid keyring accepted")
		}
	}
	k := ring(t, "v", map[string][]byte{"v": make([]byte, 32)})
	for _, ctx := range []Context{{}, {"id", ""}, {"", "purpose"}, {strings.Repeat("x", 1025), "purpose"}, {"id", string([]byte{0xff})}} {
		if _, err := k.Seal(ctx, nil); !errors.Is(err, ErrInvalidContext) {
			t.Fatal("invalid context accepted")
		}
	}
	if _, err := k.Seal(Context{"id", "purpose"}, make([]byte, maxSecretBytes+1)); !errors.Is(err, ErrSecretTooLarge) {
		t.Fatal("oversize secret accepted")
	}
	var nilRing *Keyring
	if _, err := nilRing.Seal(Context{"id", "purpose"}, nil); !errors.Is(err, ErrKeyConfiguration) {
		t.Fatal("nil keyring accepted")
	}
}

func TestEnvelopeConcurrentUse(t *testing.T) {
	k := ring(t, "v", map[string][]byte{"v": bytes.Repeat([]byte{5}, 32)})
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := Context{"record", "purpose"}
			e, err := k.Seal(ctx, []byte("secret"))
			if err == nil {
				var plain []byte
				plain, err = k.Open(ctx, e)
				if err == nil && !bytes.Equal(plain, []byte("secret")) {
					err = errors.New("incorrect plaintext")
				}
				clear(plain)
			}
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeyringDiagnosticFormattingIsRedacted(t *testing.T) {
	k := ring(t, "private-version", map[string][]byte{"private-version": bytes.Repeat([]byte{99}, 32)})
	for _, value := range []string{fmt.Sprintf("%v", k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k), fmt.Sprintf("%+v", *k)} {
		if value != "secrets.Keyring{redacted}" {
			t.Fatal("keyring diagnostic formatting exposed configuration")
		}
	}
}
