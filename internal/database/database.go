// Package database owns PostgreSQL connectivity and schema evolution.
package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Open(ctx context.Context, config *pgxpool.Config) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("cannot initialize PostgreSQL pool")
	}
	var version int
	if err = pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		pool.Close()
		// Driver errors can contain connection parameters. Keep startup output redacted.
		return nil, errors.New("PostgreSQL connection check failed")
	}
	if version < 170000 || version >= 190000 {
		pool.Close()
		return nil, errors.New("PostgreSQL major version must be 17 or 18")
	}
	return pool, nil
}
