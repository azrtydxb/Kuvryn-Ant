// Package retention implements retention policies, two-phase orphan blob garbage
// collection and background integrity scanning. See README.md for the safety
// protocol.
package retention

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrConfiguration = errors.New("invalid retention configuration")
	ErrInvalidPolicy = errors.New("invalid retention policy")
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Resolver maps a deduplication domain to its blob store (storage.Registry.Resolve).
type Resolver func(ctx context.Context, domain string) (blob.Store, error)

// Options bound the work done by one run. Zero values select defaults.
type Options struct {
	// Grace is the minimum age of an unreferenced blob before it is marked, and the
	// minimum time a mark must age before it can be swept. It must exceed the
	// longest publish commit window and any object-store consistency delay.
	Grace time.Duration
	// BatchSize bounds candidates marked or swept per run (default 500).
	BatchSize int
	// ObjectScanLimit bounds listed objects per domain per run (default 1000).
	ObjectScanLimit int
	// IntegrityBatch and IntegrityMaxBytes bound re-hashing per run.
	IntegrityBatch    int
	IntegrityMaxBytes int64
	// StaleUploadAge is how long a multipart upload may sit idle before GC
	// aborts it (default 24h).
	StaleUploadAge time.Duration
}

const (
	DefaultGrace             = 24 * time.Hour
	DefaultBatchSize         = 500
	DefaultObjectScanLimit   = 1000
	DefaultIntegrityBatch    = 200
	DefaultIntegrityMaxBytes = int64(1 << 30)
	DefaultStaleUploadAge    = 24 * time.Hour
)

func (o Options) withDefaults() Options {
	if o.Grace <= 0 {
		o.Grace = DefaultGrace
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.ObjectScanLimit <= 0 {
		o.ObjectScanLimit = DefaultObjectScanLimit
	}
	if o.IntegrityBatch <= 0 {
		o.IntegrityBatch = DefaultIntegrityBatch
	}
	if o.IntegrityMaxBytes <= 0 {
		o.IntegrityMaxBytes = DefaultIntegrityMaxBytes
	}
	if o.StaleUploadAge <= 0 {
		o.StaleUploadAge = DefaultStaleUploadAge
	}
	return o
}

// Service performs maintenance for exactly one instance.
type Service struct {
	DB         *pgxpool.Pool
	InstanceID string
	Stores     Resolver
	// Lease, when set, fences every mutating run: it must report the instance's
	// current writer lease. A lost lease aborts the run before any deletion.
	Lease   func() ownership.Lease
	Options Options
}

func (s *Service) opts() Options { return s.Options.withDefaults() }

func (s *Service) validate() error {
	if s == nil || s.DB == nil || s.Stores == nil || !uuidPattern.MatchString(s.InstanceID) {
		return ErrConfiguration
	}
	return nil
}

// requireLease verifies the writer lease in a short transaction.
func (s *Service) requireLease(ctx context.Context) error {
	if s.Lease == nil {
		return nil
	}
	lease := s.Lease()
	if lease.InstanceID != s.InstanceID {
		return ownership.ErrLost
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = ownership.Require(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// requireLeaseTx fences a mutating transaction at its commit point.
func (s *Service) requireLeaseTx(ctx context.Context, tx pgx.Tx) error {
	if s.Lease == nil {
		return nil
	}
	lease := s.Lease()
	if lease.InstanceID != s.InstanceID {
		return ownership.ErrLost
	}
	return ownership.Require(ctx, tx, lease)
}

// Report summarizes one maintenance run.
type Report struct {
	DryRun    bool            `json:"dry_run"`
	Retention RetentionReport `json:"retention"`
	GC        GCReport        `json:"gc"`
	Integrity IntegrityReport `json:"integrity"`
	Errors    []string        `json:"errors,omitempty"`
}

// RunOnce applies retention, then garbage collection, then one integrity batch.
// Stages are independent: a failure in one is recorded and the others still run.
func (s *Service) RunOnce(ctx context.Context, dryRun bool) (Report, error) {
	report := Report{DryRun: dryRun}
	if err := s.validate(); err != nil {
		return report, err
	}
	var errs []error
	stage := func(name string, run func() error) {
		if err := run(); err != nil {
			report.Errors = append(report.Errors, name+": "+err.Error())
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	stage("retention", func() (err error) { report.Retention, err = s.ApplyRetention(ctx, dryRun); return })
	stage("gc", func() (err error) { report.GC, err = s.CollectGarbage(ctx, dryRun); return })
	stage("integrity", func() (err error) { report.Integrity, err = s.VerifyIntegrity(ctx, dryRun); return })
	return report, errors.Join(errs...)
}

// Run executes RunOnce every interval until ctx ends. The first run starts
// immediately. handle (optional) observes each result.
func (s *Service) Run(ctx context.Context, interval time.Duration, dryRun bool, handle func(Report, error)) {
	if interval <= 0 {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		report, err := s.RunOnce(ctx, dryRun)
		if handle != nil {
			handle(report, err)
		}
		timer.Reset(interval)
	}
}

// Touch records that an asset was read, feeding max-idle remote-cache eviction.
// It is a best-effort single-row update intended to be called by readers.
func Touch(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, assetID string) error {
	_, err := db.Exec(ctx, "UPDATE assets SET last_accessed_at=clock_timestamp() WHERE id=$1 AND (last_accessed_at IS NULL OR last_accessed_at<clock_timestamp()-interval '1 minute')", assetID)
	return err
}
