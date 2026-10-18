package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var caReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Operator-supplied map only. Management requests may select an ID but can never
// supply a filesystem path. Relative paths are relative to this mounted map.
func upstreamCAs(path string) (map[string]string, error) {
	result := map[string]string{}
	if path == "" {
		return result, nil
	}
	invalid := errors.New("ANT_UPSTREAM_CA_MAP_FILE is invalid")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, invalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, invalid
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, invalid
	}
	decoder := json.NewDecoder(io.LimitReader(f, 65537))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalid
	}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, invalid
		}
		id, ok := token.(string)
		if !ok || !caReference.MatchString(id) || len(result) >= 32 {
			return nil, invalid
		}
		if _, exists := result[id]; exists {
			return nil, invalid
		}
		var mounted string
		if decoder.Decode(&mounted) != nil || len(mounted) == 0 || len(mounted) > 4096 {
			return nil, invalid
		}
		if !filepath.IsAbs(mounted) {
			mounted = filepath.Join(filepath.Dir(path), mounted)
		}
		result[id] = filepath.Clean(mounted)
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return nil, invalid
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, invalid
	}
	return result, nil
}
