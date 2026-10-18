package rpmrepo

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
)

type spec struct {
	name, version, release, arch string
	epoch                        *uint32
	requires                     [][3]string // name, flags-letters, version
	provides                     [][3]string
	files                        []string
	payload                      string
	noDigests                    bool
}

type tagData struct {
	tag, typ, count uint32
	data            []byte
	align           int
}

func str(tag uint32, s string) tagData {
	return tagData{tag, typeString, 1, append([]byte(s), 0), 1}
}
func strArray(tag uint32, v []string) tagData {
	var b []byte
	for _, s := range v {
		b = append(b, s...)
		b = append(b, 0)
	}
	return tagData{tag, typeStringArray, uint32(len(v)), b, 1}
}
func int32s(tag uint32, v ...uint32) tagData {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint32(b[i*4:], x)
	}
	return tagData{tag, typeInt32, uint32(len(v)), b, 4}
}
func binTag(tag uint32, v []byte) tagData { return tagData{tag, typeBin, uint32(len(v)), v, 1} }

func encodeHeader(tags []tagData) []byte {
	sort.Slice(tags, func(i, j int) bool { return tags[i].tag < tags[j].tag })
	var data bytes.Buffer
	var index bytes.Buffer
	for _, t := range tags {
		for data.Len()%t.align != 0 {
			data.WriteByte(0)
		}
		var e [16]byte
		binary.BigEndian.PutUint32(e[0:], t.tag)
		binary.BigEndian.PutUint32(e[4:], t.typ)
		binary.BigEndian.PutUint32(e[8:], uint32(data.Len()))
		binary.BigEndian.PutUint32(e[12:], t.count)
		index.Write(e[:])
		data.Write(t.data)
	}
	var out bytes.Buffer
	out.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})
	var n [8]byte
	binary.BigEndian.PutUint32(n[0:], uint32(len(tags)))
	binary.BigEndian.PutUint32(n[4:], uint32(data.Len()))
	out.Write(n[:])
	out.Write(index.Bytes())
	out.Write(data.Bytes())
	return out.Bytes()
}

func depTags(nameTag, flagTag, versTag uint32, deps [][3]string) []tagData {
	if len(deps) == 0 {
		return nil
	}
	var names, vers []string
	var flags []uint32
	for _, d := range deps {
		names = append(names, d[0])
		vers = append(vers, d[2])
		var f uint32
		if strings.Contains(d[1], "G") {
			f |= senseGreater
		}
		if strings.Contains(d[1], "L") {
			f |= senseLess
		}
		if strings.Contains(d[1], "E") {
			f |= senseEqual
		}
		if strings.HasPrefix(d[0], "rpmlib(") {
			f |= senseRPMLib
		}
		flags = append(flags, f)
	}
	return []tagData{strArray(nameTag, names), int32s(flagTag, flags...), strArray(versTag, vers)}
}

func buildRPM(s spec) []byte {
	if s.arch == "" {
		s.arch = "x86_64"
	}
	tags := []tagData{str(tagName, s.name), str(tagVersion, s.version), str(tagRelease, s.release), str(tagArch, s.arch),
		str(tagSummary, "Summary of "+s.name+" <&>"), str(tagDescription, "Description"), int32s(tagBuildTime, 1700000000),
		int32s(tagSize, 1234), str(tagLicense, "MIT"), str(tagSourceRPM, s.name+"-"+s.version+"-"+s.release+".src.rpm")}
	if s.epoch != nil {
		tags = append(tags, int32s(tagEpoch, *s.epoch))
	}
	tags = append(tags, depTags(tagRequireName, tagRequireFlags, tagRequireVersion, append([][3]string{{"rpmlib(CompressedFileNames)", "LE", "3.0.4-1"}}, s.requires...))...)
	tags = append(tags, depTags(tagProvideName, tagProvideFlags, tagProvideVersion, append([][3]string{{s.name, "E", "0:" + s.version + "-" + s.release}}, s.provides...))...)
	if len(s.files) > 0 {
		var base, dirs []string
		var idx, modes, sizes, fflags []uint32
		seen := map[string]uint32{}
		for _, f := range s.files {
			i := strings.LastIndexByte(f, '/')
			d := f[:i+1]
			if _, ok := seen[d]; !ok {
				seen[d] = uint32(len(dirs))
				dirs = append(dirs, d)
			}
			base = append(base, f[i+1:])
			idx = append(idx, seen[d])
			modes = append(modes, 0o100644)
			sizes = append(sizes, 1)
			fflags = append(fflags, 0)
		}
		tags = append(tags, strArray(tagBaseNames, base), strArray(tagDirNames, dirs), int32s(tagDirIndexes, idx...), int32s(tagFileMode, modes...), int32s(tagFileSizes, sizes...), int32s(tagFileFlags, fflags...))
	}
	tags = append(tags, int32s(tagChangeTime, 1699999999), strArray(tagChangeName, []string{"Dev <dev@example.org>"}), strArray(tagChangeTxt, []string{"- initial"}))
	hdr := encodeHeader(tags)
	payload := []byte(s.payload)
	if payload == nil {
		payload = []byte("payload-bytes")
	}
	var sig []byte
	if s.noDigests {
		sig = encodeHeader([]tagData{int32s(sigSize, uint32(len(hdr)+len(payload)))})
	} else {
		sum := sha256.Sum256(hdr)
		m := md5.New()
		m.Write(hdr)
		m.Write(payload)
		sig = encodeHeader([]tagData{int32s(sigSize, uint32(len(hdr)+len(payload))), binTag(sigMD5, m.Sum(nil)), str(sigSHA256, hex.EncodeToString(sum[:]))})
	}
	lead := make([]byte, leadSize)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0})
	var out bytes.Buffer
	out.Write(lead)
	out.Write(sig)
	for out.Len()%8 != 0 {
		out.WriteByte(0)
	}
	out.Write(hdr)
	out.Write(payload)
	return out.Bytes()
}

func parseBytes(b []byte) (*Package, error) { return parseRPM(bytes.NewReader(b), int64(len(b))) }

func TestParseRPM(t *testing.T) {
	epoch := uint32(2)
	data := buildRPM(spec{name: "demo-tool", version: "1.2.3", release: "4.el9", epoch: &epoch,
		requires: [][3]string{{"libdemo", "GE", "1:2.0-3"}, {"/bin/sh", "", ""}}, files: []string{"/usr/bin/demo", "/etc/demo.conf", "/usr/share/doc/demo/README"}})
	p, err := parseBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "demo-tool" || p.Version != "1.2.3" || p.Release != "4.el9" || p.Arch != "x86_64" || p.Epoch != "2" || !p.HasEpoch || p.BuildTime != 1700000000 {
		t.Fatalf("%+v", p)
	}
	if p.Filename() != "demo-tool-1.2.3-4.el9.x86_64.rpm" || p.VersionKey() != "1.2.3-4.el9.x86_64" {
		t.Fatal(p.Filename())
	}
	for _, d := range p.Requires {
		if strings.HasPrefix(d.Name, "rpmlib(") {
			t.Fatal("rpmlib dependency must not be indexed")
		}
	}
	if len(p.Requires) != 2 || p.Requires[0].Flags != "GE" || p.Requires[0].Epoch != "1" || p.Requires[0].Version != "2.0" || p.Requires[0].Release != "3" {
		t.Fatalf("%+v", p.Requires)
	}
	if len(p.Provides) != 1 || p.Provides[0].Flags != "EQ" || p.Provides[0].Epoch != "0" || len(p.Files) != 3 || p.Files[0].Path != "/usr/bin/demo" || len(p.Changes) != 1 {
		t.Fatalf("%+v %+v", p.Provides, p.Files)
	}
	if p.HeaderStart%8 != 0 || p.HeaderEnd <= p.HeaderStart {
		t.Fatal(p.HeaderStart, p.HeaderEnd)
	}
	sum := strings.Repeat("a", 64)
	for name, doc := range map[string]string{"primary": p.primaryXML(sum, "Packages/x.rpm", 99), "filelists": p.filelistsXML(sum), "other": p.otherXML(sum)} {
		dec := xml.NewDecoder(strings.NewReader("<r xmlns:rpm=\"x\">" + doc + "</r>"))
		for {
			if _, err := dec.Token(); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatal(name, err)
				}
				break
			}
		}
	}
	primary := p.primaryXML(sum, "Packages/x.rpm", 99)
	if !strings.Contains(primary, "<file>/usr/bin/demo</file>") || !strings.Contains(primary, "/etc/demo.conf") || strings.Contains(primary, "README") {
		t.Fatal(primary)
	}
	if !strings.Contains(primary, "Summary of demo-tool &lt;&amp;&gt;") || !strings.Contains(primary, `<rpm:entry name="libdemo" flags="GE" epoch="1" ver="2.0" rel="3"/>`) {
		t.Fatal(primary)
	}
	if _, err = parseBytes(buildRPM(spec{name: "nodigest", version: "1", release: "1", noDigests: true})); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedRPM(t *testing.T) {
	good := buildRPM(spec{name: "demo", version: "1", release: "1", files: []string{"/usr/bin/demo"}})
	if _, err := parseBytes(good); err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		b := append([]byte(nil), good...)
		b[i] ^= 0xff
		return b
	}
	p, _ := parseBytes(good)
	cases := map[string][]byte{
		"empty":            nil,
		"short":            good[:50],
		"bad lead magic":   flip(0),
		"bad sig magic":    flip(leadSize),
		"truncated header": good[:p.HeaderStart+20],
		"truncated tail":   good[:len(good)-1],
		"flipped payload":  flip(len(good) - 1),
		"flipped header":   flip(int(p.HeaderEnd) - 3),
		"flipped name":     flip(int(p.HeaderStart) + 16 + 16*20 + 2),
		"appended bytes":   append(append([]byte(nil), good...), 'x'),
		"garbage":          bytes.Repeat([]byte{0xed, 0xab, 0xee, 0xdb}, 100),
	}
	// Absurd index counts and out-of-range entries must fail without allocating.
	huge := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(huge[leadSize+8:], 0xffffffff)
	cases["huge index"] = huge
	bigData := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(bigData[leadSize+12:], 0xffffffff)
	cases["huge data"] = bigData
	badOff := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(badOff[leadSize+16+8:], 0x7fffffff)
	cases["offset out of range"] = badOff
	for name, b := range cases {
		if _, err := parseBytes(b); !errors.Is(err, errMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A header whose strings are not NUL terminated within the store.
	h := encodeHeader([]tagData{{tagName, typeString, 1, []byte("abc"), 1}})
	if _, err := readHeader(bytes.NewReader(h)); err != nil {
		t.Fatal(err)
	}
	hd, _ := readHeader(bytes.NewReader(h))
	if _, err := hd.str(tagName); !errors.Is(err, errMalformed) {
		t.Fatal(err)
	}
	// Invalid identity values.
	for _, s := range []spec{{name: "bad name!", version: "1", release: "1"}, {name: "x", version: "1-2", release: "1"}, {name: "x", version: "1", release: "1", arch: "x/y"}} {
		if _, err := parseBytes(buildRPM(s)); !errors.Is(err, errMalformed) {
			t.Errorf("%+v: %v", s, err)
		}
	}
}

func TestParseFilename(t *testing.T) {
	n, ok := parseFilename("my-pkg-1.2-3.el9.noarch.rpm")
	if !ok || n.name != "my-pkg" || n.version != "1.2" || n.release != "3.el9" || n.arch != "noarch" {
		t.Fatalf("%+v", n)
	}
	for _, bad := range []string{"a.rpm", "a-1.x86_64.rpm", "a-1-2.rpm", "a-1-2.x86_64", "../a-1-2.x86_64.rpm", "a b-1-2.x86_64.rpm", "-1-2.x86_64.rpm"} {
		if _, ok := parseFilename(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
