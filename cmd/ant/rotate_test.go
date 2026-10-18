package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
)

func TestLocalRotationIsBoundedResumableAndDoesNotIssueTokens(t *testing.T) {
	pool, cfg := runtimeFixture(t)
	ctx := context.Background()
	owner, err := identity.Bootstrap(ctx, pool, "rotation")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := secrets.New("old", map[string][]byte{"old": bytes.Repeat([]byte{1}, 32)})
	cfg.SecretKeys, _ = secrets.New("new", map[string][]byte{"old": bytes.Repeat([]byte{1}, 32), "new": bytes.Repeat([]byte{2}, 32)})
	previous := secrets.Store{DB: pool, Keys: old}
	var record secrets.Record
	for range 101 {
		record, err = previous.Create(ctx, owner.InstanceID, owner.PrincipalID, "webhook", []byte("private fixture secret"))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int{100, 1, 0} {
		var output bytes.Buffer
		if err = rotateSecrets(ctx, pool, cfg, &output); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Rotated    int `json:"rotated"`
			BatchLimit int `json:"batch_limit"`
		}
		if err = json.Unmarshal(output.Bytes(), &result); err != nil || result.Rotated != want || result.BatchLimit != 100 {
			t.Fatalf("unexpected rotation summary %s", output.String())
		}
		if bytes.Contains(output.Bytes(), []byte("private")) {
			t.Fatal("secret exposed")
		}
	}
	only, _ := secrets.New("new", map[string][]byte{"new": bytes.Repeat([]byte{2}, 32)})
	_, plaintext, err := (secrets.Store{DB: pool, Keys: only}).Open(ctx, owner.InstanceID, record.ID, "webhook")
	if err != nil || string(plaintext) != "private fixture secret" {
		t.Fatal("rotation damaged secret", err)
	}
	clear(plaintext)
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM service_tokens").Scan(&count); err != nil || count != 0 {
		t.Fatal("rotation issued token", err)
	}
	cfg.InstanceID = "00000000-0000-4000-8000-000000000099"
	if err = rotateSecrets(ctx, pool, cfg, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted mismatched instance")
	}
}
