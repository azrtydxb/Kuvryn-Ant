package backup

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// ephemeral tables hold short-lived security state (login handshakes, browser
// sessions, replay claims, writer leases, in-flight upload bookkeeping). They are
// excluded: restoring bearer-like session rows or a stale writer lease from a
// backup would be a liability, and they are rebuilt by normal operation. Any
// table not listed here is included automatically, so new migrations are safe.
var ephemeral = map[string]bool{
	"browser_sessions":        true,
	"enterprise_login_states": true,
	"instance_writer_leases":  true,
	"oci_uploads":             true,
	"oidc_login_states":       true,
	"saml_replay_claims":      true,
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func queryStrings(ctx context.Context, q querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// allTables lists base tables of the current schema except the migration ledger,
// which is represented in the manifest instead.
func allTables(ctx context.Context, q querier) ([]string, error) {
	return queryStrings(ctx, q, `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND c.relkind IN ('r','p') AND c.relname<>'schema_migrations' ORDER BY 1`)
}

// tableColumns excludes generated columns, which cannot be loaded with COPY.
func tableColumns(ctx context.Context, q querier, table string) ([]string, error) {
	return queryStrings(ctx, q, `SELECT a.attname::text FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND c.relname=$1 AND a.attnum>0 AND NOT a.attisdropped AND a.attgenerated='' ORDER BY a.attnum`, table)
}

func identityColumns(ctx context.Context, q querier) (map[string][]string, error) {
	rows, err := q.Query(ctx, `SELECT c.relname::text, a.attname::text FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND c.relkind IN ('r','p') AND a.attidentity<>'' AND NOT a.attisdropped ORDER BY 1,2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var t, c string
		if err := rows.Scan(&t, &c); err != nil {
			return nil, err
		}
		out[t] = append(out[t], c)
	}
	return out, rows.Err()
}

// foreignKeys maps each table to the other tables it references.
func foreignKeys(ctx context.Context, q querier) (map[string][]string, error) {
	rows, err := q.Query(ctx, `SELECT c.relname::text, r.relname::text FROM pg_constraint k
 JOIN pg_class c ON c.oid=k.conrelid JOIN pg_class r ON r.oid=k.confrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE k.contype='f' AND n.nspname=current_schema() ORDER BY 1,2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		if child != parent {
			out[child] = append(out[child], parent)
		}
	}
	return out, rows.Err()
}

// loadOrder sorts tables so every table follows the tables it references.
// A reference to a table outside the set, or a cycle, is an error: COPY loads
// cannot satisfy such a constraint.
func loadOrder(tables []string, fks map[string][]string) ([]string, error) {
	in := map[string]bool{}
	for _, t := range tables {
		in[t] = true
	}
	for _, t := range tables {
		for _, p := range fks[t] {
			if !in[p] {
				return nil, fmt.Errorf("%w: table %s references excluded table %s", ErrSchema, t, p)
			}
		}
	}
	sorted := append([]string(nil), tables...)
	sort.Strings(sorted)
	done := map[string]bool{}
	var order []string
	for len(order) < len(sorted) {
		progress := false
		for _, t := range sorted {
			if done[t] {
				continue
			}
			ready := true
			for _, p := range fks[t] {
				if !done[p] {
					ready = false
					break
				}
			}
			if ready {
				done[t] = true
				order = append(order, t)
				progress = true
			}
		}
		if !progress {
			return nil, fmt.Errorf("%w: cyclic foreign keys", ErrSchema)
		}
	}
	return order, nil
}

func quote(name string) string { return pgx.Identifier{name}.Sanitize() }

func quoteList(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += quote(n)
	}
	return out
}
