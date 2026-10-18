package artifact

import (
	"context"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

// Prepare authorizes before adapters accept upload bytes. Sessions pin its source
// and pass ExpectedSource on final publication; expiry or policy changes fail closed.
func (s *Service) Prepare(ctx context.Context, p identity.Principal, feed, namespace, coordinate, assetKey string) (string, error) {
	if s == nil || s.DB == nil || s.Router == nil || s.Lease == nil || p.PrincipalID == "" || !bounded(feed, 36) || !bounded(coordinate, 256) || len(namespace) > 256 {
		return "", ErrInput
	}
	if path, err := routing.NormalizePath("/" + assetKey); err != nil || path != "/"+assetKey {
		return "", ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	router, err := s.Router(ctx, feed)
	if err != nil || router == nil {
		return "", ErrUnavailable
	}
	decision, err := router.Resolve(routing.Request{Namespace: namespace, Package: coordinate, Path: "/" + assetKey, Principal: p.PrincipalID})
	if err != nil {
		return "", ErrInput
	}
	if !decision.Allowed {
		return "", ErrDenied
	}
	r := Publication{FeedID: feed, Namespace: namespace}
	if err = s.authorize(ctx, p, r, decision.TargetID); err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", ErrUnavailable
	}
	defer rollback(tx)
	lease := s.Lease()
	if lease.InstanceID != p.InstanceID {
		return "", ErrDenied
	}
	if err = ownership.Require(ctx, tx, lease); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT s.id::text FROM feed_sources fs JOIN sources s ON s.id=fs.source_id JOIN feeds f ON f.id=fs.feed_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.id=$3 AND s.instance_id=$1 AND s.kind='local' AND s.format=f.format`, p.InstanceID, feed, decision.TargetID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", ErrUnavailable
	}
	if tx.Commit(ctx) != nil {
		return "", ErrUnavailable
	}
	return id, nil
}
