package artifact

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReplicaFeedError reports that a feed is a read-only replica (a replication
// job with destination_read_only targets it) and the publish was not routed to
// a separate local source by an explicit publish rule. It satisfies
// errors.Is(err, ErrConflict), so every adapter answers 409; adapters that can
// carry a message use errors.As to name the peer to publish to.
type ReplicaFeedError struct{ Peer string }

func (e *ReplicaFeedError) Error() string { return "replica feed — publish to " + e.Peer }
func (*ReplicaFeedError) Is(target error) bool {
	return target == ErrConflict
}

// ConflictMessage returns the replica guidance when err is a ReplicaFeedError,
// otherwise fallback.
func ConflictMessage(err error, fallback string) string {
	var replica *ReplicaFeedError
	if errors.As(err, &replica) {
		return replica.Error()
	}
	return fallback
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// readOnlyReplicaPeer returns the peer URL when the feed is the destination of a
// read-only replication job.
func readOnlyReplicaPeer(ctx context.Context, q rowQuerier, instance, feed string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var url string
	err := q.QueryRow(ctx, `SELECT p.url FROM replication_jobs j JOIN replication_peers p ON p.instance_id=j.instance_id AND p.id=j.peer_id
 WHERE j.instance_id=$1 AND j.dest_feed_id=$2 AND j.destination_read_only ORDER BY j.name LIMIT 1`, instance, feed).Scan(&url)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return url, true, nil
}

// FeedTransferringError reports that the feed is frozen by a federation home
// transfer (planned freeze, or an old home demoting after the home moved).
// Publishes and deletes are refused until the transfer completes or aborts.
// It satisfies errors.Is(err, ErrUnavailable), so adapters answer 503.
type FeedTransferringError struct{}

func (*FeedTransferringError) Error() string {
	return "feed_transferring: the feed is moving to another federation member; retry against the new home"
}
func (*FeedTransferringError) Is(target error) bool { return target == ErrUnavailable }

// frozenFeed reports a federation transfer freeze on the feed.
func frozenFeed(ctx context.Context, q rowQuerier, instance, feed string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var frozen bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM federation_feed_freezes WHERE instance_id=$1 AND feed_id=$2)`, instance, feed).Scan(&frozen); err != nil {
		return ErrUnavailable
	}
	if frozen {
		return &FeedTransferringError{}
	}
	return nil
}
