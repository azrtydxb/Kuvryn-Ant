package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

const domainA = "00000000-0000-4000-8000-000000000001"
const domainB = "00000000-0000-4000-8000-000000000002"

func expected(data []byte) blob.Info {
	sum := sha256.Sum256(data)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(data))}
}
func put(t *testing.T, s blob.Store, data []byte) {
	t.Helper()
	u, err := s.Stage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer u.Abort()
	if _, err = u.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err = u.Finalize(context.Background(), expected(data)); err != nil {
		t.Fatal(err)
	}
}
func TestRegistryDomainIsolationAndClose(t *testing.T) {
	root := t.TempDir()
	r, err := New(Config{Directory: root})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	a, err := r.Resolve(ctx, domainA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Resolve(ctx, domainB)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Resolve(ctx, domainA)
	if err != nil || again != a {
		t.Fatal("domain store not cached")
	}
	data := []byte("isolated content")
	put(t, a, data)
	if _, err = b.Stat(ctx, expected(data).Digest); !errors.Is(err, blob.ErrNotFound) {
		t.Fatal("blob crossed domain")
	}
	put(t, b, data)
	for _, domain := range []string{domainA, domainB} {
		if _, err = os.Stat(filepath.Join(root, "blobs", domain, "objects", string(expected(data).Digest))); err != nil {
			t.Fatal("domain not physically isolated", err)
		}
	}
	for _, domain := range []string{"../escape", "", "not-uuid", strings.ToUpper("abcdefab-0000-4000-8000-000000000001")} {
		if _, err = r.Resolve(ctx, domain); !errors.Is(err, ErrConfiguration) {
			t.Fatal("invalid domain accepted")
		}
	}
	r.Close()
	r.Close()
	if _, err = r.Resolve(ctx, domainA); !errors.Is(err, ErrClosed) {
		t.Fatal("closed registry reused")
	}
	if _, err = a.Stat(ctx, expected(data).Digest); err == nil {
		t.Fatal("disk root remained open")
	}
}
func TestRegistryGlobalAdmissionAcrossDomains(t *testing.T) {
	r, err := New(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	a, err := r.Resolve(ctx, domainA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Resolve(ctx, domainB)
	if err != nil {
		t.Fatal(err)
	}
	var uploads []blob.Upload
	for i := range 16 {
		s := a
		if i%2 == 1 {
			s = b
		}
		u, err := s.Stage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		uploads = append(uploads, u)
		defer u.Abort()
	}
	blocked, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err = a.Stage(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("global admission bypassed", err)
	}
	if _, err = uploads[0].Finalize(ctx, blob.Info{Digest: "invalid"}); !errors.Is(err, blob.ErrInvalidDigest) {
		t.Fatal(err)
	}
	extra, err := b.Stage(ctx)
	if err != nil {
		t.Fatal("failed finalize retained slot", err)
	}
	defer extra.Abort()
	for _, u := range uploads {
		if err = u.Abort(); err != nil {
			t.Fatal(err)
		}
	}
	if err = extra.Abort(); err != nil {
		t.Fatal(err)
	}
	if len(r.slots) != 0 {
		t.Fatal("admission leaked or double released")
	}
}
func TestRegistryCredentialReloadAndS3DomainPrefix(t *testing.T) {
	var mu sync.Mutex
	var authorizations, paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>key</Key><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	root := t.TempDir()
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "credentials.json")
	write := func(access string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(fmt.Sprintf(`{"access_key_id":%q,"secret_access_key":"private-secret-key"}`, access)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("first-access")
	r, err := New(Config{Backend: "s3", Directory: root, Endpoint: server.URL, Region: "test", Bucket: "bucket", Prefix: "tenant", CredentialsFile: file, CAFile: ca, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	store, err := r.Resolve(context.Background(), domainA)
	if err != nil {
		t.Fatal(err)
	}
	// An empty object is published with one PutObject (S3 uploads start lazily),
	// which exercises the request signing and the domain prefix.
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	publish := func() {
		t.Helper()
		u, err := store.Stage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer u.Abort()
		if _, err = u.Finalize(context.Background(), blob.Info{Digest: empty}); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	write("second-access")
	publish()
	mu.Lock()
	defer mu.Unlock()
	first, second := false, false
	for _, value := range authorizations {
		first = first || strings.Contains(value, "Credential=first-access/")
		second = second || strings.Contains(value, "Credential=second-access/")
		if strings.Contains(value, "private-secret-key") {
			t.Fatal("plaintext key sent")
		}
	}
	if !first || !second {
		t.Fatal("credentials did not reload")
	}
	for _, path := range paths {
		if !strings.Contains(path, "/tenant/"+domainA+"/objects/") {
			t.Fatal("S3 domain namespace missing")
		}
	}
}
func TestRegistryRejectsBadConfigurationAndRedacts(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "credential-secret-filename")
	valid := []byte(`{"access_key_id":"access","secret_access_key":"private-value"}`)
	if err := os.WriteFile(file, valid, 0600); err != nil {
		t.Fatal(err)
	}
	base := Config{Backend: "s3", Directory: root, Region: "test", Bucket: "bucket", CredentialsFile: file}
	for _, change := range []func(*Config){func(c *Config) { c.Endpoint = "https://user:private-value@example.test" }, func(c *Config) { c.Endpoint = "http://example.test" }, func(c *Config) { c.Prefix = "../escape" }, func(c *Config) { c.Prefix = strings.Repeat("x", 769) }, func(c *Config) { c.Region = strings.Repeat("x", 129) }} {
		c := base
		change(&c)
		if _, err := New(c); !errors.Is(err, ErrConfiguration) || strings.Contains(err.Error(), "private") {
			t.Fatal("bad config accepted/leaked")
		}
	}
	for _, content := range [][]byte{[]byte(`{"access_key_id":"access","secret_access_key":"private-value","extra":true}`), append(bytes.Clone(valid), []byte(` {}`)...), bytes.Repeat([]byte("x"), 16385)} {
		if err := os.WriteFile(file, content, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(base); !errors.Is(err, ErrConfiguration) || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid credentials accepted/leaked")
		}
	}
	if err := os.WriteFile(file, valid, 0600); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(root, "huge-ca")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	base.CAFile = huge
	if _, err = New(base); !errors.Is(err, ErrConfiguration) {
		t.Fatal("oversized CA accepted")
	}
	r, err := New(Config{Directory: root})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if fmt.Sprintf("%+v", r) != "storage.Registry{redacted}" || fmt.Sprintf("%#v", r) != "storage.Registry{redacted}" {
		t.Fatal("registry formatting not redacted")
	}
}
