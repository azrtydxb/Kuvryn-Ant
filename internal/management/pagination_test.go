package management

import (
	"net/url"
	"strings"
	"testing"
)

func TestPageBounds(t *testing.T) {
	for _, query := range []string{"limit=0", "limit=201", "limit=a", "limit=1&limit=2", "unknown=x", "prefix=" + strings.Repeat("x", 201), "cursor=" + strings.Repeat("x", 2049)} {
		q, _ := url.ParseQuery(query)
		if _, err := ParsePage(q); err == nil {
			t.Fatalf("accepted %q", query)
		}
	}
	p, err := ParsePage(url.Values{})
	if err != nil || p.Limit != 50 {
		t.Fatalf("default %+v: %v", p, err)
	}
	p, err = ParsePage(url.Values{"prefix": {strings.Repeat("é", 200)}, "limit": {"200"}})
	if err != nil || p.Limit != 200 {
		t.Fatalf("boundary %+v: %v", p, err)
	}
}
func TestCursorBindingAndTampering(t *testing.T) {
	c := NewCursorCodec(make([]byte, 32))
	scope := "principal:1/feed:2/prefix:abc"
	token, err := c.Encode(scope, "last-id")
	if err != nil {
		t.Fatal(err)
	}
	last, err := c.Decode(scope, token)
	if err != nil || last != "last-id" {
		t.Fatalf("round trip %q %v", last, err)
	}
	if _, err = c.Decode("principal:2/feed:2/prefix:abc", token); err == nil {
		t.Fatal("cross-principal cursor accepted")
	}
	if _, err = c.Decode("principal:1/feed:2/prefix:other", token); err == nil {
		t.Fatal("cross-filter cursor accepted")
	}
	for _, bad := range []string{"invalid", token + "a", "a" + token, strings.Repeat("a", 2049)} {
		if _, err := c.Decode(scope, bad); err == nil {
			t.Fatal("tampered cursor accepted")
		}
	}
	other := NewCursorCodec([]byte(strings.Repeat("1", 32)))
	if _, err := other.Decode(scope, token); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestCursorCodecFailsClosedWithoutKey(t *testing.T) {
	for _, codec := range []CursorCodec{{}, NewCursorCodec(nil), NewCursorCodec([]byte("short"))} {
		if _, err := codec.Encode("scope", "id"); err == nil {
			t.Fatal("unconfigured cursor codec signed data")
		}
		if _, err := codec.Decode("scope", "data.signature"); err == nil {
			t.Fatal("unconfigured cursor codec accepted data")
		}
	}
}
