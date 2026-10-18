package replication

import (
	"context"
	"errors"
	"time"
)

// DeletionEntry is one tombstone and what it would do at the destination.
type DeletionEntry struct {
	Source      string `json:"source"` // "held" (received earlier) or "pending" (not yet applied)
	Kind        string `json:"kind"`
	Coordinate  string `json:"coordinate"`
	Version     string `json:"version,omitempty"`
	Key         string `json:"key,omitempty"`
	Seq         int64  `json:"seq"`
	Disposition string `json:"disposition"` // would_delete, protected (destination-only), absent
}

// DeletionPreview is a read-only dry run. It never changes destination state,
// the checkpoint or held tombstones. Applies is false while the job policy is
// "ignore": the entries then show what switching to "propagate" would do.
type DeletionPreview struct {
	Policy    string          `json:"policy"`
	Applies   bool            `json:"applies"`
	Entries   []DeletionEntry `json:"entries"`
	Counts    map[string]int  `json:"counts"`
	Truncated bool            `json:"truncated"`
	// ReseedRequired is set when the peer purged the history the job needs, so
	// tombstones between the checkpoint and the head cannot be listed.
	ReseedRequired bool `json:"reseed_required,omitempty"`
}

const previewLimit = 1000

// PreviewDeletions lists held tombstones and tombstones not yet applied
// (between the checkpoint and the peer's head), classifying each against current
// destination content. Destination-only content is reported as protected.
func (s *Service) PreviewDeletions(ctx context.Context, instance, id string) (DeletionPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	jr, err := scanJob(s.cfg.DB.QueryRow(ctx, `SELECT `+jobColumns+` FROM replication_jobs j WHERE j.instance_id=$1 AND j.id=$2`, instance, id))
	if err != nil {
		return DeletionPreview{}, dbError(err)
	}
	rt := &run{job: jr}
	out := DeletionPreview{Policy: jr.DeletionPolicy, Applies: jr.DeletionPolicy == "propagate", Entries: []DeletionEntry{}, Counts: map[string]int{}}
	add := func(e DeletionEntry) bool {
		if len(out.Entries) >= previewLimit {
			out.Truncated = true
			return false
		}
		out.Entries = append(out.Entries, e)
		out.Counts[e.Disposition]++
		return true
	}
	rows, err := s.cfg.DB.Query(ctx, `SELECT kind,coordinate,version,item_key,seq FROM replication_held_deletions WHERE job_id=$1 ORDER BY seq LIMIT $2`, id, previewLimit+1)
	if err != nil {
		return DeletionPreview{}, ErrUnavailable
	}
	var held []DeletionEntry
	for rows.Next() {
		e := DeletionEntry{Source: "held"}
		if err = rows.Scan(&e.Kind, &e.Coordinate, &e.Version, &e.Key, &e.Seq); err != nil {
			rows.Close()
			return DeletionPreview{}, ErrUnavailable
		}
		held = append(held, e)
	}
	rows.Close()
	for _, e := range held {
		if e.Disposition, err = s.classify(ctx, s.cfg.DB, rt, e.Kind, e.Coordinate, e.Version, e.Key); err != nil {
			return DeletionPreview{}, ErrUnavailable
		}
		if !add(e) {
			return out, nil
		}
	}
	if jr.Status.Phase != "incremental" {
		return out, nil
	}
	client, err := s.peerFor(ctx, jr.PeerID)
	if err != nil {
		return DeletionPreview{}, err
	}
	after := jr.Status.Checkpoint
	for {
		page, err := client.Changes(ctx, jr.RemoteFeedID, after, s.cfg.PageSize, jr.IncludeCached, jr.Filters)
		if errors.Is(err, ErrHistoryPurged) {
			// Pending tombstones are gone; the next run reseeds instead.
			out.ReseedRequired = true
			return out, nil
		}
		if err != nil {
			return DeletionPreview{}, err
		}
		for _, c := range page.Changes {
			if c.Op != "delete" {
				continue
			}
			e := DeletionEntry{Source: "pending", Kind: c.Kind, Coordinate: c.Coordinate, Version: heldKey(c.Kind, c.Version), Key: c.Key, Seq: c.Seq}
			if e.Disposition, err = s.classify(ctx, s.cfg.DB, rt, e.Kind, e.Coordinate, e.Version, e.Key); err != nil {
				return DeletionPreview{}, ErrUnavailable
			}
			if !add(e) {
				return out, nil
			}
		}
		if !page.More {
			return out, nil
		}
		after = page.Next
	}
}
