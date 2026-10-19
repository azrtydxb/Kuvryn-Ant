package federation

import (
	"context"
	"errors"
	"fmt"

	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Offline maintenance behind `ant federation re-identify` and `ant federation
// purge` (§13 cloned instance, §15 downgrade). Both refuse to run while a
// serving process holds the instance writer lease.

// ErrLeaseHeld reports a live serving process for the instance.
var ErrLeaseHeld = errors.New("federation: the instance writer lease is held; stop the serving process first")

// ErrStillFederated reports a purge of an instance that is still a member.
var ErrStillFederated = errors.New("federation: the instance is still an active member; leave the federation first")

func checkLease(ctx context.Context, tx pgx.Tx, instance string) error {
	var live bool
	err := tx.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM instance_writer_leases WHERE instance_id=$1`, instance).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if live {
		return ErrLeaseHeld
	}
	return nil
}

func quoteIdent(s string) string { return pgx.Identifier{s}.Sanitize() }

// resetFederationRows removes all federation state, documents, invitations,
// nonces, drafts, bindings, federation-managed identity rows and federation
// key material of an instance. The caller disables the append-only trigger or
// all user triggers first.
func resetFederationRows(ctx context.Context, tx pgx.Tx, instance string) error {
	if err := dropFederatedIdentity(ctx, tx, instance); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM federation_documents WHERE instance_id=$1`,
		`DELETE FROM federation_members WHERE instance_id=$1`,
		`DELETE FROM federation_invitations WHERE instance_id=$1`,
		`DELETE FROM federation_assertion_nonces WHERE instance_id=$1`,
		`DELETE FROM federation_policy_drafts WHERE instance_id=$1`,
		`DELETE FROM federated_feed_bindings WHERE instance_id=$1`,
		`DELETE FROM federation_quarantines WHERE instance_id=$1`,
		`DELETE FROM federation_route_cache WHERE instance_id=$1`,
		`DELETE FROM replication_peers p WHERE p.instance_id=$1 AND p.auth_mode='federation' AND NOT EXISTS(SELECT 1 FROM replication_jobs j WHERE j.instance_id=p.instance_id AND j.peer_id=p.id)`,
		`DELETE FROM federation_state WHERE instance_id=$1`,
		`DELETE FROM encrypted_secrets WHERE instance_id=$1 AND purpose LIKE 'federation-%'`,
	} {
		if _, err := tx.Exec(ctx, q, instance); err != nil {
			return err
		}
	}
	return nil
}

// Purge deletes every federation row of an instance that left or was removed
// (or never federated), so a binary downgrade sees no federation data. It is
// deliberate and irreversible: the local document log is deleted too.
func Purge(ctx context.Context, pool *pgxpool.Pool, instance string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := checkLease(ctx, tx, instance); err != nil {
		return err
	}
	st, err := loadState(ctx, tx, instance)
	if err != nil {
		return err
	}
	if st.State == "active" || st.State == "joining" {
		return ErrStillFederated
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE federation_documents DISABLE TRIGGER federation_documents_immutable`); err != nil {
		return err
	}
	if err := resetFederationRows(ctx, tx, instance); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE federation_documents ENABLE TRIGGER federation_documents_immutable`); err != nil {
		return err
	}
	if err := audit(ctx, tx, instance, "", "federation.purged", st.FederationID, "success", map[string]any{"previous_state": st.State, "federation_name": st.Name}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReIdentify gives a restored copy of an instance a new instance ID before it
// joins a federation (a clone of a member is otherwise refused as a duplicate).
// Every UUID column that holds the old instance ID is rewritten in one
// transaction (foreign keys are deferred for its duration and user triggers
// are suspended so append-only logs and change feeds are not disturbed),
// encrypted secrets are re-sealed under the new ID, and the federation state
// of the copy is reset to none (its member key and documents are deleted).
// Set ANT_INSTANCE_ID to the returned ID if it is configured.
func ReIdentify(ctx context.Context, pool *pgxpool.Pool, keys *secrets.Keyring, old string) (string, error) {
	if keys == nil {
		return "", ErrKeysUnavailable
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE instances IN ACCESS EXCLUSIVE MODE`); err != nil {
		return "", err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM instances WHERE id=$1)`, old).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", ErrNotFound
	}
	if err := checkLease(ctx, tx, old); err != nil {
		return "", err
	}
	var fresh string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&fresh); err != nil {
		return "", err
	}
	type constraint struct{ table, name string }
	var fks []constraint
	rows, err := tx.Query(ctx, `SELECT c.conrelid::regclass::text,c.conname FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid
WHERE c.contype='f' AND NOT c.condeferrable AND t.relnamespace=current_schema()::regnamespace ORDER BY 1,2`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var c constraint
		if err := rows.Scan(&c.table, &c.name); err != nil {
			rows.Close()
			return "", err
		}
		fks = append(fks, c)
	}
	rows.Close()
	var triggered []string
	if rows, err = tx.Query(ctx, `SELECT DISTINCT t.tgrelid::regclass::text FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
WHERE NOT t.tgisinternal AND c.relnamespace=current_schema()::regnamespace ORDER BY 1`); err != nil {
		return "", err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return "", err
		}
		triggered = append(triggered, t)
	}
	rows.Close()
	type column struct{ table, name string }
	var cols []column
	if rows, err = tx.Query(ctx, `SELECT c.table_name,c.column_name FROM information_schema.columns c JOIN information_schema.tables t ON (t.table_schema,t.table_name)=(c.table_schema,c.table_name)
WHERE c.table_schema=current_schema() AND c.data_type='uuid' AND t.table_type='BASE TABLE' ORDER BY 1,2`); err != nil {
		return "", err
	}
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name); err != nil {
			rows.Close()
			return "", err
		}
		cols = append(cols, c)
	}
	rows.Close()
	// regclass text may be schema-qualified or quoted already; it is safe to
	// splice because it comes from the catalog, not from input.
	for _, c := range fks {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ALTER CONSTRAINT %s DEFERRABLE INITIALLY DEFERRED`, c.table, quoteIdent(c.name))); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		return "", err
	}
	for _, t := range triggered {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER USER`, t)); err != nil {
			return "", err
		}
	}
	rewritten := 0
	for _, c := range cols {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET %s=$2 WHERE %s=$1`, quoteIdent(c.table), quoteIdent(c.name), quoteIdent(c.name)), old, fresh)
		if err != nil {
			return "", fmt.Errorf("rewrite %s.%s: %w", c.table, c.name, err)
		}
		rewritten += int(tag.RowsAffected())
	}
	if err := resetFederationRows(ctx, tx, fresh); err != nil {
		return "", err
	}
	sealed, err := (secrets.Store{Keys: keys}).RebindInstanceTx(ctx, tx, old, fresh)
	if err != nil {
		return "", fmt.Errorf("re-seal secrets: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		return "", err
	}
	for _, t := range triggered {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE TRIGGER USER`, t)); err != nil {
			return "", err
		}
	}
	for _, c := range fks {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ALTER CONSTRAINT %s NOT DEFERRABLE`, c.table, quoteIdent(c.name))); err != nil {
			return "", err
		}
	}
	if err := audit(ctx, tx, fresh, "", "instance.reidentified", fresh, "success", map[string]any{"previous_instance_id": old, "rows_rewritten": rewritten, "secrets_resealed": sealed}); err != nil {
		return "", err
	}
	return fresh, tx.Commit(ctx)
}
