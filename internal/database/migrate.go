package database

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type migration struct {
	version             int
	name, sql, checksum string
}

func loadMigrations(files fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, err
	}
	result := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		number, _, ok := strings.Cut(entry.Name(), "_")
		v, err := strconv.Atoi(number)
		if !ok || err != nil || v != len(result)+1 {
			return nil, errors.New("migrations must be contiguous from version 1")
		}
		content, err := fs.ReadFile(files, "migrations/"+entry.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		result = append(result, migration{v, entry.Name(), string(content), hex.EncodeToString(sum[:])})
	}
	return result, nil
}

// Migrate serializes schema changes and atomically applies pending migrations.
// A failed migration leaves the previous schema intact. Downgrades are refused.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	definitions, err := loadMigrations(migrations)
	if err != nil {
		return err
	}
	return applyMigrations(ctx, pool, definitions)
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool, definitions []migration) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return errors.New("cannot start schema transaction")
	}
	defer tx.Rollback(context.Background())
	// Transaction-scoped lock also covers first-time schema table creation.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(741812093)"); err != nil {
		return errors.New("cannot acquire schema migration lock")
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
 version integer PRIMARY KEY CHECK(version>0), name text NOT NULL,
 checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return errors.New("cannot initialize schema history")
	}
	rows, err := tx.Query(ctx, "SELECT version,name,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return errors.New("cannot read schema history")
	}
	applied := 0
	for rows.Next() {
		var v int
		var name, checksum string
		if err = rows.Scan(&v, &name, &checksum); err != nil {
			rows.Close()
			return errors.New("cannot read schema version")
		}
		if v != applied+1 || v > len(definitions) {
			rows.Close()
			return errors.New("unsupported schema version or non-contiguous history")
		}
		if name != definitions[v-1].name || checksum != definitions[v-1].checksum {
			rows.Close()
			return errors.New("schema migration checksum mismatch")
		}
		applied++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return errors.New("cannot read schema history")
	}
	for _, m := range definitions[applied:] {
		if _, err = tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("schema migration %d failed; transaction rolled back", m.version)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version,name,checksum) VALUES($1,$2,$3)", m.version, m.name, m.checksum); err != nil {
			return errors.New("cannot record schema migration")
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("cannot commit schema migrations")
	}
	return nil
}
