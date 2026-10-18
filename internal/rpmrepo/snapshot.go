package rpmrepo

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const maxPackages = 10000

type counter struct{ n int64 }

func (c *counter) Write(b []byte) (int, error) { c.n += int64(len(b)); return len(b), nil }

// sink writes one index twice over: raw (open-checksum) and gzip (checksum).
type sink struct {
	name       string
	f          *os.File
	gz         *gzip.Writer
	w          io.Writer
	rawSum     hash.Hash
	zipSum     hash.Hash
	raw, zip   counter
	rawD, zipD string
}

func newSink(name string) (*sink, error) {
	f, e := os.CreateTemp("", "ant-rpm-index-*")
	if e != nil {
		return nil, e
	}
	s := &sink{name: name, f: f, rawSum: sha256.New(), zipSum: sha256.New()}
	// No header mtime or name: identical input yields identical bytes.
	s.gz = gzip.NewWriter(io.MultiWriter(f, s.zipSum, &s.zip))
	s.w = io.MultiWriter(s.gz, s.rawSum, &s.raw)
	return s, nil
}

func (s *sink) close() {
	_ = s.f.Close()
	_ = os.Remove(s.f.Name())
}

func (s *sink) finish() error {
	if e := s.gz.Close(); e != nil {
		return e
	}
	s.rawD, s.zipD = hex.EncodeToString(s.rawSum.Sum(nil)), hex.EncodeToString(s.zipSum.Sum(nil))
	_, e := s.f.Seek(0, 0)
	return e
}

func (s *sink) key() string { return s.zipD + "-" + s.name + ".xml.gz" }

// listRecords reads every package's stored metadata through the artifact
// service (authorization and ownership are re-checked per asset).
func (s *Repository) listRecords(ctx context.Context, p identity.Principal, feed, owner, exclude string) ([]record, error) {
	rows, e := s.db.Query(ctx, `SELECT c.coordinate,v.version FROM components c JOIN versions v ON v.component_id=c.id AND v.instance_id=c.instance_id JOIN assets a ON a.version_id=v.id WHERE c.instance_id=$1 AND c.source_id=$2 AND c.namespace=$3 AND c.coordinate<>$4 AND a.asset_key=$5 ORDER BY c.coordinate,v.version LIMIT $6`, p.InstanceID, owner, namespace, snapshotCoordinate, metadataAsset, maxPackages+1)
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	type key struct{ coordinate, version string }
	var keys []key
	for rows.Next() {
		var k key
		if e = rows.Scan(&k.coordinate, &k.version); e != nil {
			break
		}
		keys = append(keys, k)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil || len(keys) > maxPackages {
		return nil, artifact.ErrUnavailable
	}
	var out []record
	total := 0
	for _, k := range keys {
		pub := artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: k.coordinate, Version: k.version, AssetKey: metadataAsset}
		rec, body, e := s.service.Open(ctx, p, pub, 0, -1)
		if e != nil {
			return nil, e
		}
		if rec.SourceID != owner || rec.Info.Size > maxMetadata {
			body.Close()
			return nil, artifact.ErrUnavailable
		}
		data, e := io.ReadAll(io.LimitReader(contextReader{ctx, body}, maxMetadata+1))
		body.Close()
		var m record
		total += len(data)
		if e != nil || len(data) > maxMetadata || total > 1<<30 || json.Unmarshal(data, &m) != nil || m.Schema != 1 || m.Name != k.coordinate || m.Version+"-"+m.Release+"."+m.Arch != k.version || len(m.SHA256) != 64 || m.Filename == "" {
			return nil, artifact.ErrUnavailable
		}
		if m.Filename == exclude {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// generate builds one coherent generation (repomd + its three indexes and the
// optional signature) and publishes it atomically as a single immutable core
// version. Callers hold the feed lock. exclude drops one file name.
func (s *Repository) generate(ctx context.Context, p identity.Principal, feed, source, exclude string) error {
	records, e := s.listRecords(ctx, p, feed, source, exclude)
	if e != nil {
		return e
	}
	entity, e := s.signer(ctx, p, feed, true)
	if e != nil {
		return e
	}
	fingerprint := "unsigned"
	if entity != nil {
		fingerprint = fmt.Sprintf("%X", entity.PrimaryKey.Fingerprint)
	}
	h := sha256.New()
	for _, r := range records {
		_, _ = io.WriteString(h, r.Filename+"\x00"+r.SHA256+"\n")
	}
	_, _ = io.WriteString(h, fingerprint)
	setHash := hex.EncodeToString(h.Sum(nil))[:16]
	// Newest generation decides both the next sequence and whether the set moved.
	var seq int64
	if latest, e := s.generationWith(ctx, source, "repomd.xml"); e == nil {
		n, suffix, ok := strings.Cut(strings.TrimPrefix(latest, "g"), "-")
		v, perr := strconv.ParseInt(n, 10, 64)
		if !ok || perr != nil {
			return artifact.ErrUnavailable
		}
		if suffix == setHash {
			return nil
		}
		seq = v
	} else if e != artifact.ErrNotFound {
		return e
	}
	version := fmt.Sprintf("g%012d-%s", seq+1, setHash)

	primary, e1 := newSink("primary")
	filelists, e2 := newSink("filelists")
	other, e3 := newSink("other")
	defer func() {
		for _, k := range []*sink{primary, filelists, other} {
			if k != nil {
				k.close()
			}
		}
	}()
	if e1 != nil || e2 != nil || e3 != nil {
		return artifact.ErrUnavailable
	}
	n := strconv.Itoa(len(records))
	header := `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
	for _, part := range []struct {
		k    *sink
		root string
		ns   string
	}{
		{primary, "metadata", ` xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm"`},
		{filelists, "filelists", ` xmlns="http://linux.duke.edu/metadata/filelists"`},
		{other, "otherdata", ` xmlns="http://linux.duke.edu/metadata/other"`},
	} {
		_, _ = io.WriteString(part.k.w, header+"<"+part.root+part.ns+` packages="`+n+`">`+"\n")
	}
	for _, r := range records {
		_, _ = io.WriteString(primary.w, r.Primary)
		_, _ = io.WriteString(filelists.w, r.Filelists)
		_, _ = io.WriteString(other.w, r.Other)
	}
	for _, part := range []struct {
		k    *sink
		root string
	}{{primary, "metadata"}, {filelists, "filelists"}, {other, "otherdata"}} {
		_, _ = io.WriteString(part.k.w, "</"+part.root+">\n")
		if e = part.k.finish(); e != nil {
			return artifact.ErrUnavailable
		}
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	var repomd strings.Builder
	repomd.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">` + "\n  <revision>" + now + "</revision>\n")
	for _, k := range []*sink{primary, filelists, other} {
		repomd.WriteString(`  <data type="` + k.name + `">` + "\n")
		repomd.WriteString(`    <checksum type="sha256">` + k.zipD + "</checksum>\n    <open-checksum type=\"sha256\">" + k.rawD + "</open-checksum>\n")
		repomd.WriteString(`    <location href="repodata/` + k.key() + `"/>` + "\n    <timestamp>" + now + "</timestamp>\n")
		repomd.WriteString("    <size>" + strconv.FormatInt(k.zip.n, 10) + "</size>\n    <open-size>" + strconv.FormatInt(k.raw.n, 10) + "</open-size>\n  </data>\n")
	}
	repomd.WriteString("</repomd>\n")
	repomdBytes := []byte(repomd.String())

	pubs := make([]artifact.Publication, 0, 5)
	bodies := make([]io.Reader, 0, 5)
	add := func(key, media string, body []byte) {
		sum := sha256.Sum256(body)
		pubs = append(pubs, artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: snapshotCoordinate, Version: version, AssetKey: key, MediaType: media, ExpectedSource: source, Info: blob.Info{Size: int64(len(body)), Digest: blob.Digest(hex.EncodeToString(sum[:]))}})
		bodies = append(bodies, bytes.NewReader(body))
	}
	add("repomd.xml", "application/xml", repomdBytes)
	if entity != nil {
		sig, e := detachedSignature(entity, repomdBytes)
		if e != nil {
			return artifact.ErrUnavailable
		}
		add("repomd.xml.asc", "application/pgp-signature", sig)
	}
	for _, k := range []*sink{primary, filelists, other} {
		pubs = append(pubs, artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: snapshotCoordinate, Version: version, AssetKey: k.key(), MediaType: "application/gzip", ExpectedSource: source, Info: blob.Info{Size: k.zip.n, Digest: blob.Digest(k.zipD)}})
		bodies = append(bodies, k.f)
	}
	_, e = s.service.PublishBundle(ctx, p, pubs, bodies)
	return e
}
