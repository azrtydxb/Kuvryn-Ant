package p2repo

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5" // p2 download.md5 property
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/ulikunitz/xz"
)

const (
	maxUpload   = 1 << 30
	maxXML      = 128 << 20
	maxEntries  = 20000
	maxManifest = 256 << 20
)

var errInvalid = errors.New("invalid p2 repository")
var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}$`)
var verRE = regexp.MustCompile(`^[0-9][A-Za-z0-9._-]{0,127}$`)

func validID(s string) bool      { return idRE.MatchString(s) }
func validVersion(s string) bool { return verRE.MatchString(s) }

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	return r.r.Read(b)
}
func info(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Size: int64(len(b)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}

type unit struct {
	ID, Version string
	XML         []byte
	Refs        [][3]string // classifier,id,version
}
type artDesc struct {
	Classifier, ID, Version string
	XML                     []byte
	SHA256, MD5             string
	Size                    int64
}

// fragments returns the raw bytes of each child element of root>section,
// preserving the original serialization.
func fragments(data []byte, root, section, child string) ([][]byte, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	var out [][]byte
	depth := 0
	in := false
	var start int64
	for {
		off := d.InputOffset()
		tok, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, errInvalid
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 && t.Name.Local != root {
				return nil, errInvalid
			}
			if depth == 2 {
				in = t.Name.Local == section
			}
			if depth == 3 && in {
				if t.Name.Local != child {
					return nil, errInvalid
				}
				start = off
			}
		case xml.EndElement:
			if depth == 3 && in {
				out = append(out, data[start:d.InputOffset()])
			}
			depth--
		}
	}
	if depth != 0 || len(out) > maxEntries*4 {
		return nil, errInvalid
	}
	return out, nil
}

func parseUnits(data []byte) ([]unit, error) {
	frags, e := fragments(data, "repository", "units", "unit")
	if e != nil {
		return nil, e
	}
	var out []unit
	for _, f := range frags {
		var x struct {
			ID        string `xml:"id,attr"`
			Version   string `xml:"version,attr"`
			Artifacts []struct {
				Classifier string `xml:"classifier,attr"`
				ID         string `xml:"id,attr"`
				Version    string `xml:"version,attr"`
			} `xml:"artifacts>artifact"`
		}
		if xml.Unmarshal(f, &x) != nil || !validID(x.ID) || !validVersion(x.Version) {
			return nil, errInvalid
		}
		u := unit{ID: x.ID, Version: x.Version, XML: f}
		for _, a := range x.Artifacts {
			u.Refs = append(u.Refs, [3]string{a.Classifier, a.ID, a.Version})
		}
		out = append(out, u)
	}
	return out, nil
}

func parseArtifacts(data []byte) ([]artDesc, error) {
	frags, e := fragments(data, "repository", "artifacts", "artifact")
	if e != nil {
		return nil, e
	}
	var out []artDesc
	for _, f := range frags {
		var x struct {
			Classifier string `xml:"classifier,attr"`
			ID         string `xml:"id,attr"`
			Version    string `xml:"version,attr"`
			Properties []struct {
				Name  string `xml:"name,attr"`
				Value string `xml:"value,attr"`
			} `xml:"properties>property"`
		}
		if xml.Unmarshal(f, &x) != nil || dirFor(x.Classifier) == "" || !validID(x.ID) || !validVersion(x.Version) {
			return nil, errInvalid
		}
		d := artDesc{Classifier: x.Classifier, ID: x.ID, Version: x.Version, XML: f, Size: -1}
		for _, p := range x.Properties {
			switch p.Name {
			case "format": // packed/Pack200 transforms are a non-goal
				return nil, errInvalid
			case "download.checksum.sha-256":
				d.SHA256 = strings.ToLower(p.Value)
			case "download.md5":
				d.MD5 = strings.ToLower(p.Value)
			case "download.size":
				n, e := strconv.ParseInt(p.Value, 10, 64)
				if e != nil || n < 0 {
					return nil, errInvalid
				}
				d.Size = n
			}
		}
		if d.SHA256 == "" && d.MD5 == "" {
			return nil, errInvalid
		}
		out = append(out, d)
	}
	return out, nil
}

func dirFor(classifier string) string {
	for d, c := range dirClassifier {
		if c == classifier {
			return d
		}
	}
	return ""
}
func entryPath(classifier, id, version string) string {
	if classifier == "binary" {
		return "binary/" + id + "_" + version
	}
	return dirFor(classifier) + "/" + id + "_" + version + ".jar"
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, errInvalid
	}
	return b, nil
}

// zipIndex maps clean names to entries after rejecting unsafe or duplicate paths.
func zipIndex(z *zip.Reader) (map[string]*zip.File, error) {
	if len(z.File) > maxEntries {
		return nil, errInvalid
	}
	m := map[string]*zip.File{}
	var total uint64
	for _, f := range z.File {
		n := strings.TrimSuffix(f.Name, "/")
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() || n == "" || strings.HasPrefix(n, "/") || strings.ContainsAny(n, "\\:\x00") || m[n] != nil {
			return nil, errInvalid
		}
		for _, part := range strings.Split(n, "/") {
			if part == "" || part == "." || part == ".." {
				return nil, errInvalid
			}
		}
		if total += f.UncompressedSize64; total > 4<<30 {
			return nil, errInvalid
		}
		m[n] = f
	}
	return m, nil
}

func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, e := f.Open()
	if e != nil {
		return nil, errInvalid
	}
	defer rc.Close()
	return readLimited(rc, limit)
}

// metadataXML locates base.xml, base.jar or base.xml.xz in the upload.
func metadataXML(idx map[string]*zip.File, base string) ([]byte, error) {
	found := 0
	var out []byte
	var err error
	if f := idx[base+".xml"]; f != nil {
		found++
		out, err = readEntry(f, maxXML)
	}
	if f := idx[base+".xml.xz"]; f != nil && err == nil {
		found++
		var rc []byte
		if rc, err = readEntry(f, maxXML); err == nil {
			var xr *xz.Reader
			if xr, err = xz.NewReader(bytes.NewReader(rc)); err == nil {
				out, err = readLimited(xr, maxXML)
			}
		}
	}
	if f := idx[base+".jar"]; f != nil && err == nil {
		found++
		var jb []byte
		if jb, err = readEntry(f, maxXML); err == nil {
			var jz *zip.Reader
			if jz, err = zip.NewReader(bytes.NewReader(jb), int64(len(jb))); err == nil {
				var ji map[string]*zip.File
				if ji, err = zipIndex(jz); err == nil && ji[base+".xml"] != nil {
					out, err = readEntry(ji[base+".xml"], maxXML)
				} else {
					err = errInvalid
				}
			}
		}
	}
	if found == 0 || err != nil {
		return nil, errInvalid
	}
	return out, nil
}

func hashes(b []byte) (string, string) {
	s := sha256.Sum256(b)
	m := md5.Sum(b)
	return hex.EncodeToString(s[:]), hex.EncodeToString(m[:])
}
