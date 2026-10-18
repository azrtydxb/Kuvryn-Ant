package cargo

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
)

func TestNamesVersionsAndLayout(t *testing.T) {
	for name, want := range map[string]string{"a": "1/a", "ab": "2/ab", "abc": "3/a/abc", "cargo": "ca/rg/cargo"} {
		if indexPath(name) != want {
			t.Fatal(name)
		}
	}
	for _, v := range []string{"1.2.3", "0.0.0-alpha.1+build.2"} {
		if !validVersion(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"v1.2.3", "1.2", "01.0.0", "1.2.3-01", "1.2.3/evil"} {
		if validVersion(v) {
			t.Fatal(v)
		}
	}
	for _, name := range []string{"Foo", "foo_bar", "../x", ""} {
		if nameRE.MatchString(name) {
			t.Fatal(name)
		}
	}
}
func TestWireAndArchiveValidation(t *testing.T) {
	m := metadata{Name: "demo", Version: "1.0.0"}
	crate := crateArchive(t, m.Name, m.Version, "")
	body := wire(m, crate)
	r := bytes.NewReader(body)
	got, e := readMetadata(r)
	if e != nil {
		t.Fatal(e)
	}
	f, info, e := readCrate(r, got)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	os.Remove(f.Name())
	if info != bytesInfo(crate) {
		t.Fatal("digest")
	}
	for _, bad := range [][]byte{body[:len(body)-1], append(append([]byte{}, body...), 1), wire(m, []byte("bad")), wire(m, crateArchive(t, m.Name, m.Version, "demo-1.0.0/../../escape"))} {
		r := bytes.NewReader(bad)
		m, e := readMetadata(r)
		if e != nil {
			continue
		}
		f, _, e := readCrate(r, m)
		if e == nil {
			f.Close()
			os.Remove(f.Name())
			t.Fatal("accepted malformed archive")
		}
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(maxMetadata+1))
	if _, e := readMetadata(&b); e == nil {
		t.Fatal("unbounded metadata")
	}
}
func TestDependencyTranslation(t *testing.T) {
	rename := "alias"
	m := metadata{Name: "demo", Version: "1.0.0", Deps: []dependency{{Name: "original", Rename: &rename, Requirement: "^1", DefaultFeatures: true}}, Features: map[string][]string{"extra": {"dep:alias"}}}
	var row map[string]any
	if e := json.Unmarshal(bytes.TrimSpace(m.index(bytesInfo([]byte("crate")))), &row); e != nil {
		t.Fatal(e)
	}
	d := row["deps"].([]any)[0].(map[string]any)
	if d["name"] != "alias" || d["package"] != "original" || d["req"] != "^1" {
		t.Fatal(d)
	}
}
func TestManifestIdentity(t *testing.T) {
	m := metadata{Name: "demo", Version: "1.0.0"}
	for _, text := range []string{"[package]\nname='demo'\nversion='1.0.0'", "[package] # comment\nname = \"demo\" # comment\nversion=\"1.0.0\""} {
		if !manifestIdentity([]byte(text), m) {
			t.Fatal(text)
		}
	}
	for _, text := range []string{"[package]\nname='other'\nversion='1.0.0'", "[package]\nname='demo'\nname='demo'\nversion='1.0.0'", "[package]\nname='demo'\nversion.workspace=true"} {
		if manifestIdentity([]byte(text), m) {
			t.Fatal(text)
		}
	}
}

func TestArchiveValidationStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := bytes.NewReader([]byte("unread archive"))
	err := validateArchiveContext(ctx, reader, metadata{Name: "demo", Version: "1.2.3"}, 1000)
	if err == nil || reader.Len() != len("unread archive") {
		t.Fatal("cancelled validation read archive")
	}
}

func TestPublicOriginValidation(t *testing.T) {
	s := new(Registry)
	for _, u := range []string{"http://registry.example", "https://user:secret@registry.example", "https://registry.example/path", "https://registry.example?x=1", "//registry.example"} {
		if s.SetPublicURL(u) == nil {
			t.Fatal("accepted", u)
		}
	}
	for _, u := range []string{"https://registry.example", "http://127.0.0.1:4000", "http://[::1]:4000"} {
		if err := s.SetPublicURL(u); err != nil {
			t.Fatal(err)
		}
	}
}
