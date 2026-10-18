// Package diagnostics builds the redacted support bundle (a tar.gz) served by
// `ant diagnostics` and the admin diagnostics endpoint. Every byte written to
// the bundle passes through a redactor that removes configured secret values
// and common credential shapes, so the bundle is safe to attach to a ticket.
package diagnostics

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"
)

// Marker replaces redacted values.
const Marker = "[REDACTED]"

var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:/\s"]*:)[^@\s"]+@`), "${1}" + Marker + "@"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + Marker},
	{regexp.MustCompile(`\bant_[A-Za-z0-9_-]{20,}`), Marker},
	{regexp.MustCompile(`(?i)(pass(?:word|wd)?|pwd|secret|token|api[_-]?key|authorization|credentials?|private[_-]?key|root[_-]?key)(\\?["']?\s*[:=]\s*\\?["']?)[^\s"',}&\\]+`), "${1}${2}" + Marker},
}

// Redactor returns a function that scrubs the literal secrets and credential
// patterns from text. Longer secrets are replaced first.
func Redactor(secrets []string) func(string) string {
	lits := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if len(s) >= 4 {
			lits = append(lits, s)
		}
	}
	sort.Slice(lits, func(i, j int) bool { return len(lits[i]) > len(lits[j]) })
	return func(s string) string {
		for _, l := range lits {
			s = strings.ReplaceAll(s, l, Marker)
		}
		for _, p := range patterns {
			s = p.re.ReplaceAllString(s, p.repl)
		}
		return s
	}
}

// Ring keeps the most recent log lines in memory. It is an io.Writer that
// treats each Write as one line.
type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{lines: make([]string, capacity)}
}

func (r *Ring) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	r.mu.Lock()
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
	return len(p), nil
}

// Lines returns the buffered lines, oldest first.
func (r *Ring) Lines() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string(nil), r.lines[:r.next]...)
	}
	return append(append([]string(nil), r.lines[r.next:]...), r.lines[:r.next]...)
}

type handler struct {
	next    slog.Handler
	capture slog.Handler
}

// NewHandler forwards to next and also records WARN and ERROR records, as JSON
// lines, into ring.
func NewHandler(next slog.Handler, ring *Ring) slog.Handler {
	return handler{next, slog.NewJSONHandler(ring, &slog.HandlerOptions{Level: slog.LevelWarn})}
}

func (h handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l) || h.capture.Enabled(ctx, l)
}
func (h handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		_ = h.capture.Handle(ctx, r)
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}
func (h handler) WithAttrs(a []slog.Attr) slog.Handler {
	return handler{h.next.WithAttrs(a), h.capture.WithAttrs(a)}
}
func (h handler) WithGroup(n string) slog.Handler {
	return handler{h.next.WithGroup(n), h.capture.WithGroup(n)}
}

// Check is one connectivity probe. Detail must be non-sensitive; it is
// redacted regardless.
type Check struct {
	Name string
	Run  func(ctx context.Context) (detail string, err error)
}

// Input describes what the bundle contains.
type Input struct {
	// Config is the already-redacted effective configuration.
	Config map[string]any
	// Secrets are literal values scrubbed from every file.
	Secrets []string
	Ring    *Ring
	// Migration reports schema state; Health reports readiness detail. Both optional.
	Migration func(ctx context.Context) (any, error)
	Health    func(ctx context.Context) any
	Checks    []Check
	// Source is "cli" or "api".
	Source string
	Now    func() time.Time
}

// Build writes the bundle to w.
func Build(ctx context.Context, w io.Writer, in Input) error {
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	redact := Redactor(in.Secrets)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	stamp := now().UTC()
	add := func(name string, data []byte) error {
		data = []byte(redact(string(data)))
		if err := tw.WriteHeader(&tar.Header{Name: "ant-diagnostics/" + name, Mode: 0o600, Size: int64(len(data)), ModTime: stamp}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	addJSON := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			data, _ = json.Marshal(map[string]string{"error": "unserializable"})
		}
		return add(name, append(data, '\n'))
	}

	info := map[string]any{"generated_at": stamp.Format(time.RFC3339), "source": in.Source, "go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		info["module"] = bi.Main.Path
		info["version"] = bi.Main.Version
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision", "vcs.time", "vcs.modified":
				info[s.Key] = s.Value
			}
		}
	}
	if err := addJSON("build.json", info); err != nil {
		return err
	}
	if err := addJSON("config.json", in.Config); err != nil {
		return err
	}
	health := any(map[string]string{"status": "unavailable"})
	if in.Health != nil {
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		health = in.Health(hctx)
		cancel()
	}
	if err := addJSON("health.json", health); err != nil {
		return err
	}
	migration := any(map[string]string{"status": "unavailable"})
	if in.Migration != nil {
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if m, err := in.Migration(mctx); err != nil {
			migration = map[string]string{"status": "error"}
		} else {
			migration = m
		}
		cancel()
	}
	if err := addJSON("migrations.json", migration); err != nil {
		return err
	}
	results := make([]map[string]any, 0, len(in.Checks))
	for _, c := range in.Checks {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		started := time.Now()
		detail, err := c.Run(cctx)
		cancel()
		r := map[string]any{"name": c.Name, "ok": err == nil, "duration_ms": time.Since(started).Milliseconds()}
		if detail != "" {
			r["detail"] = detail
		}
		if err != nil {
			r["error"] = err.Error()
		}
		results = append(results, r)
	}
	if err := addJSON("checks.json", results); err != nil {
		return err
	}
	lines := in.Ring.Lines()
	if err := add("errors.log", []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		return err
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if err := addJSON("runtime.json", map[string]any{
		"goroutines": runtime.NumGoroutine(), "num_cpu": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "cgo_calls": runtime.NumCgoCall(),
		"heap_alloc_bytes": ms.HeapAlloc, "heap_inuse_bytes": ms.HeapInuse, "heap_objects": ms.HeapObjects, "sys_bytes": ms.Sys,
		"total_alloc_bytes": ms.TotalAlloc, "gc_cycles": ms.NumGC, "gc_pause_total_ns": ms.PauseTotalNs, "stack_inuse_bytes": ms.StackInuse,
	}); err != nil {
		return err
	}
	var dump bytes.Buffer
	if p := pprof.Lookup("goroutine"); p != nil {
		_ = p.WriteTo(&dump, 2)
	}
	if err := add("goroutines.txt", dump.Bytes()); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
