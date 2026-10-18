package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationInfo identifies one embedded migration without exposing its SQL.
type MigrationInfo struct {
	Version        int
	Name, Checksum string
}

// Migrations lists the migrations embedded in this binary in version order.
func Migrations() ([]MigrationInfo, error) {
	definitions, err := loadMigrations(migrations)
	if err != nil {
		return nil, err
	}
	result := make([]MigrationInfo, len(definitions))
	for i, m := range definitions {
		result[i] = MigrationInfo{m.version, m.name, m.checksum}
	}
	return result, nil
}

// MigrateTo applies pending migrations up to and including target. Targets that
// are older than the applied history are refused like any downgrade. Restore
// uses it to recreate the exact schema a backup was taken from.
func MigrateTo(ctx context.Context, pool *pgxpool.Pool, target int) error {
	definitions, err := loadMigrations(migrations)
	if err != nil {
		return err
	}
	if target < 1 || target > len(definitions) {
		return errors.New("schema version is not supported by this binary")
	}
	return applyMigrations(ctx, pool, definitions[:target])
}
