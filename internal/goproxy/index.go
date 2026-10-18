package goproxy

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// latest follows GOPROXY preference: highest release, then highest prerelease,
// then the chronologically latest pseudo-version. Retractions remain in the
// immutable go.mod; the Go client applies them during its version queries.
func latest(versions []string) string {
	release, pre, pseudo := "", "", ""
	var newest time.Time
	for _, v := range versions {
		if module.IsPseudoVersion(v) {
			stamp, err := module.PseudoVersionTime(v)
			if err == nil && (pseudo == "" || stamp.After(newest) || (stamp.Equal(newest) && semver.Compare(v, pseudo) > 0)) {
				pseudo = v
				newest = stamp
			}
		} else if semver.Prerelease(v) == "" {
			if release == "" || semver.Compare(v, release) > 0 {
				release = v
			}
		} else if pre == "" || semver.Compare(v, pre) > 0 {
			pre = v
		}
	}
	if release != "" {
		return release
	}
	if pre != "" {
		return pre
	}
	return pseudo
}
func (s *Proxy) index(w http.ResponseWriter, r *http.Request, p identity.Principal, target route, feed string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	policy, err := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if err != nil {
		fail(w, err)
		return
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		fail(w, err)
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	}()
	var component, source string
	err = tx.QueryRow(ctx, `SELECT c.id,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1`, p.InstanceID, feed, target.module).Scan(&component, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		err = artifact.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	pub := target.publication(feed, "info")
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Namespace: pub.Namespace, Kind: identity.ArtifactResource}, identity.Read) {
		fail(w, artifact.ErrNotFound)
		return
	}
	// Defensively exclude incomplete historical records. All writes by this
	// adapter use PublishBundle and expose these three assets in one transaction.
	rows, err := tx.Query(ctx, `SELECT v.version FROM versions v JOIN assets a ON a.version_id=v.id WHERE v.component_id=$1 AND a.asset_key IN ('module.info','module.mod','module.zip') GROUP BY v.id,v.version HAVING count(DISTINCT a.asset_key)=3 ORDER BY v.version LIMIT 10001`, component)
	if err != nil {
		fail(w, err)
		return
	}
	versions := []string{}
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			break
		}
		if module.CanonicalVersion(v) == v && module.Check(target.module, v) == nil {
			versions = append(versions, v)
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil || len(versions) > 10000 {
		fail(w, artifact.ErrUnavailable)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		fail(w, err)
		return
	}
	if target.kind == "latest" {
		pub.Version = latest(versions)
		if pub.Version == "" {
			fail(w, artifact.ErrNotFound)
			return
		}
		s.download(w, r, p, pub, source)
		return
	}
	sort.Slice(versions, func(i, j int) bool { return semver.Compare(versions[i], versions[j]) < 0 })
	var body strings.Builder
	for _, v := range versions {
		if !module.IsPseudoVersion(v) {
			body.WriteString(v)
			body.WriteByte('\n')
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(200)
	if r.Method != "HEAD" {
		_, _ = w.Write([]byte(body.String()))
	}
}
