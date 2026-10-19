package rpmrepo

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestScanPrimaryZstd(t *testing.T) {
	sum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	xmlDoc := `<metadata><package type="rpm"><name>a</name><checksum type="sha256" pkgid="YES">` + sum + `</checksum><size package="42" installed="1" archive="1"/><location href="Packages/a/a-1.rpm"/></package></metadata>`
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(xmlDoc))
	w.Close()
	items, err := scanPrimary(&buf, "repodata/abc-primary.xml.zst")
	if err != nil || items["Packages/a/a-1.rpm"].size != 42 {
		t.Fatalf("%v %+v", err, items)
	}
}
