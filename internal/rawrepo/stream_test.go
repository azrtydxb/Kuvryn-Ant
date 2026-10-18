package rawrepo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type deadlineWriter struct {
	*httptest.ResponseRecorder
	reads, writes []time.Time
}

func (w *deadlineWriter) SetReadDeadline(d time.Time) error { w.reads = append(w.reads, d); return nil }
func (w *deadlineWriter) SetWriteDeadline(d time.Time) error {
	w.writes = append(w.writes, d)
	return nil
}
func TestStreamingIdleDeadlinesAndCancellation(t *testing.T) {
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	controller := http.NewResponseController(w)
	body := idleBody{ReadCloser: io.NopCloser(strings.NewReader("data")), ctx: context.Background(), controller: controller}
	buf := make([]byte, 2)
	for range 2 {
		if _, err := body.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	writer := &idleWriter{ResponseWriter: w, controller: controller}
	writer.WriteHeader(200)
	if _, err := writer.ReadFrom(strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if len(w.reads) != 2 || len(w.writes) < 2 {
		t.Fatal("deadlines not refreshed")
	}
	for _, d := range append(w.reads, w.writes...) {
		if remaining := time.Until(d); remaining < 29*time.Second || remaining > 30*time.Second {
			t.Fatal("not an idle deadline", remaining)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body.ctx = ctx
	if _, err := body.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
