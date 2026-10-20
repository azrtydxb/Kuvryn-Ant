package database

import (
	"context"
	"testing"
)

// Migration 057 turns the webhook slug into a unique display name, keeps every
// endpoint and signing secret, labels legacy unnamed webhooks and drops the
// instance webhook base URL.
func TestWebhookDisplayNameMigrationKeepsWebhooks(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	definitions, err := loadMigrations(migrations)
	if err != nil {
		t.Fatal(err)
	}
	var before []migration
	for _, m := range definitions {
		if m.name != "webhook_display_names" && m.version < 57 {
			before = append(before, m)
		}
	}
	if len(before) == len(definitions) {
		t.Fatal("migration 057 missing")
	}
	if err = applyMigrations(ctx, pool, before); err != nil {
		t.Fatal(err)
	}
	var instance, secretRow, secret, slugHook, customHook string
	if err = pool.QueryRow(ctx, `INSERT INTO instances(name) VALUES('names') RETURNING id::text`).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO encrypted_secrets(instance_id,purpose,envelope,key_version) VALUES($1,'webhook','{"key_version":"v1"}','v1') RETURNING id::text`, instance).Scan(&secretRow); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO webhook_signing_secrets(instance_id,name,secret_id) VALUES($1,'shared',$2) RETURNING id::text`, instance, secretRow).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO instance_settings(instance_id,webhook_base_url) VALUES($1,'https://hooks.example.com/ant/') ON CONFLICT (instance_id) DO UPDATE SET webhook_base_url=excluded.webhook_base_url`, instance); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,name,endpoint,custom_endpoint,signing_secret_id) VALUES($1,'deploys','https://hooks.example.com/ant/deploys',false,$2) RETURNING id::text`, instance, secret).Scan(&slugHook); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,endpoint,custom_endpoint,signing_secret_id) VALUES($1,'https://legacy.example.com/hook',true,$2) RETURNING id::text`, instance, secret).Scan(&customHook); err != nil {
		t.Fatal(err)
	}
	if err = applyMigrations(ctx, pool, definitions); err != nil {
		t.Fatal(err)
	}
	var name, endpoint, signing string
	if err = pool.QueryRow(ctx, `SELECT name,endpoint,signing_secret_id::text FROM event_subscriptions WHERE id=$1`, slugHook).Scan(&name, &endpoint, &signing); err != nil {
		t.Fatal(err)
	}
	if name != "deploys" || endpoint != "https://hooks.example.com/ant/deploys" || signing != secret {
		t.Fatalf("slug webhook: %s %s %s", name, endpoint, signing)
	}
	if err = pool.QueryRow(ctx, `SELECT name,endpoint,signing_secret_id::text FROM event_subscriptions WHERE id=$1`, customHook).Scan(&name, &endpoint, &signing); err != nil {
		t.Fatal(err)
	}
	if name != "webhook-"+customHook[:8] || endpoint != "https://legacy.example.com/hook" || signing != secret {
		t.Fatalf("custom webhook: %s %s %s", name, endpoint, signing)
	}
	var columns int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE (table_name='instance_settings' AND column_name='webhook_base_url') OR (table_name='event_subscriptions' AND column_name='custom_endpoint')`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("obsolete columns remain: %d %v", columns, err)
	}
	// Names are unique per instance, ignoring case, and always present.
	if _, err = pool.Exec(ctx, `INSERT INTO event_subscriptions(instance_id,name,endpoint,signing_secret_id) VALUES($1,'DEPLOYS','https://x.example.com/h',$2)`, instance, secret); err == nil {
		t.Fatal("duplicate display name accepted")
	}
	var generated string
	if err = pool.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,endpoint,signing_secret_id) VALUES($1,'https://y.example.com/h',$2) RETURNING name`, instance, secret).Scan(&generated); err != nil || generated == "" {
		t.Fatal("unnamed insert did not get a default name", err)
	}
}
