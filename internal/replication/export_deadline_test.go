package replication

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A blob stream that outlives the server's WriteTimeout must still complete
// while it keeps making progress (kw replicating multi-GB layers from OKE).
func TestStreamBlobOutlivesServerWriteTimeout(t *testing.T) {
	old := blobWriteIdle
	blobWriteIdle = 2 * time.Second
	t.Cleanup(func() { blobWriteIdle = old })

	payload := bytes.Repeat([]byte("k"), 8<<20)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		streamBlob(w, bytes.NewReader(payload))
	}))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Read slowly so the whole response takes several WriteTimeouts.
	var got bytes.Buffer
	buf := make([]byte, 512<<10)
	start := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream cut after %d bytes and %s: %v", got.Len(), time.Since(start), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("test did not outlive the write timeout (%s)", elapsed)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("received %d of %d bytes", got.Len(), len(payload))
	}
}
