package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCanonicalizeRFC8785(t *testing.T) {
	cases := map[string]string{
		`[56,{"d":true,"10":null,"1":[]}]`:                                              `[56,{"1":[],"10":null,"d":true}]`,
		`{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001]}`: `{"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27]}`,
		`{"string":"\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/"}`:                    `{"string":"€$\u000f\nA'B\"\\\\\"/"}`,
		`{"literals":[null,true,false]}`:                                                `{"literals":[null,true,false]}`,
		`{"\u20ac":1,"\r":2,"\ufb33":3,"1":4,"\ud83d\ude00":5,"\u0080":6,"\u00f6":7}`:   "{\"\\r\":2,\"1\":4,\"\u0080\":6,\"\u00f6\":7,\"\u20ac\":1,\"\U0001F600\":5,\"\ufb33\":3}",
		`  { "b" : [ 1 , 2.0 , -0 , 100000000000000000000 , 1e21 ] , "a" : { } }  `:     `{"a":{},"b":[1,2,0,100000000000000000000,1e+21]}`,
		`{"small":[1e-6,1e-7,123e-9,-1.5]}`:                                             `{"small":[0.000001,1e-7,1.23e-7,-1.5]}`,
	}
	for in, want := range cases {
		got, err := Canonicalize([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("Canonicalize(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{`{"a":1,"a":2}`, `{"a":1} x`, `[1,`, "\"\xff\""} {
		if _, err := Canonicalize([]byte(bad)); err == nil {
			t.Errorf("Canonicalize(%q) accepted", bad)
		}
	}
}

type testFed struct {
	recovery, authority ed25519.PrivateKey
	docs                []*Membership
}

func genKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pub(k ed25519.PrivateKey) string { return encodeKey(k.Public().(ed25519.PublicKey)) }

const (
	tfID  = "6f1e2d3c-4b5a-4968-8776-655443322110"
	tfA   = "11111111-2222-4333-8444-555555555555"
	tfB   = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	tfURL = "https://fed.example:8444"
)

func newTestFed(t *testing.T) *testFed {
	t.Helper()
	f := &testFed{recovery: genKey(t), authority: genKey(t)}
	ca, _, err := newCA("unit", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m := &Membership{Kind: "membership", FederationID: tfID, FederationName: "unit", Generation: 1, AuthorityEpoch: 1,
		Protocol: ProtocolRange{1, 1}, RecoveryPublicKey: pub(f.recovery), CACertificates: []string{ca.CertPEM}, RevokedCertSerials: []string{},
		Members:  []Member{{InstanceID: tfA, Name: "alpha", Status: "active", FederationURL: tfURL, SigningKey: pub(genKey(t)), Protocol: ProtocolRange{1, 1}}},
		IssuedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	m.Authority = AuthorityCert{Epoch: 1, InstanceID: tfA, PublicKey: pub(f.authority), CertifiedBy: "recovery"}
	m.Authority.Signature = sign(f.recovery, authorityStatement(tfID, m.Authority))
	if err := Sign(m, f.authority); err != nil {
		t.Fatal(err)
	}
	f.docs = []*Membership{m}
	return f
}

// next builds and signs the next generation after applying mutate.
func (f *testFed) next(t *testing.T, mutate func(*Membership)) *Membership {
	t.Helper()
	prev := f.docs[len(f.docs)-1]
	n := prev.Clone()
	n.Generation = prev.Generation + 1
	n.PrevHash = Hash(prev)
	if mutate != nil {
		mutate(n)
	}
	if err := Sign(n, f.authority); err != nil {
		t.Fatal(err)
	}
	return n
}

func addB(m *Membership) {
	m.Members = append(m.Members, Member{InstanceID: tfB, Name: "beta", Status: "active", FederationURL: "https://b.example:8444", SigningKey: m.Members[0].SigningKey, Protocol: ProtocolRange{1, 1}})
}

func TestVerifyChain(t *testing.T) {
	f := newTestFed(t)
	g1 := f.docs[0]
	if err := Verify(g1, nil); err != nil {
		t.Fatalf("generation 1: %v", err)
	}
	g2 := f.next(t, addB)
	if err := Verify(g2, g1); err != nil {
		t.Fatalf("generation 2: %v", err)
	}
	f.docs = append(f.docs, g2)
	g3 := f.next(t, func(m *Membership) {
		m.Members[1].Status = "removed"
		m.RevokedCertSerials = append(m.RevokedCertSerials, "abc")
	})
	if err := Verify(g3, g2); err != nil {
		t.Fatalf("generation 3: %v", err)
	}

	// A JSON round trip (as through jsonb) keeps the signature valid.
	raw, _ := CanonicalJSON(g3)
	var round Membership
	if err := json.Unmarshal(raw, &round); err != nil || Verify(&round, g2) != nil || Hash(&round) != Hash(g3) {
		t.Fatalf("round trip broke verification: %v", err)
	}

	check := func(name string, doc, applied *Membership, want error) {
		t.Helper()
		if err := Verify(doc, applied); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", name, err, want)
		}
	}
	// Tampering after signing.
	tampered := g3.Clone()
	tampered.Signature = g3.Signature
	tampered.Members[1].Status = "active"
	check("tampered", tampered, g2, ErrSignature)
	// Stale and replayed generations.
	check("replay of applied", g2, g2, ErrStaleGen)
	check("older generation", g2, g3, ErrStaleGen)
	// Gap: generation 3 on top of generation 1.
	check("gap", g3, g1, ErrGap)
	// Wrong prev_hash, correctly signed.
	forkPrev := f.next(t, func(m *Membership) {
		m.PrevHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	})
	check("chain", forkPrev, g2, ErrChain)
	// Generation 1 must start a chain.
	check("non-genesis without applied", g2, nil, ErrGap)
	// Signed by a key not certified by the recovery key.
	rogue := genKey(t)
	forged := f.docs[len(f.docs)-1].Clone()
	forged.Generation, forged.PrevHash = 3, Hash(g2)
	forged.Authority.PublicKey = pub(rogue)
	forged.Authority.Signature = sign(rogue, authorityStatement(tfID, forged.Authority))
	_ = Sign(forged, rogue)
	check("rogue authority", forged, g2, ErrAuthorityCert)
	// Another federation.
	other := newTestFed(t)
	other.docs[0].FederationID = "00000000-0000-4000-8000-000000000001"
	_ = Sign(other.docs[0], other.authority)
	check("wrong federation", other.docs[0], g1, ErrWrongFed)
	// Malformed: duplicate member.
	dup := f.next(t, func(m *Membership) { m.Members = append(m.Members, m.Members[0]) })
	check("duplicate member", dup, g2, ErrMalformed)
}

func TestRecoveryRotationAndEpochs(t *testing.T) {
	f := newTestFed(t)
	g1 := f.docs[0]
	newRec := genKey(t)
	rotate := func(signer ed25519.PrivateKey) *Membership {
		return f.next(t, func(m *Membership) {
			r := RecoveryRotation{PreviousKey: g1.RecoveryPublicKey, NewKey: pub(newRec), Generation: m.Generation}
			r.Signature = sign(signer, rotationStatement(tfID, r))
			m.RecoveryRotation, m.RecoveryPublicKey = &r, r.NewKey
			m.Authority.Signature = sign(newRec, authorityStatement(tfID, m.Authority))
		})
	}
	if err := Verify(rotate(f.recovery), g1); err != nil {
		t.Fatalf("valid rotation rejected: %v", err)
	}
	if err := Verify(rotate(genKey(t)), g1); !errors.Is(err, ErrRecoveryChange) {
		t.Fatalf("rotation signed by an unknown key: %v", err)
	}
	silent := f.next(t, func(m *Membership) {
		m.RecoveryPublicKey = pub(newRec)
		m.Authority.Signature = sign(newRec, authorityStatement(tfID, m.Authority))
	})
	if err := Verify(silent, g1); !errors.Is(err, ErrRecoveryChange) {
		t.Fatalf("unannounced recovery change: %v", err)
	}
	// Planned handover: epoch 2 certified by the epoch 1 key.
	epoch2 := genKey(t)
	g2 := g1.Clone()
	g2.Generation, g2.PrevHash, g2.AuthorityEpoch = 2, Hash(g1), 2
	g2.Authority = AuthorityCert{Epoch: 2, InstanceID: tfA, PublicKey: pub(epoch2), CertifiedBy: "previous_epoch"}
	g2.Authority.Signature = sign(f.authority, authorityStatement(tfID, g2.Authority))
	_ = Sign(g2, epoch2)
	if err := Verify(g2, g1); err != nil {
		t.Fatalf("planned handover rejected: %v", err)
	}
	// An epoch-1 document after epoch 2 was applied is stale.
	f.docs = []*Membership{g1}
	old := f.next(t, nil)
	old.Generation = 3
	old.PrevHash = Hash(g2)
	_ = Sign(old, f.authority)
	if err := Verify(old, g2); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("lower epoch accepted: %v", err)
	}
	// Same epoch, different key.
	sameEpoch := g1.Clone()
	sameEpoch.Generation, sameEpoch.PrevHash = 2, Hash(g1)
	k := genKey(t)
	sameEpoch.Authority.PublicKey = pub(k)
	sameEpoch.Authority.Signature = sign(f.recovery, authorityStatement(tfID, sameEpoch.Authority))
	_ = Sign(sameEpoch, k)
	if err := Verify(sameEpoch, g1); !errors.Is(err, ErrAuthorityCert) {
		t.Fatalf("key swap within an epoch accepted: %v", err)
	}
}

func TestRecoveryExport(t *testing.T) {
	k := genKey(t)
	back, err := parseRecovery(exportRecovery(k))
	if err != nil || !back.Equal(k) {
		t.Fatalf("recovery export round trip failed: %v", err)
	}
	if _, err := parseRecovery("ant-recovery-v1:short"); err == nil {
		t.Fatal("short recovery key accepted")
	}
}
