package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKeyFixture(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestLoadFilesVersionedMountedKeys(t *testing.T) {
	dir := t.TempDir()
	old := bytes.Repeat([]byte{7}, 32)
	active := bytes.Repeat([]byte{9}, 32)
	oldRing, err := New("v1", map[string][]byte{"v1": old})
	if err != nil {
		t.Fatal(err)
	}
	ctx := Context{RecordID: "instance/record", Purpose: "webhook"}
	envelope, err := oldRing.Seal(ctx, []byte("preserved secret"))
	if err != nil {
		t.Fatal(err)
	}
	writeKeyFixture(t, filepath.Join(dir, "old.key"), old)
	writeKeyFixture(t, filepath.Join(dir, "versions.json"), []byte(`{"v1":"old.key"}`))
	ring, err := LoadFiles("v2", active, filepath.Join(dir, "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(active, bytes.Repeat([]byte{9}, 32)) {
		t.Fatal("caller active key cleared")
	}
	clear(active)
	clear(old)
	// The loaded ring owns copies independent of caller memory and mounted files.
	writeKeyFixture(t, filepath.Join(dir, "old.key"), bytes.Repeat([]byte{5}, 32))
	plaintext, err := ring.Open(ctx, envelope)
	if err != nil || string(plaintext) != "preserved secret" {
		t.Fatal("previous key cannot decrypt")
	}
	clear(plaintext)
	rotated, err := ring.Rewrap(ctx, envelope)
	if err != nil || rotated.KeyVersion != "v2" {
		t.Fatal("rotation did not select active version")
	}
	activeOnly, err := LoadFiles("v2", bytes.Repeat([]byte{9}, 32), "")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err = activeOnly.Open(ctx, rotated)
	if err != nil || string(plaintext) != "preserved secret" {
		t.Fatal("active-only ring cannot decrypt rewrapped secret")
	}
	clear(plaintext)
	if _, err = activeOnly.Open(ctx, envelope); !errors.Is(err, ErrUnknownVersion) {
		t.Fatal("old version unexpectedly available")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", ring, ring), "old.key") {
		t.Fatal("ring diagnostics contain file paths")
	}
}
func TestLoadFilesProjectedSymlinksAndAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "projected.key")
	writeKeyFixture(t, keyPath, bytes.Repeat([]byte{3}, 32))
	keyLink := filepath.Join(dir, "mounted.key")
	if err := os.Symlink(keyPath, keyLink); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(map[string]string{"v1": keyLink})
	manifest := filepath.Join(dir, "manifest.json")
	writeKeyFixture(t, manifest, content)
	link := filepath.Join(dir, "mounted.json")
	if err := os.Symlink(manifest, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFiles("v2", make([]byte, 32), link); err != nil {
		t.Fatal("projected files rejected", err)
	}
}
func TestLoadFilesRejectsMalformedManifest(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "valid.key")
	writeKeyFixture(t, key, make([]byte, 32))
	validPath, _ := json.Marshal(key)
	path := string(validPath)
	entries := map[string]string{}
	for i := 0; i < 17; i++ {
		entries[fmt.Sprintf("old%d", i)] = key
	}
	tooMany, _ := json.Marshal(entries)
	cases := [][]byte{
		[]byte(`null`), []byte(`[]`), []byte(`"text"`), []byte(`{}` + `{}`), []byte(`{"v1":null}`), []byte(`{"v1":17}`), []byte(`{"v1":""}`), []byte(`{"v2":` + path + `}`), []byte(`{"invalid version":` + path + `}`), []byte(`{"":` + path + `}`), []byte(`{"v1":` + path + `,"v1":` + path + `}`), []byte(`{"v1":` + path + `,}`), []byte(`{"v1":"\u0000"}`), []byte(`{"v1":"` + strings.Repeat("a", 4097) + `"}`), []byte(`{"v1":"missing"}`), []byte{'{', '"', 'v', '1', '"', ':', '"', 255, '"', '}'}, tooMany, bytes.Repeat([]byte{' '}, 65537),
	}
	for i, body := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			manifest := filepath.Join(dir, "private-manifest.json")
			writeKeyFixture(t, manifest, body)
			_, err := LoadFiles("v2", make([]byte, 32), manifest)
			if err != ErrKeyConfiguration {
				t.Fatalf("invalid manifest accepted or unsafe error: %v", err)
			}
		})
	}
	// Maximum number of previous keys is accepted.
	delete(entries, "old16")
	body, _ := json.Marshal(entries)
	manifest := filepath.Join(dir, "max.json")
	writeKeyFixture(t, manifest, body)
	if _, err := LoadFiles("v2", make([]byte, 32), manifest); err != nil {
		t.Fatal(err)
	}
	writeKeyFixture(t, manifest, []byte("{} \n\t"))
	if _, err := LoadFiles("v2", make([]byte, 32), manifest); err != nil {
		t.Fatal("empty object should be valid")
	}
}
func TestLoadFilesRejectsInvalidKeysAndNonregularFiles(t *testing.T) {
	dir := t.TempDir()
	active := make([]byte, 32)
	for _, version := range []string{"", "bad version", strings.Repeat("v", 65)} {
		if _, err := LoadFiles(version, active, ""); err != ErrKeyConfiguration {
			t.Fatal("invalid active version accepted")
		}
	}
	for _, size := range []int{0, 31, 33, 1024} {
		if _, err := LoadFiles("v2", make([]byte, size), ""); err != ErrKeyConfiguration {
			t.Fatal("invalid active key accepted")
		}
	}
	for _, size := range []int{0, 31, 33, 1024} {
		keyPath := filepath.Join(dir, "private.key")
		writeKeyFixture(t, keyPath, make([]byte, size))
		body, _ := json.Marshal(map[string]string{"v1": keyPath})
		manifest := filepath.Join(dir, "manifest.json")
		writeKeyFixture(t, manifest, body)
		if _, err := LoadFiles("v2", active, manifest); err != ErrKeyConfiguration {
			t.Fatal("invalid previous key accepted")
		}
	}
	if _, err := LoadFiles("v2", active, dir); err != ErrKeyConfiguration {
		t.Fatal("directory manifest accepted")
	}
	body, _ := json.Marshal(map[string]string{"v1": dir})
	manifest := filepath.Join(dir, "directory.json")
	writeKeyFixture(t, manifest, body)
	if _, err := LoadFiles("v2", active, manifest); err != ErrKeyConfiguration {
		t.Fatal("directory key accepted")
	}
	if _, err := LoadFiles("v2", active, filepath.Join(dir, "private-missing")); err != ErrKeyConfiguration {
		t.Fatal("missing manifest accepted")
	}
	// Reject a huge sparse file by metadata, without allocating its advertised size.
	sparse := filepath.Join(dir, "huge.json")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := LoadFiles("v2", active, sparse); err != ErrKeyConfiguration {
		t.Fatal("oversized manifest accepted")
	}
}
