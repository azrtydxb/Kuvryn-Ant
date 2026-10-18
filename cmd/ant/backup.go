package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"

	"github.com/azrtydxb/Kuvryn-Ant/internal/backup"
	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
)

const backupUsage = "usage: ant backup [--concurrency N] <new-or-empty-dir> | ant backup verify [--expect-manifest-sha256 HEX] <dir> | ant restore [--force] [--expect-manifest-sha256 HEX] [--concurrency N] <dir>"

// backupCommand handles `backup`, `backup verify` and `restore`. It runs before
// the normal startup path: backup must not migrate the source database, restore
// must migrate only after its own emptiness check, and verify needs nothing.
func backupCommand(ctx context.Context, logger *slog.Logger, args []string, output io.Writer) int {
	mode, rest := args[0], args[1:]
	if mode == "backup" && len(rest) > 0 && rest[0] == "verify" {
		mode, rest = "verify", rest[1:]
	}
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	expect := fs.String("expect-manifest-sha256", "", "")
	force := fs.Bool("force", false, "")
	workers := fs.Int("concurrency", 4, "")
	if fs.Parse(rest) != nil || fs.NArg() != 1 || *workers < 1 || *workers > 64 || *force && mode != "restore" || *expect != "" && mode == "backup" {
		logger.Error(backupUsage)
		return 1
	}
	dir := fs.Arg(0)
	if mode == "verify" {
		summary, err := backup.Verify(ctx, dir, *expect, *workers)
		if err != nil {
			logger.Error("backup verification failed", "reason", err)
			return 1
		}
		return emit(logger, output, struct {
			Verified bool `json:"verified"`
			backup.Summary
		}{true, summary})
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
	stores, err := storage.New(cfg.Storage)
	if err != nil {
		logger.Error("storage configuration rejected", "reason", err)
		return 1
	}
	defer stores.Close()
	if mode == "backup" {
		res, err := backup.Create(ctx, pool, stores, dir, backup.Options{Concurrency: *workers})
		if err != nil {
			logger.Error("backup failed", "reason", err)
			return 1
		}
		return emit(logger, output, struct {
			Created bool `json:"created"`
			backup.Summary
			Notice string `json:"notice"`
		}{true, summaryOf(res), res.Manifest.Notice})
	}
	res, err := backup.Restore(ctx, pool, stores, dir, backup.RestoreOptions{Force: *force, Keys: cfg.SecretKeys, ExpectManifestSHA256: *expect, Concurrency: *workers})
	if err != nil {
		logger.Error("restore failed", "reason", err)
		if errors.Is(err, backup.ErrKeyring) {
			logger.Error("mount the keyring (ANT_ROOT_KEY_FILE, ANT_PREVIOUS_ROOT_KEYS_FILE) that protected the source deployment")
		}
		return 1
	}
	return emit(logger, output, struct {
		Restored bool `json:"restored"`
		backup.RestoreSummary
	}{true, res})
}

func summaryOf(r backup.Result) backup.Summary {
	s := backup.Summary{ManifestSHA256: r.ManifestSHA256, SchemaVersion: r.Manifest.SchemaVersion, Tables: len(r.Manifest.Tables), Blobs: len(r.Manifest.Blobs), BlobBytes: r.Manifest.BlobBytes()}
	for _, t := range r.Manifest.Tables {
		s.Rows += t.Rows
	}
	return s
}

func emit(logger *slog.Logger, output io.Writer, v any) int {
	if err := json.NewEncoder(output).Encode(v); err != nil {
		logger.Error("cannot write summary")
		return 1
	}
	return 0
}
