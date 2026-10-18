package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBootstrapCommandAndRecovery(t *testing.T) {
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("integration DSN required")
		}
		t.Skip("PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("ant_cli_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid fixture DSN")
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema
	}
	key := filepath.Join(t.TempDir(), "key")
	if err = os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANT_DATABASE_URL", dsn)
	t.Setenv("ANT_ROOT_KEY_FILE", key)
	var output, logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	if code := execute(ctx, logger, []string{"bootstrap", "test"}, &output); code != 0 {
		t.Fatalf("bootstrap failed: %s", logs.String())
	}
	var result struct {
		Token   string `json:"token"`
		TokenID string `json:"token_id"`
	}
	if err = json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Token == "" || result.TokenID == "" {
		t.Fatal("bootstrap did not deliver credential")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	principal, err := (identity.Tokens{DB: pool}).Authenticate(ctx, result.Token)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := (identity.Roles{DB: pool}).Resolve(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Authorize(principal, identity.Resource{InstanceID: principal.InstanceID, Kind: identity.FeedResource}, identity.Configure) {
		t.Fatal("bootstrap cannot configure feeds")
	}
	output.Reset()
	if code := execute(ctx, logger, []string{"bootstrap", "duplicate"}, &output); code != 1 || output.Len() != 0 {
		t.Fatal("second bootstrap should refuse")
	}
	if code := execute(ctx, logger, []string{"bootstrap-token"}, &output); code != 0 {
		t.Fatalf("recovery failed: %s", logs.String())
	}
	var recovered struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(output.Bytes(), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Token == result.Token {
		t.Fatal("recovery reused secret")
	}
	if _, err := (identity.Tokens{DB: pool}).Authenticate(ctx, recovered.Token); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), result.Token) || strings.Contains(logs.String(), recovered.Token) {
		t.Fatal("credential leaked into logs")
	}
}
