package secrets

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxKeyManifestBytes = 64 << 10
const maxPreviousKeys = 16
const maxMountedPathBytes = 4096

// LoadFiles builds a versioned keyring from an already loaded active key and an
// optional JSON object mapping at most 16 previous versions to mounted key-file
// paths. Relative paths resolve against the manifest's directory. Symlinks are
// allowed for Kubernetes projected volumes; targets must be regular files.
// Errors never include file paths or contents. The caller retains activeKey;
// temporary previous-key buffers are cleared after New copies them.
func LoadFiles(active string, activeKey []byte, previousFile string) (*Keyring, error) {
	if !versionPattern.MatchString(active) || len(activeKey) != 32 {
		return nil, ErrKeyConfiguration
	}
	keys := map[string][]byte{active: activeKey}
	defer func() {
		for version, key := range keys {
			if version != active {
				clear(key)
			}
		}
	}()
	if previousFile != "" {
		if !validMountedPath(previousFile) {
			return nil, ErrKeyConfiguration
		}
		// Stat before open avoids blocking on an accidentally configured FIFO or
		// device. Stat again after open validates the actual mounted file descriptor.
		info, err := os.Stat(previousFile)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxKeyManifestBytes {
			return nil, ErrKeyConfiguration
		}
		f, err := os.Open(previousFile)
		if err != nil {
			return nil, ErrKeyConfiguration
		}
		defer f.Close()
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxKeyManifestBytes {
			return nil, ErrKeyConfiguration
		}
		data, err := io.ReadAll(io.LimitReader(f, maxKeyManifestBytes+1))
		if err != nil || len(data) > maxKeyManifestBytes || !utf8.Valid(data) {
			return nil, ErrKeyConfiguration
		}
		files, err := decodeKeyManifest(data, active)
		if err != nil {
			return nil, err
		}
		for version, path := range files {
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(previousFile), path)
			}
			key, err := readMountedKey(path)
			if err != nil {
				return nil, err
			}
			keys[version] = key
		}
	}
	return New(active, keys)
}
func validMountedPath(path string) bool {
	return len(path) > 0 && len(path) <= maxMountedPathBytes && utf8.ValidString(path) && !strings.ContainsRune(path, 0)
}
func decodeKeyManifest(data []byte, active string) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrKeyConfiguration
	}
	files := map[string]string{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, ErrKeyConfiguration
		}
		version, ok := token.(string)
		if !ok || !versionPattern.MatchString(version) || version == active || len(files) >= maxPreviousKeys {
			return nil, ErrKeyConfiguration
		}
		if _, exists := files[version]; exists {
			return nil, ErrKeyConfiguration
		}
		var path *string
		if decoder.Decode(&path) != nil || path == nil || !validMountedPath(*path) {
			return nil, ErrKeyConfiguration
		}
		files[version] = *path
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrKeyConfiguration
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, ErrKeyConfiguration
	}
	return files, nil
}
func readMountedKey(path string) ([]byte, error) {
	if !validMountedPath(path) {
		return nil, ErrKeyConfiguration
	}
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() != 32 {
		return nil, ErrKeyConfiguration
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrKeyConfiguration
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 {
		return nil, ErrKeyConfiguration
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, ErrKeyConfiguration
	}
	return key, nil
}
