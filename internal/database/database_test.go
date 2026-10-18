package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("set ANT_TEST_DATABASE_URL for real PostgreSQL integration")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	pool, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := pool.Exec(ctx, "SELECT pg_sleep(10)"); err == nil {
		t.Fatal("query did not respect deadline")
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal("pool did not recover after cancellation")
	}
	bad := cfg.Copy()
	bad.ConnConfig.Database = "ant_nonexistent_database"
	if p, err := Open(context.Background(), bad); err == nil {
		p.Close()
		t.Fatal("startup accepted missing database")
	}
}
