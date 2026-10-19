// Command antbench is a small HTTP load generator for the Kuvryn Ant release
// workloads (docs/planning/release-workloads.md, spec section 8). It is
// deliberately dependency-free (standard library only) and prints one JSON
// document per run so results can be committed as evidence.
//
// Subcommands:
//
//	get      concurrent GETs of one URL or a URL list (warm/cold pulls, fan-out)
//	put      concurrent PUTs of deterministic bodies (publish throughput, duplicate writers)
//	stream   stream one large object to a discarding sink, verifying SHA-256
//
// The bearer token is read from ANT_TOKEN (or -token-file holding the JSON
// credential written by the automation tooling) and is never printed.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type common struct {
	token       string
	tokenFile   string
	insecure    bool
	concurrency int
	timeout     time.Duration
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.tokenFile, "token-file", "", "JSON credential file with a \"secret\" field (default: $ANT_TOKEN)")
	fs.BoolVar(&c.insecure, "insecure", false, "skip TLS verification (private-CA ingress)")
	fs.IntVar(&c.concurrency, "c", 4, "concurrent workers")
	fs.DurationVar(&c.timeout, "timeout", 10*time.Minute, "per-request timeout")
}

func (c *common) resolve() error {
	c.token = os.Getenv("ANT_TOKEN")
	if c.tokenFile != "" {
		b, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return err
		}
		var j struct {
			Secret string `json:"secret"`
			Token  string `json:"token"`
		}
		if err := json.Unmarshal(b, &j); err != nil {
			return err
		}
		c.token = j.Secret
		if c.token == "" {
			c.token = j.Token
		}
	}
	return nil
}

func (c *common) client() *http.Client {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: c.insecure}, //nolint:gosec // opt-in for private CA
		MaxIdleConnsPerHost: c.concurrency * 2,
		MaxConnsPerHost:     c.concurrency * 2,
		DisableCompression:  true,
	}
	return &http.Client{Transport: tr, Timeout: c.timeout}
}

type sample struct {
	d      time.Duration
	ttfb   time.Duration
	status int
	bytes  int64
	err    string
	sum    string
}

type stats struct {
	Requests     int            `json:"requests"`
	Errors       int            `json:"errors"`
	ErrorSamples []string       `json:"error_samples,omitempty"`
	Status       map[string]int `json:"status"`
	Bytes        int64          `json:"bytes"`
	WallSeconds  float64        `json:"wall_seconds"`
	ReqPerSec    float64        `json:"req_per_sec"`
	MiBPerSec    float64        `json:"mib_per_sec"`
	LatencyMs    pct            `json:"latency_ms"`
	TTFBMs       pct            `json:"ttfb_ms"`
	Digests      map[string]int `json:"body_sha256_counts,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
}

type pct struct {
	P50, P95, P99, Max, Mean float64
}

func percentiles(d []time.Duration) pct {
	if len(d) == 0 {
		return pct{}
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) float64 {
		i := int(q*float64(len(s)-1) + 0.5)
		return float64(s[i].Microseconds()) / 1000
	}
	var sum time.Duration
	for _, v := range s {
		sum += v
	}
	return pct{at(.5), at(.95), at(.99), float64(s[len(s)-1].Microseconds()) / 1000, float64(sum.Microseconds()) / 1000 / float64(len(s))}
}

func summarize(ss []sample, wall time.Duration) stats {
	st := stats{Status: map[string]int{}, Digests: map[string]int{}}
	var lat, ttfb []time.Duration
	for _, s := range ss {
		st.Requests++
		st.Bytes += s.bytes
		if s.err != "" {
			st.Errors++
			if len(st.ErrorSamples) < 5 {
				st.ErrorSamples = append(st.ErrorSamples, s.err)
			}
			st.Status["error"]++
			continue
		}
		st.Status[fmt.Sprint(s.status)]++
		if s.status >= 500 {
			st.Errors++
		}
		lat = append(lat, s.d)
		ttfb = append(ttfb, s.ttfb)
		if s.sum != "" {
			st.Digests[s.sum]++
		}
	}
	st.WallSeconds = wall.Seconds()
	if wall > 0 {
		st.ReqPerSec = float64(st.Requests) / wall.Seconds()
		st.MiBPerSec = float64(st.Bytes) / (1 << 20) / wall.Seconds()
	}
	st.LatencyMs, st.TTFBMs = percentiles(lat), percentiles(ttfb)
	if len(st.Digests) == 0 {
		st.Digests = nil
	}
	return st
}

func emit(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func do(cl *http.Client, ctx context.Context, method, url, token string, body []byte, hdr map[string]string, hash bool) sample {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return sample{err: err.Error()}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		return sample{err: redact(err.Error(), token), d: time.Since(start)}
	}
	defer func() { _ = resp.Body.Close() }()
	ttfb := time.Since(start)
	h := sha256.New()
	w := io.Discard
	if hash {
		w = h
	}
	n, err := io.Copy(w, resp.Body)
	s := sample{d: time.Since(start), ttfb: ttfb, status: resp.StatusCode, bytes: n}
	if err != nil {
		s.err = "body: " + err.Error()
	} else if hash && resp.StatusCode/100 == 2 {
		s.sum = hex.EncodeToString(h.Sum(nil))
	}
	return s
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func cmdGet(args []string) error {
	var c common
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	c.register(fs)
	url := fs.String("url", "", "URL to fetch (or use -urls-file)")
	urlsFile := fs.String("urls-file", "", "file with one URL per line")
	n := fs.Int("n", 0, "total requests (0 = use -d)")
	d := fs.Duration("d", 0, "run duration")
	rate := fs.Float64("rate", 0, "target aggregate requests/second (0 = closed loop)")
	rng := fs.String("range", "", "Range header value")
	popular := fs.Int("popular", 0, "with -urls-file: the first N URLs are the popular set")
	popFrac := fs.Float64("popular-frac", 0.9, "fraction of requests drawn from the popular set")
	hash := fs.Bool("hash", false, "SHA-256 every response body and report the digest histogram")
	seed := fs.Uint64("seed", 1, "PRNG seed for URL selection")
	sequential := fs.Bool("sequential", false, "walk the URL list in order (index = request number mod list length) instead of sampling")
	_ = fs.Parse(args)
	if err := c.resolve(); err != nil {
		return err
	}
	urls := []string{*url}
	if *urlsFile != "" {
		var err error
		if urls, err = readLines(*urlsFile); err != nil {
			return err
		}
	}
	if len(urls) == 0 || urls[0] == "" || (*n == 0 && *d == 0) {
		return fmt.Errorf("need -url/-urls-file and -n or -d")
	}
	cl := c.client()
	var mu sync.Mutex
	var all []sample
	var issued atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if *d > 0 {
		time.AfterFunc(*d, cancel)
	}
	var tick <-chan time.Time
	if *rate > 0 {
		t := time.NewTicker(time.Duration(float64(time.Second) / *rate))
		defer t.Stop()
		tick = t.C
	}
	hdr := map[string]string{}
	if *rng != "" {
		hdr["Range"] = *rng
	}
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < c.concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(*seed, uint64(w)))
			for {
				if tick != nil {
					select {
					case <-tick:
					case <-ctx.Done():
						return
					}
				}
				i := issued.Add(1)
				if (*n > 0 && int(i) > *n) || ctx.Err() != nil {
					return
				}
				u := urls[0]
				if *sequential {
					u = urls[int(i-1)%len(urls)]
				} else if len(urls) > 1 {
					if *popular > 0 && *popular < len(urls) {
						if r.Float64() < *popFrac {
							u = urls[r.IntN(*popular)]
						} else {
							u = urls[*popular+r.IntN(len(urls)-*popular)]
						}
					} else {
						u = urls[r.IntN(len(urls))]
					}
				}
				s := do(cl, context.Background(), "GET", u, c.token, nil, hdr, *hash)
				mu.Lock()
				all = append(all, s)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	emit(map[string]any{"cmd": "get", "concurrency": c.concurrency, "urls": len(urls), "range": *rng, "started": start.UTC().Format(time.RFC3339), "result": summarize(all, time.Since(start))})
	return nil
}

// det returns a deterministic body for (seed,size) so any run can be re-verified.
func det(seed string, size int) []byte {
	out := make([]byte, 0, size+32)
	for i := 0; len(out) < size; i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", seed, i)))
		out = append(out, h[:]...)
	}
	return out[:size]
}

// streamPutMin is the body size from which put generates the body on the fly (bounded client memory).
const streamPutMin = 32 << 20

// detReader yields exactly the bytes det(seed, size) returns, without materializing them.
type detReader struct {
	seed string
	left int64
	i    int
	buf  []byte
}

func (r *detReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	if len(r.buf) == 0 {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", r.seed, r.i)))
		r.i++
		r.buf = h[:]
	}
	n := copy(p, r.buf)
	if int64(n) > r.left {
		n = int(r.left)
	}
	r.buf = r.buf[n:]
	r.left -= int64(n)
	return n, nil
}

// putStream uploads a generated body of known length and returns its digest.
func putStream(cl *http.Client, url, token, seed string, size int64, declare bool) (sample, [32]byte) {
	var sum [32]byte
	h := sha256.New()
	_, _ = io.Copy(h, &detReader{seed: seed, left: size})
	copy(sum[:], h.Sum(nil))
	req, err := http.NewRequest("PUT", url, &detReader{seed: seed, left: size})
	if err != nil {
		return sample{err: err.Error()}, sum
	}
	req.ContentLength = size
	if declare {
		req.Header.Set("X-Checksum-Sha256", hex.EncodeToString(sum[:]))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		return sample{err: redact(err.Error(), token), d: time.Since(start)}, sum
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return sample{d: time.Since(start), ttfb: time.Since(start), status: resp.StatusCode, bytes: size}, sum
}

func cmdPut(args []string) error {
	var c common
	fs := flag.NewFlagSet("put", flag.ExitOnError)
	c.register(fs)
	tmpl := fs.String("url-template", "", "URL with {i} replaced by the request index")
	n := fs.Int("n", 100, "number of PUTs")
	size := fs.Int("size", 4096, "body size in bytes")
	dup := fs.Bool("dup", false, "duplicate-coordinate writers: every request targets index 0 with a different body")
	seed := fs.String("seed", "antbench", "body seed")
	declare := fs.Bool("declare-digest", true, "send X-Checksum-Sha256 for streamed bodies (raw feeds then stream straight to storage instead of spooling to /tmp)")
	_ = fs.Parse(args)
	if err := c.resolve(); err != nil {
		return err
	}
	if !strings.Contains(*tmpl, "{i}") {
		return fmt.Errorf("-url-template must contain {i}")
	}
	cl := c.client()
	jobs := make(chan int)
	var mu sync.Mutex
	var all []sample
	bodies := map[string]int{}
	var accepted []string
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < c.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				idx := i
				if *dup {
					idx = 0
				}
				u := strings.ReplaceAll(*tmpl, "{i}", fmt.Sprint(idx))
				var s sample
				var sum [32]byte
				if *size >= streamPutMin {
					s, sum = putStream(cl, u, c.token, fmt.Sprintf("%s:%d", *seed, i), int64(*size), *declare)
				} else {
					body := det(fmt.Sprintf("%s:%d", *seed, i), *size)
					s = do(cl, context.Background(), "PUT", u, c.token, body, map[string]string{"Content-Type": "application/octet-stream"}, false)
					sum = sha256.Sum256(body)
				}
				mu.Lock()
				all = append(all, s)
				if s.status/100 == 2 {
					bodies[hex.EncodeToString(sum[:])]++
					if *dup && s.status == 201 {
						accepted = append(accepted, hex.EncodeToString(sum[:]))
					}
				}
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < *n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	st := summarize(all, time.Since(start))
	st.Extra = map[string]any{"accepted_body_sha256": bodyList(bodies), "distinct_accepted_bodies": len(bodies), "created_201_body_sha256": accepted, "size": *size, "dup": *dup}
	emit(map[string]any{"cmd": "put", "concurrency": c.concurrency, "started": start.UTC().Format(time.RFC3339), "result": st})
	return nil
}

func cmdStream(args []string) error {
	var c common
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	c.register(fs)
	url := fs.String("url", "", "object URL")
	expect := fs.String("expect-sha256", "", "expected hex SHA-256 of the whole body")
	repeat := fs.Int("repeat", 1, "downloads per worker")
	cancelAfter := fs.Int64("cancel-after", 0, "abort after N bytes (client cancellation test)")
	_ = fs.Parse(args)
	if err := c.resolve(); err != nil {
		return err
	}
	cl := c.client()
	var mu sync.Mutex
	var all []sample
	mismatch := 0
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < c.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < *repeat; r++ {
				ctx, cancel := context.WithCancel(context.Background())
				req, _ := http.NewRequestWithContext(ctx, "GET", *url, nil)
				req.Header.Set("Authorization", "Bearer "+c.token)
				t0 := time.Now()
				resp, err := cl.Do(req)
				if err != nil {
					mu.Lock()
					all = append(all, sample{err: redact(err.Error(), c.token)})
					mu.Unlock()
					cancel()
					continue
				}
				ttfb := time.Since(t0)
				h := sha256.New()
				var n int64
				buf := make([]byte, 256<<10)
				var rerr error
				for {
					m, e := resp.Body.Read(buf)
					h.Write(buf[:m])
					n += int64(m)
					if *cancelAfter > 0 && n >= *cancelAfter {
						cancel()
						rerr = fmt.Errorf("cancelled after %d bytes (intentional)", n)
						break
					}
					if e != nil {
						if e != io.EOF {
							rerr = e
						}
						break
					}
				}
				_ = resp.Body.Close()
				cancel()
				s := sample{d: time.Since(t0), ttfb: ttfb, status: resp.StatusCode, bytes: n, sum: hex.EncodeToString(h.Sum(nil))}
				if rerr != nil {
					s.err = rerr.Error()
				}
				mu.Lock()
				if *expect != "" && *cancelAfter == 0 && s.sum != *expect {
					mismatch++
				}
				all = append(all, s)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	st := summarize(all, time.Since(start))
	st.Extra = map[string]any{"digest_mismatches": mismatch, "expected_sha256": *expect, "cancel_after": *cancelAfter}
	emit(map[string]any{"cmd": "stream", "concurrency": c.concurrency, "started": start.UTC().Format(time.RFC3339), "result": st})
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: antbench get|put|stream [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "get":
		err = cmdGet(os.Args[2:])
	case "put":
		err = cmdPut(os.Args[2:])
	case "stream":
		err = cmdStream(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "antbench:", err)
		os.Exit(1)
	}
}

var amzParam = regexp.MustCompile(`X-Amz-[A-Za-z0-9-]+=[^&"]*`)

// redact removes the bearer token (when there is one) and any pre-signed URL parameters from an error string.
func redact(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "***")
	}
	return amzParam.ReplaceAllString(s, "X-Amz-REDACTED")
}

func bodyList(m map[string]int) []string {
	var out []string
	for k := range m {
		if len(out) < 4 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
