package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// Uploads and downloads on data-plane routes outlive the server's fixed
// Read/WriteTimeout as long as they keep making progress.
func TestDataRoutesOutliveFixedTimeouts(t *testing.T) {
	old := streamIdle
	streamIdle = 2 * time.Second
	t.Cleanup(func() { streamIdle = old })
	s := New(pinger{}, 8, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), func(mux *http.ServeMux) {
		mux.HandleFunc("/feeds/x/raw/slow", func(w http.ResponseWriter, r *http.Request) {
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			for range 6 {
				_, _ = w.Write([]byte("chunk"))
				w.(http.Flusher).Flush()
				time.Sleep(100 * time.Millisecond)
			}
			_, _ = w.Write([]byte{byte(n % 251)})
		})
	})
	s.http.ReadTimeout, s.http.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.http.Serve(ln)
	defer s.http.Close()
	pr, pw := io.Pipe()
	go func() {
		for range 5 {
			_, _ = pw.Write(bytes.Repeat([]byte("a"), 10))
			time.Sleep(150 * time.Millisecond)
		}
		pw.Close()
	}()
	resp, err := http.Post("http://"+ln.Addr().String()+"/feeds/x/raw/slow", "application/octet-stream", pr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || len(b) != 31 {
		t.Fatalf("transfer cut: status=%d len=%d err=%v", resp.StatusCode, len(b), err)
	}
}

type pinger struct{}

func (pinger) Ping(context.Context) error { return nil }
