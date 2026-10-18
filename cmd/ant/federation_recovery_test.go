package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
)

// Recovery-key rotation is signed offline: the CLI needs no database or
// network and the recovery private keys stay in local files.
func TestFederationRecoverySigningIsOffline(t *testing.T) {
	t.Setenv("ANT_DATABASE_URL", "")
	t.Setenv("ANT_ROOT_KEY_FILE", "")
	dir := t.TempDir()
	current, currentPub, _ := federation.NewRecoveryKey()
	_, authorityPub, _ := federation.NewRecoveryKey()
	tpl := federation.RecoveryTemplate{FederationID: "11111111-2222-4333-8444-555555555555", Generation: 7, PreviousKey: currentPub,
		Authority: federation.AuthorityCert{Epoch: 1, InstanceID: "66666666-7777-4888-8999-000000000000", PublicKey: authorityPub, CertifiedBy: "recovery"}}
	raw, _ := json.Marshal(tpl)
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tplPath, keyPath := write("template.json", string(raw)), write("current.key", current+"\n")
	newKey := filepath.Join(dir, "new.key")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := func(args ...string) (int, []byte) {
		var out bytes.Buffer
		code := execute(context.Background(), logger, append([]string{"federation", "recovery"}, args...), &out)
		return code, out.Bytes()
	}
	code, out := run("sign-rotation", "--template", tplPath, "--key", keyPath, "--new-key-out", newKey)
	if code != 0 {
		t.Fatalf("sign-rotation: %d", code)
	}
	var rot struct {
		Rotation federation.RecoveryRotation `json:"rotation"`
	}
	if err := json.Unmarshal(out, &rot); err != nil || rot.Rotation.PreviousKey != currentPub || rot.Rotation.Generation != 7 || rot.Rotation.Signature == "" {
		t.Fatalf("rotation output: %s", out)
	}
	if strings.Contains(string(out), "ant-recovery-v1:") {
		t.Fatal("a private key was printed")
	}
	info, err := os.Stat(newKey)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("new key file: %v %v", info, err)
	}
	nk, _ := os.ReadFile(newKey)
	if pub, err := federation.RecoveryPublicKey(strings.TrimSpace(string(nk))); err != nil || pub != rot.Rotation.NewKey {
		t.Fatalf("new key file does not hold the rotated key: %v", err)
	}
	// Never overwrites an existing key file.
	if code, _ := run("sign-rotation", "--template", tplPath, "--key", keyPath, "--new-key-out", newKey); code == 0 {
		t.Fatal("sign-rotation overwrote an existing key file")
	}
	// The current key must match the template.
	other, _, _ := federation.NewRecoveryKey()
	if code, _ := run("sign-rotation", "--template", tplPath, "--key", write("other.key", other), "--new-key-out", filepath.Join(dir, "x.key")); code == 0 {
		t.Fatal("signed with a key that is not the current recovery key")
	}
	rotPath := write("rotation.json", string(out))
	code, out = run("sign-authority-cert", "--template", tplPath, "--key", newKey, "--rotation", rotPath)
	if code != 0 {
		t.Fatalf("sign-authority-cert: %d", code)
	}
	var req federation.SignedRecoveryRotation
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Rotation != rot.Rotation || req.AuthorityCertificate.PublicKey != authorityPub ||
		req.AuthorityCertificate.CertifiedBy != "recovery" || req.AuthorityCertificate.Signature == "" {
		t.Fatalf("request output: %s (%v)", out, err)
	}
	// The authority certificate must be signed with the rotation's new key.
	if code, _ := run("sign-authority-cert", "--template", tplPath, "--key", keyPath, "--rotation", rotPath); code == 0 {
		t.Fatal("authority certificate signed with the old key against a rotation")
	}
	// Keys are never read from arguments or standard input.
	if code, _ := run("sign-rotation", "--template", tplPath, "--key", "-", "--new-key-out", filepath.Join(dir, "y.key")); code == 0 {
		t.Fatal("key read from standard input")
	}
}
