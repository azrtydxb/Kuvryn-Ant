package replication

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config wires the destination side. Lease must return the serving instance's
// current writer lease: every metadata application is fenced by it.
type Config struct {
	DB         *pgxpool.Pool
	Keys       *secrets.Keyring
	InstanceID string
	Lease      func() ownership.Lease
	Storage    func(context.Context, string) (blob.Store, error)
	// CAs maps operator-defined CA references to mounted PEM bundles.
	CAs map[string]string

	PageSize        int           // items per manifest/change page (default 200)
	BlobConcurrency int           // parallel blob transfers per page (default 4)
	PollInterval    time.Duration // scheduler tick (default 2s)
	ContinuousDelay time.Duration // pause between continuous passes (default 2s)
	LeaseTTL        time.Duration // per-job claim lease (default 60s)
	MaxBackoff      time.Duration // retry backoff cap (default 5m)
}

// Service owns peers, jobs, the scheduler and the apply path.
type Service struct {
	cfg   Config
	owner string
}

func New(cfg Config) (*Service, error) {
	if cfg.DB == nil || cfg.Keys == nil || cfg.InstanceID == "" || cfg.Lease == nil || cfg.Storage == nil {
		return nil, ErrInput
	}
	if cfg.PageSize <= 0 || cfg.PageSize > maxPage {
		cfg.PageSize = defaultPage
	}
	if cfg.BlobConcurrency <= 0 {
		cfg.BlobConcurrency = 4
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.ContinuousDelay <= 0 {
		cfg.ContinuousDelay = 2 * time.Second
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 60 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var owner string
	if err := cfg.DB.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&owner); err != nil {
		return nil, ErrUnavailable
	}
	return &Service{cfg: cfg, owner: owner}, nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func dbError(err error) error {
	var pg *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case errors.As(err, &pg):
		switch pg.Code {
		case "23505", "23503":
			return ErrConflict
		case "22P02", "23514", "22001":
			return ErrInput
		}
	}
	if errors.Is(err, ErrInput) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, secrets.ErrRevisionConflict) {
		return err
	}
	return ErrUnavailable
}

func audit(ctx context.Context, tx pgx.Tx, instance, principal, action, resource string, details any) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	var p any
	if principal != "" {
		p = principal
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,$3,$4,$5)`, instance, p, action, resource, encoded)
	return err
}

// Peer is a configured remote instance. The token is never returned.
type Peer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	CARef     string    `json:"ca_ref,omitempty"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type PeerInput struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	CARef string `json:"ca_ref"`
	Token string `json:"token"`
}

func (s *Service) validCARef(ref string) bool {
	if ref == "" {
		return true
	}
	_, ok := s.cfg.CAs[ref]
	return ok
}

const peerColumns = `id::text,name,url,COALESCE(ca_ref,''),revision,created_at,updated_at`

func scanPeer(row pgx.Row) (Peer, error) {
	var p Peer
	err := row.Scan(&p.ID, &p.Name, &p.URL, &p.CARef, &p.Revision, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (s *Service) CreatePeer(ctx context.Context, instance, actor string, in PeerInput) (Peer, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u, err := ValidatePeerURL(in.URL)
	if err != nil || in.Name == "" || len(in.Token) < 16 || len(in.Token) > 4096 || !s.validCARef(in.CARef) {
		return Peer{}, ErrInput
	}
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Peer{}, ErrUnavailable
	}
	defer rollback(tx)
	var id string
	if err = tx.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		return Peer{}, ErrUnavailable
	}
	token := []byte(in.Token)
	defer clear(token)
	rec, err := (secrets.Store{Keys: s.cfg.Keys}).CreateTx(ctx, tx, instance, actor, "replication-peer:"+id, token)
	if err != nil {
		return Peer{}, ErrUnavailable
	}
	var ca any
	if in.CARef != "" {
		ca = in.CARef
	}
	peer, err := scanPeer(tx.QueryRow(ctx, `INSERT INTO replication_peers(id,instance_id,name,url,ca_ref,token_secret_id) VALUES($1,$2,$3,$4,$5,$6) RETURNING `+peerColumns, id, instance, in.Name, u, ca, rec.ID))
	if err != nil {
		return Peer{}, dbError(err)
	}
	if err = audit(ctx, tx, instance, actor, "replication.peer.create", peer.ID, map[string]any{"name": peer.Name}); err != nil {
		return Peer{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return Peer{}, ErrUnavailable
	}
	return peer, nil
}

func (s *Service) ListPeers(ctx context.Context, instance string) ([]Peer, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.cfg.DB.Query(ctx, `SELECT `+peerColumns+` FROM replication_peers WHERE instance_id=$1 ORDER BY name LIMIT 1000`, instance)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	out := []Peer{}
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, ErrUnavailable
		}
		out = append(out, p)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return out, nil
}

func (s *Service) GetPeer(ctx context.Context, instance, id string) (Peer, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := scanPeer(s.cfg.DB.QueryRow(ctx, `SELECT `+peerColumns+` FROM replication_peers WHERE instance_id=$1 AND id=$2`, instance, id))
	return p, dbError(err)
}

// PeerPatch updates a peer at an exact revision. A non-empty Token rotates the
// stored credential.
type PeerPatch struct {
	Revision int64   `json:"revision"`
	URL      *string `json:"url"`
	CARef    *string `json:"ca_ref"`
	Token    *string `json:"token"`
}

func (s *Service) UpdatePeer(ctx context.Context, instance, actor, id string, in PeerPatch) (Peer, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if in.Revision < 1 {
		return Peer{}, ErrInput
	}
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Peer{}, ErrUnavailable
	}
	defer rollback(tx)
	var rev int64
	var secretID, url string
	var ca *string
	err = tx.QueryRow(ctx, `SELECT revision,token_secret_id::text,url,ca_ref FROM replication_peers WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, id).Scan(&rev, &secretID, &url, &ca)
	if err != nil {
		return Peer{}, dbError(err)
	}
	if rev != in.Revision {
		return Peer{}, ErrConflict
	}
	if in.URL != nil {
		if url, err = ValidatePeerURL(*in.URL); err != nil {
			return Peer{}, ErrInput
		}
	}
	if in.CARef != nil {
		if !s.validCARef(*in.CARef) {
			return Peer{}, ErrInput
		}
		ca = nil
		if *in.CARef != "" {
			ca = in.CARef
		}
	}
	if in.Token != nil {
		if len(*in.Token) < 16 || len(*in.Token) > 4096 {
			return Peer{}, ErrInput
		}
		var secretRevision int64
		if err = tx.QueryRow(ctx, `SELECT revision FROM encrypted_secrets WHERE instance_id=$1 AND id=$2`, instance, secretID).Scan(&secretRevision); err != nil {
			return Peer{}, dbError(err)
		}
		token := []byte(*in.Token)
		defer clear(token)
		if _, err = (secrets.Store{Keys: s.cfg.Keys}).UpdateTx(ctx, tx, instance, actor, secretID, "replication-peer:"+id, secretRevision, token); err != nil {
			return Peer{}, dbError(err)
		}
	}
	peer, err := scanPeer(tx.QueryRow(ctx, `UPDATE replication_peers SET url=$3,ca_ref=$4,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id=$2 RETURNING `+peerColumns, instance, id, url, ca))
	if err != nil {
		return Peer{}, dbError(err)
	}
	if err = audit(ctx, tx, instance, actor, "replication.peer.update", id, map[string]any{"token_rotated": in.Token != nil}); err != nil {
		return Peer{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return Peer{}, ErrUnavailable
	}
	return peer, nil
}

func (s *Service) DeletePeer(ctx context.Context, instance, actor, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ErrUnavailable
	}
	defer rollback(tx)
	var secretID string
	err = tx.QueryRow(ctx, `DELETE FROM replication_peers WHERE instance_id=$1 AND id=$2 RETURNING token_secret_id::text`, instance, id).Scan(&secretID)
	if err != nil {
		return dbError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id=$2`, instance, secretID); err != nil {
		return ErrUnavailable
	}
	if err = audit(ctx, tx, instance, actor, "replication.peer.delete", id, map[string]any{}); err != nil {
		return ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Status is the queryable progress, lag, failure and last-success report.
type Status struct {
	Phase               string     `json:"phase"` // new, seed_assets, seed_tags, incremental
	Running             bool       `json:"running"`
	Checkpoint          int64      `json:"checkpoint"`
	RemoteHead          *int64     `json:"remote_head,omitempty"`
	ChangesBehind       *int64     `json:"changes_behind,omitempty"`
	LagSeconds          *float64   `json:"lag_seconds,omitempty"`
	RemoteCheckedAt     *time.Time `json:"remote_checked_at,omitempty"`
	SeedTotal           *int64     `json:"seed_total,omitempty"`
	SeedDone            int64      `json:"seed_done"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	NextRunAt           *time.Time `json:"next_run_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	AssetsApplied       int64      `json:"assets_applied"`
	BlobsTransferred    int64      `json:"blobs_transferred"`
	BlobsDeduplicated   int64      `json:"blobs_deduplicated"`
	BytesTransferred    int64      `json:"bytes_transferred"`
	DeletionsApplied    int64      `json:"deletions_applied"`
	DeletionsHeld       int64      `json:"deletions_held"`
	Conflicts           int64      `json:"conflicts"`
}

// Job is a replication job for one destination feed.
type Job struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	PeerID          string    `json:"peer_id"`
	RemoteFeedID    string    `json:"remote_feed_id"`
	DestFeedID      string    `json:"dest_feed_id"`
	ReplicaSourceID string    `json:"replica_source_id"`
	Mode            string    `json:"mode"`
	IntervalSeconds int       `json:"interval_seconds"`
	IncludeCached   bool      `json:"include_cached"`
	Filters         Filters   `json:"filters"`
	DeletionPolicy  string    `json:"deletion_policy"`
	Enabled         bool      `json:"enabled"`
	Revision        int64     `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	Status          Status    `json:"status"`
}

// jobRow carries internal-only fields in addition to the public job.
type jobRow struct {
	Job
	seedHead           *int64
	seedCursor         string
	checkpointChangeAt *time.Time
	remoteHeadAt       *time.Time
	runRequested       bool
	leaseOwner         *string
	leaseUntil         *time.Time
	held               int64
}

const jobColumns = `j.id::text,j.name,j.peer_id::text,j.remote_feed_id::text,j.dest_feed_id::text,j.replica_source_id::text,j.mode,j.interval_seconds,j.include_cached,j.filters,j.deletion_policy,j.enabled,j.revision,j.created_at,
 j.phase,j.seed_head,j.seed_cursor,j.seed_total,j.seed_done,j.checkpoint,j.checkpoint_change_at,j.remote_head,j.remote_head_at,j.remote_checked_at,j.next_run_at,j.run_requested,j.lease_owner::text,j.lease_until,
 j.last_attempt_at,j.last_success_at,COALESCE(j.last_error,''),j.consecutive_failures,j.assets_applied,j.blobs_transferred,j.blobs_deduplicated,j.bytes_transferred,j.deletions_applied,j.conflicts,
 (SELECT count(*) FROM replication_held_deletions h WHERE h.job_id=j.id),clock_timestamp()`

func scanJob(row pgx.Row) (jobRow, error) {
	var j jobRow
	var filters []byte
	var now time.Time
	st := &j.Status
	err := row.Scan(&j.ID, &j.Name, &j.PeerID, &j.RemoteFeedID, &j.DestFeedID, &j.ReplicaSourceID, &j.Mode, &j.IntervalSeconds, &j.IncludeCached, &filters, &j.DeletionPolicy, &j.Enabled, &j.Revision, &j.CreatedAt,
		&st.Phase, &j.seedHead, &j.seedCursor, &st.SeedTotal, &st.SeedDone, &st.Checkpoint, &j.checkpointChangeAt, &st.RemoteHead, &j.remoteHeadAt, &st.RemoteCheckedAt, &st.NextRunAt, &j.runRequested, &j.leaseOwner, &j.leaseUntil,
		&st.LastAttemptAt, &st.LastSuccessAt, &st.LastError, &st.ConsecutiveFailures, &st.AssetsApplied, &st.BlobsTransferred, &st.BlobsDeduplicated, &st.BytesTransferred, &st.DeletionsApplied, &st.Conflicts,
		&j.held, &now)
	if err != nil {
		return jobRow{}, err
	}
	_ = json.Unmarshal(filters, &j.Filters)
	st.DeletionsHeld = j.held
	st.Running = j.leaseUntil != nil && j.leaseUntil.After(now)
	if st.RemoteHead != nil {
		behind := *st.RemoteHead - st.Checkpoint
		if st.Phase != "incremental" {
			behind = 0
		}
		if behind < 0 {
			behind = 0
		}
		if st.Phase == "incremental" {
			st.ChangesBehind = &behind
			lag := 0.0
			if behind > 0 && j.remoteHeadAt != nil {
				ref := j.checkpointChangeAt
				if ref == nil {
					ref = &j.CreatedAt
				}
				lag = j.remoteHeadAt.Sub(*ref).Seconds()
				if lag < 0 {
					lag = 0
				}
			}
			st.LagSeconds = &lag
		}
	}
	return j, nil
}

// JobInput creates a job and its read-only replica source on the destination feed.
type JobInput struct {
	Name            string   `json:"name"`
	PeerID          string   `json:"peer_id"`
	RemoteFeedID    string   `json:"remote_feed_id"`
	DestFeedID      string   `json:"dest_feed_id"`
	Mode            string   `json:"mode"`
	IntervalSeconds *int     `json:"interval_seconds"`
	IncludeCached   *bool    `json:"include_cached"`
	Filters         *Filters `json:"filters"`
	DeletionPolicy  string   `json:"deletion_policy"`
}

func validMode(m string) bool { return m == "continuous" || m == "scheduled" || m == "manual" }
func validPolicy(p string) bool {
	return p == "ignore" || p == "propagate"
}

// CreateJob attaches a new replica-kind source (never writable through any
// publish path) at the lowest precedence of the destination feed and creates the
// job. Existing destination content and configuration are never modified.
func (s *Service) CreateJob(ctx context.Context, instance, actor string, in JobInput) (Job, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if in.Mode == "" {
		in.Mode = "continuous"
	}
	if in.DeletionPolicy == "" {
		in.DeletionPolicy = "ignore"
	}
	interval := 300
	if in.IntervalSeconds != nil {
		interval = *in.IntervalSeconds
	}
	include := true
	if in.IncludeCached != nil {
		include = *in.IncludeCached
	}
	filters := Filters{}
	if in.Filters != nil {
		filters = *in.Filters
	}
	if in.Name == "" || !validMode(in.Mode) || !validPolicy(in.DeletionPolicy) || interval < 1 || interval > 604800 || !filters.valid() ||
		!uuidPattern.MatchString(in.PeerID) || !uuidPattern.MatchString(in.RemoteFeedID) || !uuidPattern.MatchString(in.DestFeedID) {
		return Job{}, ErrInput
	}
	encoded, _ := json.Marshal(filters)
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Job{}, ErrUnavailable
	}
	defer rollback(tx)
	var format string
	if err = tx.QueryRow(ctx, `SELECT format FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, in.DestFeedID).Scan(&format); err != nil {
		return Job{}, dbError(err)
	}
	var peerOK bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM replication_peers WHERE instance_id=$1 AND id=$2)`, instance, in.PeerID).Scan(&peerOK); err != nil {
		return Job{}, ErrUnavailable
	}
	if !peerOK {
		return Job{}, ErrNotFound
	}
	var source string
	if err = tx.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,'replica',$3) RETURNING id::text`, instance, "replica:"+in.Name, format).Scan(&source); err != nil {
		return Job{}, dbError(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, instance, "source:"+source); err != nil {
		return Job{}, dbError(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) SELECT $1,$2,$3,COALESCE(max(priority),0)+1 FROM feed_sources WHERE feed_id=$2`, instance, in.DestFeedID, source); err != nil {
		return Job{}, dbError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id=$2`, instance, in.DestFeedID); err != nil {
		return Job{}, ErrUnavailable
	}
	var next any
	if in.Mode != "manual" {
		next = time.Now().UTC()
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO replication_jobs(instance_id,name,peer_id,remote_feed_id,dest_feed_id,replica_source_id,mode,interval_seconds,include_cached,filters,deletion_policy,next_run_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id::text`, instance, in.Name, in.PeerID, in.RemoteFeedID, in.DestFeedID, source, in.Mode, interval, include, encoded, in.DeletionPolicy, next).Scan(&id)
	if err != nil {
		return Job{}, dbError(err)
	}
	if err = audit(ctx, tx, instance, actor, "replication.job.create", id, map[string]any{"name": in.Name, "dest_feed_id": in.DestFeedID}); err != nil {
		return Job{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return Job{}, ErrUnavailable
	}
	return s.GetJob(ctx, instance, id)
}

func (s *Service) GetJob(ctx context.Context, instance, id string) (Job, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	j, err := scanJob(s.cfg.DB.QueryRow(ctx, `SELECT `+jobColumns+` FROM replication_jobs j WHERE j.instance_id=$1 AND j.id=$2`, instance, id))
	if err != nil {
		return Job{}, dbError(err)
	}
	return j.Job, nil
}

func (s *Service) ListJobs(ctx context.Context, instance string) ([]Job, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.cfg.DB.Query(ctx, `SELECT `+jobColumns+` FROM replication_jobs j WHERE j.instance_id=$1 ORDER BY j.name LIMIT 1000`, instance)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, ErrUnavailable
		}
		out = append(out, j.Job)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return out, nil
}

// JobPatch changes scheduling, filters or the deletion policy at an exact
// revision. Filter and cache changes apply to future changes only; content
// already replicated is never removed by them.
type JobPatch struct {
	Revision        int64    `json:"revision"`
	Mode            *string  `json:"mode"`
	IntervalSeconds *int     `json:"interval_seconds"`
	IncludeCached   *bool    `json:"include_cached"`
	Filters         *Filters `json:"filters"`
	DeletionPolicy  *string  `json:"deletion_policy"`
	Enabled         *bool    `json:"enabled"`
}

func (s *Service) UpdateJob(ctx context.Context, instance, actor, id string, in JobPatch) (Job, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if in.Revision < 1 {
		return Job{}, ErrInput
	}
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Job{}, ErrUnavailable
	}
	defer rollback(tx)
	cur, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM replication_jobs j WHERE j.instance_id=$1 AND j.id=$2 FOR UPDATE OF j`, instance, id))
	if err != nil {
		return Job{}, dbError(err)
	}
	if cur.Revision != in.Revision {
		return Job{}, ErrConflict
	}
	mode, interval, include, filters, policy, enabled := cur.Mode, cur.IntervalSeconds, cur.IncludeCached, cur.Filters, cur.DeletionPolicy, cur.Enabled
	if in.Mode != nil {
		mode = *in.Mode
	}
	if in.IntervalSeconds != nil {
		interval = *in.IntervalSeconds
	}
	if in.IncludeCached != nil {
		include = *in.IncludeCached
	}
	if in.Filters != nil {
		filters = *in.Filters
	}
	if in.DeletionPolicy != nil {
		policy = *in.DeletionPolicy
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if !validMode(mode) || !validPolicy(policy) || interval < 1 || interval > 604800 || !filters.valid() {
		return Job{}, ErrInput
	}
	encoded, _ := json.Marshal(filters)
	// Re-enable or a switch away from manual schedules a run; a switch to
	// manual stops automatic runs unless one was explicitly requested.
	_, err = tx.Exec(ctx, `UPDATE replication_jobs SET mode=$3,interval_seconds=$4,include_cached=$5,filters=$6,deletion_policy=$7,enabled=$8,revision=revision+1,
 next_run_at=CASE WHEN NOT $8 THEN NULL WHEN $3='manual' THEN CASE WHEN run_requested THEN next_run_at END ELSE COALESCE(next_run_at,clock_timestamp()) END
 WHERE instance_id=$1 AND id=$2`, instance, id, mode, interval, include, encoded, policy, enabled)
	if err != nil {
		return Job{}, dbError(err)
	}
	if err = audit(ctx, tx, instance, actor, "replication.job.update", id, map[string]any{"deletion_policy": policy, "mode": mode, "enabled": enabled}); err != nil {
		return Job{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return Job{}, ErrUnavailable
	}
	return s.GetJob(ctx, instance, id)
}

// DeleteJob removes the job only. Replicated content and the replica source stay
// in place so the destination keeps serving it; remove them separately.
func (s *Service) DeleteJob(ctx context.Context, instance, actor, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ErrUnavailable
	}
	defer rollback(tx)
	var running bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(lease_until>clock_timestamp(),false) FROM replication_jobs WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, id).Scan(&running)
	if err != nil {
		return dbError(err)
	}
	if running {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `DELETE FROM replication_jobs WHERE instance_id=$1 AND id=$2`, instance, id); err != nil {
		return dbError(err)
	}
	if err = audit(ctx, tx, instance, actor, "replication.job.delete", id, map[string]any{}); err != nil {
		return ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Trigger requests an immediate run (manual or out-of-schedule sync).
func (s *Service) Trigger(ctx context.Context, instance, actor, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tag, err := s.cfg.DB.Exec(ctx, `UPDATE replication_jobs SET run_requested=true,next_run_at=clock_timestamp() WHERE instance_id=$1 AND id=$2 AND enabled`, instance, id)
	if err != nil {
		return ErrUnavailable
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
