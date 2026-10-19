package diskusage

import (
	"bytes"
	"strings"
	"testing"
)

func TestCollectorReportsFreeAndTotal(t *testing.T) {
	var b bytes.Buffer
	Collector(t.TempDir())(&b)
	out := b.String()
	for _, want := range []string{"ant_data_dir_bytes_free ", "ant_data_dir_bytes_total "} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	var none bytes.Buffer
	Collector("/nonexistent/ant/dir")(&none)
	if none.Len() != 0 {
		t.Fatal("unreadable directory must emit nothing")
	}
}

func TestPercent(t *testing.T) {
	if p := (Usage{Total: 100, Free: 20}).Percent(); p != 80 {
		t.Fatalf("got %d", p)
	}
	if p := (Usage{}).Percent(); p != 0 {
		t.Fatalf("got %d", p)
	}
}
