package apkrepo

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- APK v2 control checksums and RSA signatures are SHA-1 by specification.
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxPackage  = 64 << 20
	maxSegment  = 4 << 20
	maxInflated = 1 << 30
	maxField    = 8 << 10
)

var errInvalid = errors.New("invalid apk package")
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
var versionPattern = regexp.MustCompile(`^[0-9][A-Za-z0-9._+~-]{0,127}$`)
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var archPattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// counter tracks exactly how many bytes the gzip reader consumed so segment
// boundaries (and thus the control-segment checksum) are exact.
type counter struct {
	br *bufio.Reader
	n  int64
}

func (c *counter) Read(b []byte) (int, error) {
	n, e := c.br.Read(b)
	c.n += int64(n)
	return n, e
}
func (c *counter) ReadByte() (byte, error) {
	b, e := c.br.ReadByte()
	if e == nil {
		c.n++
	}
	return b, e
}

type pkginfo struct{ fields map[string][]string }

func (p pkginfo) one(k string) string {
	if v := p.fields[k]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func parsePkginfo(b []byte) (pkginfo, error) {
	if !utf8.Valid(b) || bytes.ContainsAny(b, "\x00\r") {
		return pkginfo{}, errInvalid
	}
	p := pkginfo{fields: map[string][]string{}}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, " = ")
		if !ok || k == "" || len(v) > maxField {
			return pkginfo{}, errInvalid
		}
		p.fields[k] = append(p.fields[k], v)
	}
	return p, nil
}

// inspect reads the optional signature and the control segments without
// executing or extracting anything, validates that a complete data segment
// follows, and returns the APKINDEX entry block for the package.
func inspect(f *os.File, size int64, stem, arch string) (string, error) {
	c := &counter{br: bufio.NewReaderSize(f, 64<<10)}
	var gz *gzip.Reader
	var info pkginfo
	var controlStart, controlEnd int64
	found := false
	for i := 0; i < 2 && !found; i++ {
		start := c.n
		var e error
		if gz == nil {
			gz, e = gzip.NewReader(c)
		} else {
			e = gz.Reset(c)
		}
		if e != nil {
			return "", errInvalid
		}
		gz.Multistream(false)
		tr := tar.NewReader(io.LimitReader(gz, maxSegment))
		sig := false
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil || h.Size > maxSegment {
				return "", errInvalid
			}
			switch {
			case strings.HasPrefix(h.Name, ".SIGN."):
				sig = true
			case h.Name == ".PKGINFO":
				b, e := io.ReadAll(io.LimitReader(tr, maxSegment))
				if e != nil {
					return "", errInvalid
				}
				if info, e = parsePkginfo(b); e != nil {
					return "", e
				}
				found = true
			}
		}
		if _, e = io.Copy(io.Discard, io.LimitReader(gz, maxSegment)); e != nil {
			return "", errInvalid
		}
		if found {
			controlStart, controlEnd = start, c.n
		} else if !sig || i == 1 {
			return "", errInvalid
		}
	}
	if !found {
		return "", errInvalid
	}
	if e := gz.Reset(c); e != nil {
		return "", errInvalid
	}
	gz.Multistream(false)
	if n, e := io.Copy(io.Discard, io.LimitReader(gz, maxInflated+1)); e != nil || n > maxInflated {
		return "", errInvalid
	}
	name, ver, pa := info.one("pkgname"), info.one("pkgver"), info.one("arch")
	if !namePattern.MatchString(name) || !versionPattern.MatchString(ver) || name+"-"+ver != stem || (pa != arch && pa != "noarch") || !archPattern.MatchString(pa) {
		return "", errInvalid
	}
	h := sha1.New() // #nosec G401
	if _, e := io.Copy(h, io.NewSectionReader(f, controlStart, controlEnd-controlStart)); e != nil {
		return "", errInvalid
	}
	installed := info.one("size")
	if _, e := strconv.ParseUint(installed, 10, 63); installed != "" && e != nil {
		return "", errInvalid
	}
	var b strings.Builder
	add := func(k, v string) {
		if v != "" {
			b.WriteString(k + ":" + v + "\n")
		}
	}
	add("C", "Q1"+base64.StdEncoding.EncodeToString(h.Sum(nil)))
	add("P", name)
	add("V", ver)
	add("A", pa)
	add("S", strconv.FormatInt(size, 10))
	add("I", installed)
	add("T", info.one("pkgdesc"))
	add("U", info.one("url"))
	add("L", info.one("license"))
	add("o", info.one("origin"))
	add("m", info.one("maintainer"))
	add("t", info.one("builddate"))
	add("c", info.one("commit"))
	add("D", strings.Join(info.fields["depend"], " "))
	add("p", strings.Join(info.fields["provides"], " "))
	add("i", strings.Join(info.fields["install_if"], " "))
	add("k", info.one("provider_priority"))
	return b.String(), nil
}

func entryFields(s string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if len(l) > 2 && l[1] == ':' {
			m[l[:1]] = l[2:]
		}
	}
	return m
}

func segment(files map[string][]byte, order []string, final bool) ([]byte, error) {
	var out bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	for _, n := range order {
		d := files[n]
		if e := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(d)), ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR, Typeflag: tar.TypeReg}); e != nil {
			return nil, e
		}
		if _, e := tw.Write(d); e != nil {
			return nil, e
		}
	}
	var e error
	if final {
		e = tw.Close()
	} else {
		e = tw.Flush() // signature segments omit the end-of-archive blocks
	}
	if e != nil {
		return nil, e
	}
	if e = gz.Close(); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}

// buildIndex returns a signed APKINDEX.tar.gz: a signature segment followed by
// the index segment; the signature covers the compressed index segment.
func buildIndex(entries []string, description string, key *rsa.PrivateKey, keyname string) ([]byte, error) {
	sort.Strings(entries)
	var idx strings.Builder
	for _, e := range entries {
		idx.WriteString(e + "\n")
	}
	body, e := segment(map[string][]byte{"DESCRIPTION": []byte(description), "APKINDEX": []byte(idx.String())}, []string{"DESCRIPTION", "APKINDEX"}, true)
	if e != nil {
		return nil, e
	}
	sum := sha1.Sum(body) // #nosec G401
	sig, e := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, sum[:])
	if e != nil {
		return nil, e
	}
	name := ".SIGN.RSA." + keyname
	head, e := segment(map[string][]byte{name: sig}, []string{name}, false)
	if e != nil {
		return nil, e
	}
	return append(head, body...), nil
}

func publicPEM(key *rsa.PrivateKey) ([]byte, error) {
	der, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		return nil, e
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
