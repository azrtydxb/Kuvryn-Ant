package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/diagnostics"
	"github.com/azrtydxb/Kuvryn-Ant/internal/maintenance"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oci"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errorRing holds recent WARN/ERROR log lines of this process. It is installed
// by main and nil in tests.
var errorRing *diagnostics.Ring

const diagnosticsUsage = "usage: ant diagnostics [-o FILE|-]  (default ant-diagnostics-<timestamp>.tar.gz)"
const repairUsage = "usage: ant repair metadata [--dry-run] | ant repair oci-index-children [--dry-run]"

type storeResolver func(ctx context.Context, domain string) (blob.Store, error)

// storageCheck proves the blob backend answers: a missing object is success.
func storageCheck(pool *pgxpool.Pool, resolve storeResolver) diagnostics.Check {
	return diagnostics.Check{Name: "blob_storage", Run: func(ctx context.Context) (string, error) {
		domain := "00000000-0000-0000-0000-000000000000"
		var existing string
		if pool != nil && pool.QueryRow(ctx, `SELECT id::text FROM dedup_domains ORDER BY id LIMIT 1`).Scan(&existing) == nil {
			domain = existing
		}
		store, err := resolve(ctx, domain)
		if err != nil {
			return "", errors.New("store_unavailable")
		}
		_, err = store.Stat(ctx, blob.Digest("0000000000000000000000000000000000000000000000000000000000000000"))
		if err != nil && !errors.Is(err, blob.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
			return "", errors.New("probe_failed")
		}
		return "reachable", nil
	}}
}

func databaseChecks(pool *pgxpool.Pool) (diagnostics.Check, func(context.Context) (any, error)) {
	ping := diagnostics.Check{Name: "database", Run: func(ctx context.Context) (string, error) {
		if pool == nil {
			return "", errors.New("not_connected")
		}
		if err := pool.Ping(ctx); err != nil {
			return "", errors.New("ping_failed")
		}
		var version string
		_ = pool.QueryRow(ctx, `SHOW server_version`).Scan(&version)
		return "server " + version, nil
	}}
	migration := func(ctx context.Context) (any, error) {
		if pool == nil {
			return nil, errors.New("not_connected")
		}
		return database.SchemaStatus(ctx, pool)
	}
	return ping, migration
}

// diagnosticsInput assembles the bundle for a running process (resolve and
// health may be nil).
func diagnosticsInput(cfg config.Config, pool *pgxpool.Pool, resolve storeResolver, health func(context.Context) any, source string) diagnostics.Input {
	ping, migration := databaseChecks(pool)
	in := diagnostics.Input{Config: cfg.Redacted(), Secrets: cfg.SecretValues(), Ring: errorRing, Migration: migration, Health: health, Checks: []diagnostics.Check{ping}, Source: source}
	if resolve != nil {
		in.Checks = append(in.Checks, storageCheck(pool, resolve))
	}
	return in
}

// diagnosticsCommand writes a redacted support bundle. It works even when the
// database or storage is unreachable (those become failed checks), and never
// migrates. The in-memory error log only covers this short-lived process; use
// the admin endpoint for the running service's recent errors.
func diagnosticsCommand(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	set := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	out := set.String("o", "", "")
	if set.Parse(args) != nil || set.NArg() != 0 {
		logger.Error(diagnosticsUsage)
		return 1
	}
	var in diagnostics.Input
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		in = diagnostics.Input{Config: map[string]any{"load_error": err.Error()}, Ring: errorRing, Source: "cli"}
	} else {
		defer clear(cfg.RootKey)
		var pool *pgxpool.Pool
		if p, e := database.Open(ctx, cfg.Database); e == nil {
			pool = p
			defer p.Close()
		}
		var resolve storeResolver
		if stores, e := storage.New(cfg.Storage); e == nil {
			defer stores.Close()
			resolve = stores.Resolve
		}
		in = diagnosticsInput(cfg, pool, resolve, nil, "cli")
	}
	var buf bytes.Buffer
	if err = diagnostics.Build(ctx, &buf, in); err != nil {
		logger.Error("diagnostics bundle failed")
		return 1
	}
	if *out == "-" {
		_, err = output.Write(buf.Bytes())
		if err != nil {
			return 1
		}
		return 0
	}
	path := *out
	if path == "" {
		path = "ant-diagnostics-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	}
	if err = os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		logger.Error("cannot write diagnostics bundle")
		return 1
	}
	return emit(logger, output, struct {
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	}{path, buf.Len()})
}

// repairCommand implements `ant repair metadata [--dry-run]`. It refuses to run
// unless the schema matches this binary, and never migrates.
func repairCommand(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	if len(args) < 1 || (args[0] != "metadata" && args[0] != "oci-index-children") {
		logger.Error(repairUsage)
		return 1
	}
	set := flag.NewFlagSet("repair", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	dry := set.Bool("dry-run", false, "")
	if set.Parse(args[1:]) != nil || set.NArg() != 0 {
		logger.Error(repairUsage)
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
	state, err := database.SchemaStatus(ctx, pool)
	if err != nil || !state.Current {
		logger.Error("schema does not match this binary; start the service (or upgrade) first", "applied", state.Applied, "binary", state.Latest)
		return 1
	}
	if args[0] == "oci-index-children" {
		stores, serr := storage.New(cfg.Storage)
		if serr != nil {
			logger.Error("storage configuration rejected", "reason", serr)
			return 1
		}
		defer stores.Close()
		result, rerr := oci.BackfillIndexChildren(ctx, pool, stores.Resolve, *dry)
		if code := emit(logger, output, result); code != 0 {
			return code
		}
		if rerr != nil {
			logger.Error("index child backfill failed", "reason", rerr)
			return 1
		}
		return 0
	}
	report, err := maintenance.RepairMetadata(ctx, pool, *dry)
	if code := emit(logger, output, report); code != 0 {
		return code
	}
	if err != nil {
		logger.Error("metadata repair failed; no changes were committed", "reason", err)
		return 1
	}
	return 0
}
