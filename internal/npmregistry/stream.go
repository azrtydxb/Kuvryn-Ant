package npmregistry

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var errUpstreamDocument = errors.New("invalid upstream metadata")

// rewritePackument copies an upstream packument to dst one top-level member and
// one version at a time, so memory is bounded by the largest single version
// rather than by the document. Every dist.tarball is replaced by tarball(version);
// versions that are not valid semver are dropped and dist-tags are limited to
// the versions kept. Members it does not interpret are copied byte for byte
// (_rev is removed; name and _id are set to name).
func rewritePackument(dst io.Writer, src io.Reader, name string, tarball func(version string) string) error {
	dec := json.NewDecoder(src)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errUpstreamDocument
	}
	out := bufio.NewWriterSize(dst, 64<<10)
	if _, err := out.WriteString("{"); err != nil {
		return err
	}
	first := true
	member := func(key string, value []byte) error {
		if !first {
			out.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		out.Write(k)
		out.WriteByte(':')
		_, err := out.Write(value)
		return err
	}
	seen := map[string]bool{}
	var tags map[string]string
	versionsDone := false
	for dec.More() {
		t, err := dec.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return errUpstreamDocument
		}
		switch key {
		case "name", "_id", "_rev":
			var skip json.RawMessage
			if err = dec.Decode(&skip); err != nil {
				return errUpstreamDocument
			}
		case "dist-tags":
			var raw json.RawMessage
			if err = dec.Decode(&raw); err != nil {
				return errUpstreamDocument
			}
			_ = json.Unmarshal(raw, &tags)
		case "versions":
			if t, err = dec.Token(); err != nil || t != json.Delim('{') {
				return errUpstreamDocument
			}
			if !first {
				out.WriteByte(',')
			}
			first = false
			out.WriteString(`"versions":{`)
			firstVersion := true
			for dec.More() {
				t, err = dec.Token()
				version, ok := t.(string)
				if err != nil || !ok {
					return errUpstreamDocument
				}
				var raw json.RawMessage
				if err = dec.Decode(&raw); err != nil {
					return errUpstreamDocument
				}
				if !validVersion(version) {
					continue
				}
				value, err := rewriteVersion(raw, tarball(version))
				if err != nil {
					return err
				}
				if !firstVersion {
					out.WriteByte(',')
				}
				firstVersion = false
				k, _ := json.Marshal(version)
				out.Write(k)
				out.WriteByte(':')
				out.Write(value)
				seen[version] = true
			}
			if _, err = dec.Token(); err != nil {
				return errUpstreamDocument
			}
			if _, err = out.WriteString("}"); err != nil {
				return err
			}
			versionsDone = true
		default:
			var raw json.RawMessage
			if err = dec.Decode(&raw); err != nil {
				return errUpstreamDocument
			}
			if err = member(key, raw); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return errUpstreamDocument
	}
	if !versionsDone {
		if err := member("versions", []byte("{}")); err != nil {
			return err
		}
	}
	kept := map[string]string{}
	for tag, version := range tags {
		if seen[version] && validTag(tag) {
			kept[tag] = version
		}
	}
	b, _ := json.Marshal(kept)
	n, _ := json.Marshal(name)
	for _, m := range []struct {
		k string
		v []byte
	}{{"dist-tags", b}, {"name", n}, {"_id", n}} {
		if err := member(m.k, m.v); err != nil {
			return err
		}
	}
	if _, err := out.WriteString("}"); err != nil {
		return err
	}
	return out.Flush()
}

func rewriteVersion(raw json.RawMessage, tarball string) ([]byte, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, errUpstreamDocument
	}
	dist := map[string]json.RawMessage{}
	if d, ok := m["dist"]; ok {
		_ = json.Unmarshal(d, &dist) // a non-object dist is replaced, as before
		if dist == nil {
			dist = map[string]json.RawMessage{}
		}
	}
	dist["tarball"], _ = json.Marshal(tarball)
	m["dist"], _ = json.Marshal(dist)
	return json.Marshal(m)
}

// documentETag derives a validator for a rewritten document from the upstream
// validator (or digest) and everything the rewrite depends on.
func documentETag(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

// lazyWriter starts the response on the first write and refreshes the write
// deadline as bytes flow, so a large document is bounded by idleness only.
type lazyWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	start   func()
	started bool
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		l.start()
	}
	_ = l.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return l.w.Write(p)
}
