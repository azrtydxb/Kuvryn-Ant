package helmrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strings"
	"testing"
)

type member struct {
	name, body string
	kind       byte
}

func chartArchive(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		kind := m.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		size := int64(len(m.body))
		if kind == tar.TypeSymlink {
			size = 0
		}
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Size: size, Mode: 0644, Typeflag: kind, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func goodChart(t *testing.T, name, version string) []byte {
	return chartArchive(t, member{name + "/Chart.yaml", fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\ndescription: test chart\n", name, version), 0}, member{name + "/templates/configmap.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fixture\n", 0})
}
func provenance(data []byte, name, version string) []byte {
	return []byte(fmt.Sprintf("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\napiVersion: v2\nname: %s\nversion: %s\n...\nfiles:\n  %s-%s.tgz: sha256:%s\n-----BEGIN PGP SIGNATURE-----\nfixture-not-a-trusted-signature\n-----END PGP SIGNATURE-----\n", name, version, name, version, digestInfo(data).Digest))
}
func TestArchiveValidation(t *testing.T) {
	good := goodChart(t, "demo", "1.2.3")
	if _, err := inspectChart(bytes.NewReader(good), "demo", "1.2.3", DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	base := member{"demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: 1.2.3\n", 0}
	for _, m := range []member{{"../escape", "", 0}, {"/demo/absolute", "", 0}, {"demo/../escape", "", 0}, {"demo\\escape", "", 0}, {"other/x", "", 0}, {"demo/link", "", tar.TypeSymlink}, {"demo/hard", "", tar.TypeLink}, base} {
		t.Run(m.name, func(t *testing.T) {
			if _, err := inspectChart(bytes.NewReader(chartArchive(t, base, m)), "demo", "1.2.3", DefaultLimits()); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	for _, tc := range []struct{ name, version string }{{"other", "1.2.3"}, {"demo", "2.0.0"}} {
		if _, err := inspectChart(bytes.NewReader(good), tc.name, tc.version, DefaultLimits()); err == nil {
			t.Fatal("mismatched metadata accepted")
		}
	}
	bad := append([]byte(nil), good...)
	bad[len(bad)-5] ^= 0xff
	if _, err := inspectChart(bytes.NewReader(bad), "demo", "1.2.3", DefaultLimits()); err == nil {
		t.Fatal("gzip corruption accepted")
	}
	limits := DefaultLimits()
	limits.ExpandedBytes = 100
	if _, err := inspectChart(bytes.NewReader(good), "demo", "1.2.3", limits); err == nil {
		t.Fatal("expansion budget ignored")
	}
	limits = DefaultLimits()
	limits.MetadataBytes = 8
	if _, err := inspectChart(bytes.NewReader(good), "demo", "1.2.3", limits); err == nil {
		t.Fatal("metadata budget ignored")
	}
	limits = DefaultLimits()
	limits.Members = 1
	if _, err := inspectChart(bytes.NewReader(good), "demo", "1.2.3", limits); err == nil {
		t.Fatal("member budget ignored")
	}
}
func TestProvenanceBinding(t *testing.T) {
	data := goodChart(t, "demo", "1.2.3")
	prov := provenance(data, "demo", "1.2.3")
	digest := string(digestInfo(data).Digest)
	if err := validateProvenance(prov, "demo", "1.2.3", digest); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{bytes.Replace(prov, []byte(digest), []byte(strings.Repeat("0", 64)), 1), bytes.Replace(prov, []byte("version: 1.2.3"), []byte("version: 2.0.0"), 1), []byte("unsigned")} {
		if validateProvenance(bad, "demo", "1.2.3", digest) == nil {
			t.Fatal("invalid provenance accepted")
		}
	}
}
func TestYAMLandCoordinates(t *testing.T) {
	for _, s := range []string{"name: one\nname: two\n", "a: &a [1]\nb: *a\n", "name: demo\n---\nname: second\n"} {
		var m map[string]any
		if decodeYAML([]byte(s), &m) == nil {
			t.Fatal("ambiguous YAML accepted")
		}
	}
	for _, v := range []string{"1.2.3", "1.2.3-rc.1+build"} {
		if !validCoordinate("demo", v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"1", "v1.2.3", "01.2.3", "1.2.3/evil", "1.2.3-01"} {
		if validCoordinate("demo", v) {
			t.Fatal(v)
		}
	}
}
func TestRanges(t *testing.T) {
	for _, tc := range []struct {
		raw           string
		start, length int64
		ok            bool
	}{{"", 0, 10, true}, {"bytes=0-3", 0, 4, true}, {"bytes=-4", 6, 4, true}, {"bytes=3-", 3, 7, true}, {"bytes=8-99", 8, 2, true}, {"bytes=10-", 0, 0, false}, {"bytes=0-1,3-4", 0, 0, false}, {"bytes=-0", 0, 0, false}} {
		a, b, ok := parseRange(tc.raw, 10)
		if a != tc.start || b != tc.length || ok != tc.ok {
			t.Fatalf("%s: %d %d %v", tc.raw, a, b, ok)
		}
	}
}

func TestArchiveValidationStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := bytes.NewReader([]byte("unread archive"))
	_, err := inspectChartContext(ctx, reader, "demo", "1.2.3", DefaultLimits())
	if err == nil || reader.Len() != len("unread archive") {
		t.Fatal("cancelled validation read archive")
	}
}
