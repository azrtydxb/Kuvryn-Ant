package harness

import (
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is an injectable per-instance clock: real time plus an offset,
// so scenarios can age documents or skew one member without sleeping.
type fakeClock struct{ offset atomic.Int64 }

func (c *fakeClock) Now() time.Time          { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *fakeClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }
func (c *fakeClock) Reset()                  { c.offset.Store(0) }

// Fault modes of the TCP proxy in front of each federation listener.
const (
	faultPass      int32 = iota // forward
	faultDrop                   // accept and close immediately (connection refused-like)
	faultBlackhole              // accept and never answer (timeouts)
	faultDelay                  // forward after a delay
)

// faultProxy is the controllable TCP fault proxy in front of an instance's
// federation listener. Members reach each other only through these proxies
// (the federation URL in membership is the proxy address), so a scenario can
// take a member off the network without stopping it.
type faultProxy struct {
	ln     net.Listener
	target string
	mode   atomic.Int32
	delay  atomic.Int64
	mu     sync.Mutex
	held   []net.Conn
	wg     sync.WaitGroup
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{ln: ln, target: target}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

func (p *faultProxy) addr() string { return p.ln.Addr().String() }

// set switches the fault mode; leaving blackhole releases held connections.
func (p *faultProxy) set(mode int32) {
	p.mode.Store(mode)
	if mode != faultBlackhole {
		p.mu.Lock()
		for _, c := range p.held {
			_ = c.Close()
		}
		p.held = nil
		p.mu.Unlock()
	}
}

func (p *faultProxy) setDelay(d time.Duration) {
	p.delay.Store(int64(d))
	p.set(faultDelay)
}

func (p *faultProxy) close() {
	_ = p.ln.Close()
	p.set(faultDrop)
	p.wg.Wait()
}

func (p *faultProxy) accept() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		switch p.mode.Load() {
		case faultDrop:
			_ = c.Close()
			continue
		case faultBlackhole:
			p.mu.Lock()
			p.held = append(p.held, c)
			p.mu.Unlock()
			continue
		}
		p.wg.Add(1)
		go p.forward(c)
	}
}

func (p *faultProxy) forward(c net.Conn) {
	defer p.wg.Done()
	defer c.Close()
	if p.mode.Load() == faultDelay {
		time.Sleep(time.Duration(p.delay.Load()))
	}
	up, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

type rawResponse struct {
	status int
	body   []byte
}

func newJSONRequest(method, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, err
}

func httpDo(req *http.Request) (rawResponse, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return rawResponse{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return rawResponse{status: resp.StatusCode, body: b}, err
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mustB64(s string) []byte {
	b, _ := base64.RawURLEncoding.DecodeString(s)
	return b
}
