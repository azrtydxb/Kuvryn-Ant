package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

const storageUsage = "usage: ant storage status | ant storage migrate --to disk|s3 [--dry-run] [--throttle-mb-per-sec N] | ant storage cleanup [--dry-run]"

// openStores builds the blob registry with the persisted active-backend router,
// so every command and the server agree on where blobs live after a migration.
func openStores(pool *pgxpool.Pool, cfg config.Config, instance string) (*storage.Registry, *storage.DBRouter, error) {
	stores, err := storage.New(cfg.Storage)
	if err != nil {
		return nil, nil, err
	}
	router := &storage.DBRouter{DB: pool, InstanceID: instance, Default: cfg.Storage.Backend}
	stores.SetRouter(router)
	return stores, router, nil
}

func newMigrator(pool *pgxpool.Pool, cfg config.Config, instance string, stores *storage.Registry, router *storage.DBRouter) *storage.Migrator {
	owner, _ := os.Hostname()
	return &storage.Migrator{DB: pool, InstanceID: instance, Backends: stores, Default: cfg.Storage.Backend, OnSwitch: router.Invalidate, Owner: owner + "/" + strconv.Itoa(os.Getpid())}
}

// storageCommand implements `ant storage ...`. Like gc it is authorized by
// possession of the deployment's database and storage configuration. It is safe
// to run while the service is serving; Ctrl-C pauses the job and running the same
// command again resumes it from the persisted cursor.
func storageCommand(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	if len(args) < 2 {
		logger.Error(storageUsage)
		return 1
	}
	verb := args[1]
	fs := flag.NewFlagSet("storage "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "")
	dry := fs.Bool("dry-run", false, "")
	mb := fs.Int64("throttle-mb-per-sec", 0, "")
	if fs.Parse(args[2:]) != nil || fs.NArg() != 0 || *mb < 0 || *mb > 1<<20 ||
		(verb != "migrate" && verb != "cleanup" && verb != "status") ||
		(verb == "migrate" && *to == "") || (verb != "migrate" && (*to != "" || *mb != 0)) || (verb == "status" && *dry) {
		logger.Error(storageUsage)
		return 1
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("configuration rejected", "reason", err)
		return 1
	}
	defer clear(cfg.RootKey)
	pool, err := database.Open(ctx, cfg.Database)
	if err != nil {
		logger.Error("startup failed", "reason", err)
		return 1
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		logger.Error("schema initialization failed", "reason", err)
		return 1
	}
	owner, err := identity.LoadBootstrapIdentity(ctx, pool)
	if err != nil {
		logger.Error("no bootstrapped instance", "reason", err)
		return 1
	}
	if cfg.InstanceID != "" && !strings.EqualFold(cfg.InstanceID, owner.InstanceID) {
		logger.Error("storage instance does not match local recovery identity")
		return 1
	}
	stores, router, err := openStores(pool, cfg, owner.InstanceID)
	if err != nil {
		logger.Error("storage configuration rejected", "reason", err)
		return 1
	}
	defer stores.Close()
	m := newMigrator(pool, cfg, owner.InstanceID, stores, router)
	if verb == "status" {
		st, err := m.Status(ctx)
		if err != nil {
			logger.Error("storage status failed", "reason", err)
			return 1
		}
		return emit(logger, output, st)
	}
	opts := storage.StartOptions{Kind: storage.KindMigrate, To: *to, DryRun: *dry, BytesPerSec: *mb << 20}
	if verb == "cleanup" {
		opts.Kind = storage.KindCleanup
	}
	job, resumed, err := m.Start(ctx, opts)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrBusy):
			logger.Error("another storage job is running; wait for it or let it go idle for 90s")
		case errors.Is(err, storage.ErrCleanupPending):
			logger.Error("previous storage still retained; run `ant storage cleanup` first")
		case errors.Is(err, storage.ErrBadTarget):
			logger.Error("invalid target: it must be configured, differ from the active backend", "available", stores.Available())
		default:
			logger.Error("storage job rejected", "reason", err)
		}
		return 1
	}
	if resumed {
		logger.Info("resuming interrupted storage job", "job", job.ID, "processed", job.Processed, "of", job.TotalBlobs)
	}
	job, err = m.Run(ctx, job.ID)
	if emit(logger, output, job) != 0 {
		return 1
	}
	if err != nil {
		if ctx.Err() != nil {
			logger.Warn("interrupted; run the same command again to resume", "job", job.ID)
		} else {
			logger.Error("storage job failed", "job", job.ID, "reason", err)
		}
		return 1
	}
	return 0
}
