package harness

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Scenario is one format's conformance run.
type Scenario struct {
	Format   string // directory/file name of the results, e.g. "pypi"
	Contract string // F-number
	Clients  []string
	Run      func(c *Ctx)
}

var registry = map[string]Scenario{}

// Register adds a scenario (called from init in each s_*.go file).
func Register(s Scenario) { registry[s.Format] = s }

// Available lists registered formats.
func Available() []string {
	var out []string
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newLoopbackListener() (*net.TCPListener, error) {
	return net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
}

// Config configures a run.
type Config struct {
	Target, Base, Token, CA, AntVersion, ResultsDir string
	Formats                                         []string
}

// Run executes the scenarios and writes <ResultsDir>/<target>/<date>/<format>.json plus SUMMARY.md.
func Run(cfg Config) error {
	formats := cfg.Formats
	if len(formats) == 0 {
		formats = Available()
		formats = defaultFormats(cfg.Target, formats)
	}
	for _, f := range formats {
		if _, ok := registry[f]; !ok {
			return fmt.Errorf("no scenario for %q; available: %s", f, strings.Join(Available(), " "))
		}
	}
	runID := randHex(3)
	env, err := NewEnv(cfg.Target, cfg.Base, cfg.Token, cfg.CA, runID)
	if err != nil {
		return err
	}
	api := Api{env}
	status := map[string]any{}
	if _, m, err := api.Req("GET", "/api/v1/status", nil, 0); err == nil {
		status = m
	}
	ant := map[string]any{"api_version": status["api_version"], "version": cfg.AntVersion, "target_base_url": cfg.Base}
	date := time.Now().UTC().Format("2006-01-02")
	outDir := filepath.Join(cfg.ResultsDir, cfg.Target, date)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := env.Start(); err != nil {
		return err
	}
	defer env.Stop()
	imgs := map[string]bool{"curl": true}
	for _, f := range formats {
		for _, cl := range registry[f].Clients {
			imgs[cl] = true
		}
	}
	for cl := range imgs {
		fmt.Println("pull", Clients[cl].Image)
		args := []string{"pull", "-q"}
		if p := Clients[cl].Platform; p != "" {
			args = append(args, "--platform", p)
		}
		_ = exec.Command("docker", append(args, Clients[cl].Image)...).Run()
	}
	for _, f := range formats {
		sc := registry[f]
		fmt.Println("==", f)
		ev := &Evidence{Format: f, Target: cfg.Target, RunID: runID, Started: time.Now().UTC().Format(time.RFC3339), Ant: ant, Clients: map[string]ClientInfo{}}
		c, err := NewCtx(env, f, ev)
		if err != nil {
			return err
		}
		t := time.Now()
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.Step("scenario-setup", func() error { return fmt.Errorf("%v", r) })
				}
			}()
			sc.Run(c)
		}()
		ev.Dur = time.Since(t).Milliseconds()
		ev.Summary = map[string]int{"pass": 0, "fail": 0, "skip": 0, "unsupported": 0}
		for _, s := range ev.Steps {
			ev.Summary[s.Status]++
		}
		switch {
		case ev.Summary["fail"] > 0:
			ev.Result = "fail"
		case ev.Summary["pass"] > 0:
			ev.Result = "pass"
		default:
			ev.Result = "skip"
		}
		c.Cleanup()
		b, _ := json.MarshalIndent(ev, "", "  ")
		text := env.Redact(string(b))
		if strings.Contains(text, cfg.Token) {
			return fmt.Errorf("token present in evidence for %s", f)
		}
		if err := os.WriteFile(filepath.Join(outDir, f+".json"), []byte(text+"\n"), 0o644); err != nil {
			return err
		}
		// The instance limits failed authentication attempts per client address to 10 per minute (HTTP 429 for every
		// request from that address while exceeded), so let the window drain before the next format's probes.
		if f != formats[len(formats)-1] {
			gap := 65 * time.Second
			if v := os.Getenv("CONF_FORMAT_GAP"); v != "" {
				if d, err := time.ParseDuration(v + "s"); err == nil {
					gap = d
				}
			}
			time.Sleep(gap)
		}
	}
	return WriteSummary(outDir)
}

// ---- fixtures

// Part is one multipart form part.
type Part struct {
	Name, Filename, ContentType string
	Data                        []byte
}

// Multipart builds a multipart/form-data body.
func Multipart(parts ...Part) ([]byte, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		d := fmt.Sprintf(`form-data; name="%s"`, p.Name)
		if p.Filename != "" {
			d += fmt.Sprintf(`; filename="%s"`, p.Filename)
		}
		h.Set("Content-Disposition", d)
		if p.ContentType != "" {
			h.Set("Content-Type", p.ContentType)
		}
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write(p.Data)
	}
	_ = w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

// ZipBytes builds a deterministic zip (insertion order given by names).
func ZipBytes(names []string, files map[string]string) []byte {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, n := range names {
		h := &zip.FileHeader{Name: n, Method: zip.Deflate}
		h.SetMode(0o644)
		h.Modified = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		w, _ := z.CreateHeader(h)
		_, _ = w.Write([]byte(files[n]))
	}
	_ = z.Close()
	return buf.Bytes()
}

// TarGz builds a deterministic tar.gz (insertion order given by names).
func TarGz(names []string, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		d := []byte(files[n])
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(d)), ModTime: time.Unix(1704067200, 0), Typeflag: tar.TypeReg})
		_, _ = tw.Write(d)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// Sha256Hex returns the hex SHA-256 of b.
func Sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Md5Hex returns the hex MD5 of b.
func Md5Hex(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) }

// ReadAll is io.ReadAll for scenarios.
var ReadAll = io.ReadAll

// ---- report

// WriteSummary renders SUMMARY.md from the <format>.json files in dir.
func WriteSummary(dir string) error {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	var rows, detail []string
	var first Evidence
	for i, f := range files {
		b, _ := os.ReadFile(f)
		var e Evidence
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		if i == 0 {
			first = e
		}
		var cl []string
		names := make([]string, 0, len(e.Clients))
		for k := range e.Clients {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			v := e.Clients[k]
			ver := v.Version
			if ver == "" {
				ver = v.Image
			}
			cl = append(cl, fmt.Sprintf("%s `%s`", k, strings.ReplaceAll(ver, "|", "/")))
		}
		rows = append(rows, fmt.Sprintf("| %s | %s | %d | %d | %d | %d | %s |", e.Format, strings.ToUpper(e.Result),
			e.Summary["pass"], e.Summary["fail"], e.Summary["unsupported"], e.Summary["skip"], strings.Join(cl, ", ")))
		var bad []string
		for _, s := range e.Steps {
			if s.Status == "fail" || s.Status == "unsupported" {
				d := strings.ReplaceAll(strings.TrimSpace(s.Detail), "\n", " ")
				if len(d) > 400 {
					d = d[:400]
				}
				bad = append(bad, fmt.Sprintf("- **%s** `%s`: %s", s.Status, s.Name, strings.ReplaceAll(d, "|", "/")))
			}
		}
		if len(bad) > 0 {
			detail = append(detail, "### "+e.Format, "")
			detail = append(detail, bad...)
			detail = append(detail, "")
		}
	}
	ver := ""
	if first.Ant != nil {
		ver = fmt.Sprint(first.Ant["version"])
	}
	out := []string{"# Conformance summary", "", fmt.Sprintf("Target: `%s`  Ant: `%s`  Date: %s", first.Target, ver, filepath.Base(dir)), "",
		"| Format | Result | Pass | Fail | Documented limitation | Skip | Clients |", "|---|---|---|---|---|---|---|"}
	out = append(out, rows...)
	out = append(out, "")
	out = append(out, detail...)
	p := filepath.Join(dir, "SUMMARY.md")
	fmt.Println("wrote", p)
	return os.WriteFile(p, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

// skipByDefault lists formats that are not run for a target unless named
// explicitly: OKE has no GPUs, so Hugging Face is not configured there.
var skipByDefault = map[string][]string{"oke": {"huggingface"}}

func defaultFormats(target string, all []string) []string {
	skip := skipByDefault[target]
	out := make([]string, 0, len(all))
	for _, f := range all {
		if !slices.Contains(skip, f) {
			out = append(out, f)
		}
	}
	return out
}
