package diagnostics_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/diagnostics"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	dbPassword = "Sup3rS3cretDbPassw0rd"
	sinkToken  = "sink-bearer-" + "ZXhhbXBsZXRva2Vu" + "MTIzNDU2"
	antToken   = "ant_" + "AbCdEfGhIjKlMnOpQrStUv" + "WxYz0123456789abc"
	logSecret  = "hunter2-plaintext-secret"
)

func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		files[strings.TrimPrefix(h.Name, "ant-diagnostics/")] = string(body)
	}
}

func TestBundleRedactsSeededSecrets(t *testing.T) {
	root := []byte("0123456789abcdef0123456789abcdef")
	dbConfig, err := pgxpool.ParseConfig("postgres://ant:" + dbPassword + "@db.internal:5432/ant?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Database: dbConfig, RootKey: root, Listen: ":8080", PublicURL: "https://ant.example"}
	cfg.Storage.Backend = "s3"
	cfg.Storage.Bucket = "artifacts"

	ring := diagnostics.NewRing(10)
	logger := slog.New(diagnostics.NewHandler(slog.NewJSONHandler(io.Discard, nil), ring))
	logger.Error("connect failed", "dsn", "postgres://ant:"+dbPassword+"@db.internal/ant", "password", logSecret)
	logger.Warn("upstream rejected", "authorization", "Bearer "+sinkToken, "detail", "token="+antToken)
	logger.Error("keys", "root", hex.EncodeToString(root), "b64", base64.StdEncoding.EncodeToString(root), "raw", string(root))
	logger.Info("not captured")

	var out bytes.Buffer
	err = diagnostics.Build(context.Background(), &out, diagnostics.Input{
		Config:  cfg.Redacted(),
		Secrets: cfg.SecretValues(),
		Ring:    ring,
		Source:  "test",
		Health:  func(context.Context) any { return map[string]string{"database": "password=" + dbPassword} },
		Checks: []diagnostics.Check{{Name: "storage", Run: func(context.Context) (string, error) {
			return "", errors.New("dial postgres://ant:" + dbPassword + "@db.internal failed with secret=" + logSecret)
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, out.Bytes())
	for _, name := range []string{"build.json", "config.json", "health.json", "migrations.json", "checks.json", "errors.log", "runtime.json", "goroutines.txt"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("bundle missing %s (have %v)", name, len(files))
		}
	}
	secrets := []string{dbPassword, sinkToken, antToken, logSecret, string(root), hex.EncodeToString(root), base64.StdEncoding.EncodeToString(root)}
	for name, body := range files {
		for _, s := range secrets {
			if strings.Contains(body, s) {
				t.Errorf("%s contains secret %q", name, s)
			}
		}
	}
	if !strings.Contains(files["config.json"], "db.internal") || !strings.Contains(files["config.json"], config.RedactedMarker) {
		t.Fatalf("config.json lost non-secret values or the marker:\n%s", files["config.json"])
	}
	if strings.Contains(files["errors.log"], "not captured") || !strings.Contains(files["errors.log"], "connect failed") {
		t.Fatalf("error ring wrong:\n%s", files["errors.log"])
	}
}

func TestRingKeepsNewestInOrder(t *testing.T) {
	r := diagnostics.NewRing(3)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		_, _ = r.Write([]byte(s + "\n"))
	}
	if got := strings.Join(r.Lines(), ","); got != "c,d,e" {
		t.Fatalf("lines = %s", got)
	}
}
