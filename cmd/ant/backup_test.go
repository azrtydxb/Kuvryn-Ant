package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/backup"
	"github.com/azrtydxb/Kuvryn-Ant/internal/backup/backuptest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5"
)

// cliDatabase creates a database on the fixture server and returns a DSN for it.
func cliDatabase(t *testing.T, admin *pgx.Conn) string {
	t.Helper()
	name := fmt.Sprintf("ant_cli_backup_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	c := admin.Config()
	return fmt.Sprintf("host=%s user=%s dbname=%s sslmode=disable", c.Host, c.User, name)
}

func TestBackupRestoreVerifyCommands(t *testing.T) {
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	keyPath := filepath.Join(t.TempDir(), "root.key")
	if err = os.WriteFile(keyPath, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANT_ROOT_KEY_FILE", keyPath)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	use := func(database string) {
		t.Setenv("ANT_DATABASE_URL", database)
		t.Setenv("ANT_DATA_DIR", t.TempDir())
	}
	source, target := cliDatabase(t, admin), cliDatabase(t, admin)
	use(source)
	var out bytes.Buffer
	if code := execute(ctx, quiet, []string{"bootstrap", "cli-backup"}, &out); code != 0 {
		t.Fatalf("bootstrap exit %d", code)
	}
	dir := filepath.Join(t.TempDir(), "bk")
	out.Reset()
	if code := execute(ctx, quiet, []string{"backup", "--max-disk-percent", "0", dir}, &out); code != 0 {
		t.Fatalf("backup exit %d", code)
	}
	var created struct {
		Created        bool   `json:"created"`
		ManifestSHA256 string `json:"manifest_sha256"`
		Tables         int    `json:"tables"`
	}
	if err = json.Unmarshal(out.Bytes(), &created); err != nil || !created.Created || created.Tables == 0 || len(created.ManifestSHA256) != 64 {
		t.Fatalf("summary %s", out.String())
	}
	out.Reset()
	if code := execute(ctx, quiet, []string{"backup", "verify", "--expect-manifest-sha256", created.ManifestSHA256, dir}, &out); code != 0 {
		t.Fatalf("verify exit %d", code)
	}
	if code := execute(ctx, quiet, []string{"backup", "verify", "--expect-manifest-sha256", "00" + created.ManifestSHA256[2:], dir}, io.Discard); code != 1 {
		t.Fatal("verify ignored manifest pin")
	}
	if code := execute(ctx, quiet, []string{"backup", dir}, io.Discard); code != 1 {
		t.Fatal("backup reused a non-empty directory")
	}
	if code := execute(ctx, quiet, []string{"restore", "--concurrency", "0", dir}, io.Discard); code != 1 {
		t.Fatal("accepted bad flags")
	}
	use(target)
	out.Reset()
	if code := execute(ctx, quiet, []string{"restore", dir}, &out); code != 0 {
		t.Fatalf("restore exit %d", code)
	}
	var restored struct {
		Restored bool `json:"restored"`
		Rows     int  `json:"rows"`
	}
	if err = json.Unmarshal(out.Bytes(), &restored); err != nil || !restored.Restored || restored.Rows == 0 {
		t.Fatalf("summary %s", out.String())
	}
	if code := execute(ctx, quiet, []string{"restore", dir}, io.Discard); code != 1 {
		t.Fatal("restore accepted a populated database")
	}
	// The restored deployment still recognizes its recovery identity.
	if code := execute(ctx, quiet, []string{"bootstrap-token"}, io.Discard); code != 0 {
		t.Fatal("restored database has no usable bootstrap identity")
	}
}

func TestBackupToObjectStorageWithRetention(t *testing.T) {
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	keyPath := filepath.Join(t.TempDir(), "root.key")
	if err = os.WriteFile(keyPath, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANT_ROOT_KEY_FILE", keyPath)
	fake := backuptest.NewMemObjects()
	old := objectClient
	objectClient = func(*storage.Registry) (backup.ObjectAPI, bool) { return fake, true }
	t.Cleanup(func() { objectClient = old })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	source, target := cliDatabase(t, admin), cliDatabase(t, admin)
	dataDir := t.TempDir()
	use := func(database string) {
		t.Setenv("ANT_DATABASE_URL", database)
		t.Setenv("ANT_DATA_DIR", dataDir)
	}
	use(source)
	if code := execute(ctx, quiet, []string{"bootstrap", "cli-backup"}, io.Discard); code != 0 {
		t.Fatalf("bootstrap exit %d", code)
	}
	var last string
	for i := range 3 {
		last = fmt.Sprintf("s3://kuvryn-ant/backups/pre-%d", i)
		var out bytes.Buffer
		if code := execute(ctx, quiet, []string{"backup", "--keep", "2", last}, &out); code != 0 {
			t.Fatalf("backup %d exit %d", i, code)
		}
		var created struct {
			Created bool     `json:"created"`
			Removed []string `json:"removed_old_backups"`
			Mode    string   `json:"blob_mode"`
		}
		if err = json.Unmarshal(out.Bytes(), &created); err != nil || !created.Created || created.Mode != "copy" {
			t.Fatalf("summary %s", out.String())
		}
		if removedOld := len(created.Removed) == 1 && created.Removed[0] == "pre-0"; removedOld != (i == 2) {
			t.Fatalf("backup %d removed %v", i, created.Removed)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for k := range fake.Objects {
		if strings.Contains(k, "/backups/pre-0/") {
			t.Fatalf("pre-0 not removed: %s", k)
		}
	}
	if code := execute(ctx, quiet, []string{"backup", "verify", last}, io.Discard); code != 0 {
		t.Fatal("verify of object-storage backup failed")
	}
	if code := execute(ctx, quiet, []string{"backup", "--keep", "-1", "s3://kuvryn-ant/backups/x"}, io.Discard); code != 1 {
		t.Fatal("accepted negative --keep")
	}
	if code := execute(ctx, quiet, []string{"backup", "--blobs", "bogus", "s3://kuvryn-ant/backups/x"}, io.Discard); code != 1 {
		t.Fatal("accepted unknown blob mode")
	}
	use(target)
	var out bytes.Buffer
	if code := execute(ctx, quiet, []string{"restore", last}, &out); code != 0 {
		t.Fatalf("restore from object storage exit %d", code)
	}
}

func TestResolveBlobMode(t *testing.T) {
	for _, c := range []struct{ flag, backend, want string }{
		{"auto", "s3", "reference"}, {"", "disk", "copy"}, {"auto", "disk", "copy"},
		{"copy", "s3", "copy"}, {"reference", "disk", "reference"},
	} {
		if got, err := resolveBlobMode(c.flag, c.backend); err != nil || got != c.want {
			t.Errorf("%+v -> %q %v", c, got, err)
		}
	}
	if _, err := resolveBlobMode("x", "s3"); err == nil {
		t.Fatal("bogus mode accepted")
	}
}
