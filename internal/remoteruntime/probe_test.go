package remoteruntime

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/upstream"
)

func TestProbePath(t *testing.T) {
	for format, want := range map[string]string{"oci": "v2/", "oci-artifacts": "v2/", "nuget": "v3/index.json", "npm": "-/ping", "maven": "", "go": ""} {
		if got := ProbePath(format); got != want {
			t.Errorf("%s: %q want %q", format, got, want)
		}
	}
}

func TestProbeOutcome(t *testing.T) {
	cases := []struct {
		name      string
		format    string
		status    int
		challenge bool
		record    bool
		auth      bool
		errored   bool
	}{
		{"oci anonymous challenge", "oci", 401, true, false, true, false},
		{"oci 401 without challenge", "oci", 401, false, true, false, true},
		{"maven 401 is an auth failure", "maven", 401, true, true, false, true},
		{"nuget 400 bad probe path", "nuget", 400, false, false, true, false},
		{"404 bad probe path", "go", 404, false, false, true, false},
		{"500 is a failure", "npm", 500, false, true, true, true},
		{"200", "oci", 200, false, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := probeOutcome(c.format, c.status, c.challenge)
			if !d.Reachable || d.record != c.record || d.Authenticated != c.auth || (d.Error != "") != c.errored {
				t.Fatalf("%+v", d)
			}
		})
	}
}

func TestHealthIgnoresNonUpstreamErrors(t *testing.T) {
	h := NewHealth()
	h.Record("s", 0, time.Millisecond, context.Canceled)
	h.Record("s", 0, time.Millisecond, upstream.ErrCircuitOpen)
	if _, ok := h.Snapshot("s"); ok {
		t.Fatal("canceled/circuit-open requests must not be recorded")
	}
	h.Record("s", 0, time.Millisecond, &upstream.FetchError{Class: "dns", Host: "example.com"})
	snap, _ := h.Snapshot("s")
	if snap.Failures != 1 || snap.LastError != "dns" {
		t.Fatalf("%+v", snap)
	}
}

func TestErrorClass(t *testing.T) {
	if got := errorClass(&upstream.FetchError{Class: "reset"}, 0); got != "reset" {
		t.Fatal(got)
	}
	if got := errorClass(errors.Join(upstream.ErrCircuitOpen), 0); got != "circuit_open" {
		t.Fatal(got)
	}
	_ = http.StatusOK
}
