package federation

import (
	"errors"
	"strings"
	"testing"
)

// malleate sets the unused trailing bits of an unpadded base64url value: the
// lenient decoder maps the result to the same bytes.
func malleate(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, s[len(s)-1])
	return s[:len(s)-1] + string(alphabet[i|1])
}

// One signature must have exactly one accepted spelling: the chain hash
// covers the signature string, so a relay that re-spells it would otherwise
// fork the chain of every member that applied its copy.
func TestDocumentSignatureSpellingIsUnique(t *testing.T) {
	f := newTestFed(t)
	g1 := f.docs[0]
	g2 := f.next(t, addB)
	variant := *g2
	sig := *g2.Signature
	sig.Value = malleate(sig.Value)
	variant.Signature = &sig
	if Hash(&variant) == Hash(g2) {
		t.Fatal("precondition: spellings hash differently")
	}
	if err := Verify(&variant, g1); !errors.Is(err, ErrSignature) {
		t.Fatalf("membership with a re-spelled signature: %v", err)
	}
	cert := *g2
	cert.Authority.Signature = malleate(cert.Authority.Signature)
	if err := Verify(&cert, g1); !errors.Is(err, ErrAuthorityCert) {
		t.Fatalf("re-spelled authority certificate signature: %v", err)
	}
	if _, err := decodeKey(malleate(g1.RecoveryPublicKey)); err == nil {
		t.Fatal("re-spelled public key accepted")
	}
	if _, err := parseRecovery(malleate(exportRecovery(f.recovery))); err == nil {
		t.Fatal("re-spelled recovery key export accepted")
	}

	m, _, docs := signedChain(t, 2)
	p := docs[1]
	ps := *p.Signature
	ps.Value = malleate(ps.Value)
	p.Signature = &ps
	if err := VerifyPolicy(&p, &docs[0], m); !errors.Is(err, ErrSignature) {
		t.Fatalf("policy with a re-spelled signature: %v", err)
	}
}
