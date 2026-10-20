package database

import (
	"context"
	"testing"
)

// Migration 050 adopts each inline webhook secret as a signing secret named after
// the webhook, reusing the same ciphertext row so delivery signatures are unchanged.
func TestWebhookSigningSecretMigrationPreservesInlineSecrets(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	definitions, err := loadMigrations(migrations)
	if err != nil {
		t.Fatal(err)
	}
	var before []migration
	for _, m := range definitions {
		if m.name != "webhook_signing_secrets" && m.version < 50 {
			before = append(before, m)
		}
	}
	if len(before) == len(definitions) {
		t.Fatal("migration 050 missing")
	}
	if err = applyMigrations(ctx, pool, before); err != nil {
		t.Fatal(err)
	}
	var instance, secret, linked, opaque string
	if err = pool.QueryRow(ctx, `INSERT INTO instances(name) VALUES('legacy') RETURNING id::text`).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO encrypted_secrets(instance_id,purpose,envelope,key_version) VALUES($1,'webhook','{"key_version":"v1"}','v1') RETURNING id::text`, instance).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,endpoint,secret_ref) VALUES($1,'https://legacy.example.com/hook',$2) RETURNING id::text`, instance, secret).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,endpoint,secret_ref) VALUES($1,'https://other.example.com/hook','not-a-uuid') RETURNING id::text`, instance).Scan(&opaque); err != nil {
		t.Fatal(err)
	}
	if err = applyMigrations(ctx, pool, definitions); err != nil {
		t.Fatal(err)
	}
	var name, ref, subRef string
	var signing string
	if err = pool.QueryRow(ctx, `SELECT w.name,w.secret_id::text,COALESCE(s.secret_ref,''),s.signing_secret_id::text FROM event_subscriptions s JOIN webhook_signing_secrets w ON w.id=s.signing_secret_id WHERE s.id=$1`, linked).Scan(&name, &ref, &subRef, &signing); err != nil {
		t.Fatal(err)
	}
	if name != "webhook-"+linked[:8] || ref != secret || subRef != "" || signing != linked {
		t.Fatalf("migrated webhook: name=%s ref=%s sub_ref=%q", name, ref, subRef)
	}
	// A subscription whose reference is not an encrypted webhook secret keeps working as legacy.
	var legacyRef string
	var legacySigning *string
	if err = pool.QueryRow(ctx, `SELECT secret_ref,signing_secret_id::text FROM event_subscriptions WHERE id=$1`, opaque).Scan(&legacyRef, &legacySigning); err != nil || legacyRef != "not-a-uuid" || legacySigning != nil {
		t.Fatal("legacy subscription altered", err)
	}
}
