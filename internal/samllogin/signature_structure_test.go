package samllogin

import (
	"context"
	"errors"
	"testing"

	"github.com/beevik/etree"
)

// GO-2026-4753 / GHSA-479m-364c-43vc involves selecting the wrong
// SignedInfo Reference. Test the structural gate directly so rejection cannot
// accidentally be attributed to the invalidated SignatureValue after mutation.
// These are structural regressions, not a cryptographically re-signed exploit.
func TestSignatureReferenceStructure(t *testing.T) {
	p, signer, pending := fixture(t)
	login := response(t, p, signer, pending, assertion(p, pending))
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(login); err != nil {
		t.Fatal(err)
	}
	login = document(t, signed(t, signer, doc.Root()))
	_, logoutPending, err := p.BeginLogout(Identity{Issuer: p.cfg.IDPEntityID, Subject: "user"}, "state")
	if err != nil {
		t.Fatal(err)
	}
	logout := logoutResponse(t, p, signer, logoutPending, nil)
	cases := []struct {
		name, root, path string
		raw              []byte
		consume          func([]byte) error
	}{
		{"assertion", "Response", "./Assertion/Signature", login, func(raw []byte) error { _, err := p.Consume(context.Background(), raw, pending); return err }},
		{"response", "Response", "./Signature", login, func(raw []byte) error { _, err := p.Consume(context.Background(), raw, pending); return err }},
		{"logout", "LogoutResponse", "./Signature", logout, func(raw []byte) error { return p.ConsumeLogout(context.Background(), raw, logoutPending) }},
	}
	mutations := map[string]func(*etree.Element){
		"duplicate reference": func(si *etree.Element) { si.AddChild(si.FindElement("./Reference").Copy()) },
		"different reference last": func(si *etree.Element) {
			r := si.FindElement("./Reference").Copy()
			r.CreateAttr("URI", "#other")
			si.AddChild(r)
		},
		"different reference first": func(si *etree.Element) {
			r := si.FindElement("./Reference").Copy()
			r.CreateAttr("URI", "#other")
			si.InsertChildAt(0, r)
		},
		"no reference": func(si *etree.Element) { si.RemoveChild(si.FindElement("./Reference")) },
		"wrapped reference": func(si *etree.Element) {
			r := si.FindElement("./Reference")
			si.RemoveChild(r)
			si.CreateElement("wrapper").AddChild(r)
		},
		"foreign reference namespace": func(si *etree.Element) {
			r := si.FindElement("./Reference")
			r.Space = "foreign"
			r.CreateAttr("xmlns:foreign", "urn:foreign")
		},
		"duplicate SignedInfo": func(si *etree.Element) { si.Parent().AddChild(si.Copy()) },
		"no SignedInfo":        func(si *etree.Element) { si.Parent().RemoveChild(si) },
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := safeXML(c.raw, c.root, protocolNS); err != nil {
				t.Fatalf("valid signed fixture rejected: %v", err)
			}
			for name, mutate := range mutations {
				t.Run(name, func(t *testing.T) {
					d := etree.NewDocument()
					if err := d.ReadFromBytes(c.raw); err != nil {
						t.Fatal(err)
					}
					si := d.Root().FindElement(c.path + "/SignedInfo")
					if si == nil {
						t.Fatal("fixture lacks SignedInfo")
					}
					mutate(si)
					raw := document(t, d.Root())
					if err := safeXML(raw, c.root, protocolNS); !errors.Is(err, ErrInvalid) {
						t.Fatalf("structural gate accepted invalid signature shape: %v", err)
					}
					if err := c.consume(raw); !errors.Is(err, ErrInvalid) {
						t.Fatalf("callback did not reject shape: %v", err)
					}
				})
			}
		})
	}
	// Positive controls verify real signatures with the currently selected dependency.
	if _, err := p.Consume(context.Background(), login, pending); err != nil {
		t.Fatalf("valid doubly signed login: %v", err)
	}
	if err := p.ConsumeLogout(context.Background(), logout, logoutPending); err != nil {
		t.Fatalf("valid signed logout: %v", err)
	}
}
