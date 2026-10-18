package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CompareItem names one coordinate: a path inside a source-manager repository.
// Feed is the Ant feed to compare against; it defaults to the repository's slug.
type CompareItem struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Feed       string `json:"feed,omitempty"`
}

// Side is what a client would receive from one system.
type Side struct {
	Found  bool   `json:"found"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// Detail names the Ant source that would answer, or the reason for a miss.
	Detail string `json:"detail,omitempty"`
}

// CompareResult is the staged-adoption comparison for one coordinate.
// Status is identical, differs, missing_in_ant, missing_in_source, missing_in_both or error.
type CompareResult struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Feed       string `json:"feed"`
	Status     string `json:"status"`
	Source     Side   `json:"source"`
	Ant        Side   `json:"ant"`
}

// CompareReport lists results and a status histogram.
type CompareReport struct {
	Manager string          `json:"manager"`
	Counts  map[string]int  `json:"counts"`
	Results []CompareResult `json:"results"`
}

const (
	maxCompareItems = 500
	maxCompareBytes = 1 << 30
)

// Compare reports, for each coordinate, the content a client would get from the
// source manager and from the Ant feed (first source in feed order holding the
// path). It reads only; the source file is streamed through SHA-256.
func Compare(ctx context.Context, pool *pgxpool.Pool, instance string, m Manager, items []CompareItem) (*CompareReport, error) {
	if len(items) == 0 || len(items) > maxCompareItems {
		return nil, fmt.Errorf("%w: between 1 and %d coordinates are required", ErrInput, maxCompareItems)
	}
	rep := &CompareReport{Manager: m.Name(), Counts: map[string]int{}, Results: make([]CompareResult, 0, len(items))}
	for _, it := range items {
		if it.Repository == "" || it.Path == "" {
			return nil, fmt.Errorf("%w: repository and path are required", ErrInput)
		}
		feed := it.Feed
		if feed == "" {
			feed = Slug(it.Repository)
		}
		r := CompareResult{Repository: it.Repository, Path: it.Path, Feed: feed}
		var err error
		r.Source, err = sourceSide(ctx, m, it)
		if err != nil {
			r.Status = "error"
			r.Source.Detail = "source manager: " + err.Error()
		} else if r.Ant, err = antSide(ctx, pool, instance, feed, it.Path); err != nil {
			r.Status = "error"
			r.Ant.Detail = err.Error()
		} else {
			switch {
			case r.Source.Found && r.Ant.Found && r.Source.SHA256 == r.Ant.SHA256:
				r.Status = "identical"
			case r.Source.Found && r.Ant.Found:
				r.Status = "differs"
			case r.Source.Found:
				r.Status = "missing_in_ant"
			case r.Ant.Found:
				r.Status = "missing_in_source"
			default:
				r.Status = "missing_in_both"
			}
		}
		rep.Counts[r.Status]++
		rep.Results = append(rep.Results, r)
	}
	return rep, nil
}

func sourceSide(ctx context.Context, m Manager, it CompareItem) (Side, error) {
	rc, err := m.Open(ctx, SourceAsset{Repository: it.Repository, Path: it.Path, Size: -1})
	if errors.Is(err, ErrNotFound) {
		return Side{Detail: "not found"}, nil
	}
	if err != nil {
		return Side{}, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(ctxReader{ctx, rc}, maxCompareBytes+1))
	if err != nil {
		return Side{}, err
	}
	if n > maxCompareBytes {
		return Side{}, fmt.Errorf("asset exceeds the %d byte comparison limit", int64(maxCompareBytes))
	}
	return Side{Found: true, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

func antSide(ctx context.Context, pool *pgxpool.Pool, instance, feed, path string) (Side, error) {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND slug=$2)`, instance, feed).Scan(&exists); err != nil {
		return Side{}, err
	}
	if !exists {
		return Side{Detail: "feed " + feed + " does not exist"}, nil
	}
	var s Side
	var name string
	err := pool.QueryRow(ctx, `SELECT s.name,b.digest,b.size_bytes FROM feeds f
 JOIN feed_sources fs ON fs.feed_id=f.id JOIN sources s ON s.id=fs.source_id
 JOIN components c ON c.source_id=s.id JOIN versions v ON v.component_id=c.id
 JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id
 WHERE f.instance_id=$1 AND f.slug=$2 AND a.asset_key=$3 ORDER BY fs.priority,a.created_at DESC LIMIT 1`, instance, feed, path).Scan(&name, &s.SHA256, &s.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return Side{Detail: "not stored in any source of the feed (a remote source would fetch it on demand)"}, nil
	}
	if err != nil {
		return Side{}, err
	}
	s.Found, s.Detail = true, "source "+name
	return s, nil
}
