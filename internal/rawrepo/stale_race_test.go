package rawrepo

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

// A cached object must be served even if the upstream goes away the instant the
// first response completes: clients such as the go tool stop reading (and
// disconnect) as soon as they have Content-Length bytes, so the cache entry
// must be committed without depending on a later end-of-body read.
func TestCachedObjectSurvivesImmediateOutage(t *testing.T) {
	f := remotetest.New(t)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB: several copy buffers
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload[:len(payload)-1])
		w.(http.Flusher).Flush()
		_, _ = w.Write(payload[len(payload)-1:])
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond) // end of stream arrives after the data
	}))
	// HTTP/2 delivers the end of the body separately from the last data, so the
	// client can have every byte before the proxy has seen EOF.
	up.EnableHTTP2 = true
	up.StartTLS()
	t.Cleanup(up.Close)
	feed := f.AddFeed("rawfeed", "raw")
	f.AddLocal(feed, "raw", "local", 0)
	f.AddRemote(feed, "raw", "up", 1, up.URL, remotetest.RemoteOptions{Credential: "Bearer x"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/rawfeed/raw/"
	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", fmt.Sprintf("%sobj-%d.bin", base, i), nil)
			req.Header.Set("Authorization", "Bearer "+f.Token)
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			// Read exactly Content-Length bytes and hang up, like the go tool.
			if _, err := io.ReadFull(resp.Body, make([]byte, len(payload))); err != nil {
				t.Error(err)
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	up.Close()
	for i := range n {
		status, body, _ := f.Get(fmt.Sprintf("%sobj-%d.bin", base, i))
		if status != 200 || len(body) != len(payload) {
			t.Errorf("obj-%d after outage: %d (%d bytes)", i, status, len(body))
		}
	}
}
