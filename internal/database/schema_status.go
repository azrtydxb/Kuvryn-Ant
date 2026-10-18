package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaState compares the applied schema history with this binary's embedded
// migrations. It never modifies the database.
type SchemaState struct {
	Applied   int        `json:"applied_version"`
	Latest    int        `json:"binary_version"`
	Pending   int        `json:"pending"`
	Newer     bool       `json:"database_newer_than_binary"`
	Current   bool       `json:"current"`
	AppliedAt *time.Time `json:"last_applied_at,omitempty"`
}

// SchemaStatus reads schema_migrations without taking the migration lock. A
// missing history table reports version 0.
func SchemaStatus(ctx context.Context, pool *pgxpool.Pool) (SchemaState, error) {
	defs, err := Migrations()
	if err != nil {
		return SchemaState{}, err
	}
	s := SchemaState{Latest: len(defs)}
	var exists bool
	if err = pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return s, errors.New("cannot read schema history")
	}
	if exists {
		if err = pool.QueryRow(ctx, `SELECT COALESCE(max(version),0),max(applied_at) FROM schema_migrations`).Scan(&s.Applied, &s.AppliedAt); err != nil {
			return s, errors.New("cannot read schema history")
		}
	}
	s.Newer = s.Applied > s.Latest
	if s.Applied < s.Latest {
		s.Pending = s.Latest - s.Applied
	}
	s.Current = s.Applied == s.Latest
	return s, nil
}
