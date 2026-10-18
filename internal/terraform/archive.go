package terraform

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

const (
	maxModuleArchive  = 128 << 20
	maxModuleExpanded = 512 << 20
	maxModuleEntries  = 10000
	maxProviderZip    = 512 << 20
	maxZipEntries     = 256
	maxZipExpanded    = 2 << 30
	maxSmallPart      = 256 << 10
	maxPlatforms      = 12 // PublishBundle caps a version at 16 assets: 4 fixed + platforms
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// spooled is a verified-size temporary copy of an upload body.
type spooled struct {
	f    *os.File
	info struct {
		size   int64
		digest string
	}
}

func (s *spooled) close() {
	if s != nil && s.f != nil {
		_ = s.f.Close()
		_ = os.Remove(s.f.Name())
	}
}

// spool copies at most limit bytes to a temporary file while hashing; larger
// input is rejected rather than truncated.
func spool(ctx context.Context, r io.Reader, limit int64) (*spooled, error) {
	f, e := os.CreateTemp("", "ant-terraform-*")
	if e != nil {
		return nil, e
	}
	s := &spooled{f: f}
	h := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, r}, limit+1), make([]byte, 64<<10))
	if e != nil || n > limit {
		s.close()
		return nil, errInvalid
	}
	s.info.size = n
	s.info.digest = hex.EncodeToString(h.Sum(nil))
	if _, e = f.Seek(0, 0); e != nil {
		s.close()
		return nil, e
	}
	return s, nil
}

func safeName(n string) bool {
	if n == "" || len(n) > 512 || !utf8.ValidString(n) || strings.ContainsAny(n, "\x00\\") || strings.HasPrefix(n, "/") {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(n, "/"), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// inspectModule validates a module archive without extracting it: bounded
// gzip+tar, safe names, no devices/hardlinks/escaping symlinks, and at least
// one root-level Terraform configuration file (the closure Terraform needs to
// load the module). Nothing in the archive is executed.
func inspectModule(ctx context.Context, f io.Reader) error {
	g, e := gzip.NewReader(contextReader{ctx, f})
	if e != nil {
		return errInvalid
	}
	limited := &io.LimitedReader{R: contextReader{ctx, g}, N: maxModuleExpanded + 1}
	t := tar.NewReader(limited)
	config := false
	entries := 0
	for {
		h, e := t.Next()
		if e == io.EOF {
			break
		}
		if e != nil || limited.N <= 0 {
			return errInvalid
		}
		entries++
		if entries > maxModuleEntries || !safeName(h.Name) {
			return errInvalid
		}
		clean := path.Clean(h.Name)
		switch h.Typeflag {
		case tar.TypeReg:
			if !strings.Contains(clean, "/") && (strings.HasSuffix(clean, ".tf") || strings.HasSuffix(clean, ".tf.json")) {
				config = true
			}
		case tar.TypeDir:
		case tar.TypeSymlink:
			target := h.Linkname
			if target == "" || strings.HasPrefix(target, "/") || strings.ContainsAny(target, "\x00\\") {
				return errInvalid
			}
			if joined := path.Join(path.Dir(clean), target); joined == ".." || strings.HasPrefix(joined, "../") {
				return errInvalid
			}
		case tar.TypeXGlobalHeader:
		default:
			return errInvalid
		}
	}
	// Drain trailing bytes so the gzip trailer/CRC is verified.
	if _, e = io.Copy(io.Discard, limited); e != nil || limited.N <= 0 {
		return errInvalid
	}
	if !config {
		return errInvalid
	}
	return nil
}

// inspectProviderZip checks the zip structure and that it contains the plugin
// binary terraform-provider-{typ}[_v...][.exe] at its root.
func inspectProviderZip(f io.ReaderAt, size int64, typ string) error {
	z, e := zip.NewReader(f, size)
	if e != nil || len(z.File) == 0 || len(z.File) > maxZipEntries {
		return errInvalid
	}
	var total uint64
	binary := false
	prefix := "terraform-provider-" + typ
	for _, file := range z.File {
		if !safeName(file.Name) || file.Mode()&os.ModeSymlink != 0 || file.Mode()&os.ModeDevice != 0 {
			return errInvalid
		}
		total += file.UncompressedSize64
		if total > maxZipExpanded {
			return errInvalid
		}
		if !strings.Contains(file.Name, "/") && !file.FileInfo().IsDir() {
			base := strings.TrimSuffix(file.Name, ".exe")
			if base == prefix || strings.HasPrefix(base, prefix+"_v") {
				binary = true
			}
		}
	}
	if !binary {
		return errInvalid
	}
	return nil
}

// parseSums strictly parses a SHA256SUMS file into filename -> hex digest.
func parseSums(b []byte) (map[string]string, error) {
	if len(b) == 0 || len(b) > maxSmallPart || !utf8.Valid(b) || bytes.ContainsAny(b, "\x00\r") {
		return nil, errInvalid
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 || strings.ToLower(sum) != sum || name == "" || strings.ContainsAny(name, "/ *") {
			return nil, errInvalid
		}
		if _, e := hex.DecodeString(sum); e != nil {
			return nil, errInvalid
		}
		if _, dup := out[name]; dup {
			return nil, errInvalid
		}
		out[name] = sum
	}
	return out, nil
}

// signingKey is a parsed single-entity public key.
type signingKey struct {
	armored string
	keyID   string
	ring    openpgp.EntityList
}

func parseKey(b []byte) (signingKey, error) {
	if len(b) == 0 || len(b) > maxSmallPart {
		return signingKey{}, errInvalid
	}
	ring, e := openpgp.ReadArmoredKeyRing(bytes.NewReader(b))
	if e != nil || len(ring) != 1 {
		return signingKey{}, errInvalid
	}
	if ring[0].PrivateKey != nil {
		return signingKey{}, errInvalid // never accept or store secret key material
	}
	return signingKey{armored: string(b), keyID: strings.ToUpper(fmt.Sprintf("%016x", ring[0].PrimaryKey.KeyId)), ring: ring}, nil
}

// verifySignature checks the detached signature over the SHA256SUMS bytes with
// the supplied key and returns the signature in binary form (what Terraform
// reads). An ASCII-armored signature is accepted and normalized.
func verifySignature(key signingKey, sums, sig []byte) ([]byte, error) {
	if len(sig) == 0 || len(sig) > maxSmallPart {
		return nil, errInvalid
	}
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN PGP")) {
		blk, e := armor.Decode(bytes.NewReader(sig))
		if e != nil {
			return nil, errInvalid
		}
		sig, e = io.ReadAll(io.LimitReader(blk.Body, maxSmallPart+1))
		if e != nil || len(sig) > maxSmallPart {
			return nil, errInvalid
		}
	}
	signer, e := openpgp.CheckDetachedSignature(key.ring, bytes.NewReader(sums), bytes.NewReader(sig), nil)
	if e != nil || signer == nil || signer.PrimaryKey.KeyId != key.ring[0].PrimaryKey.KeyId {
		return nil, errInvalid
	}
	return sig, nil
}
