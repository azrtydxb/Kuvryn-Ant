package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/backup"
	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
)

const backupUsage = "usage: ant backup [--concurrency N] [--blobs auto|copy|reference] [--keep N] [--max-age DURATION] [--max-disk-percent P] <new-or-empty-dir | s3://bucket/prefix/backups/name> | ant backup verify [--expect-manifest-sha256 HEX] [--check-blobs] <dir | s3://...> | ant restore [--force] [--expect-manifest-sha256 HEX] [--concurrency N] <dir | s3://...>"

// objectClient builds the object-storage client for s3:// backup locations. It
// is a variable so tests can substitute an in-memory object store.
var objectClient = func(stores *storage.Registry) (backup.ObjectAPI, bool) {
	c, ok := stores.ObjectClient()
	if !ok {
		return nil, false
	}
	return c, true
}

// backupLocation is where a backup lives: a directory, or an object prefix.
type backupLocation struct {
	tree backup.Tree
	sib  backup.Siblings
	name string
}

func openLocation(loc string, stores *storage.Registry, create bool) (backupLocation, error) {
	bucket, key, isS3, err := backup.ParseS3Location(loc)
	if err != nil {
		return backupLocation{}, err
	}
	if !isS3 {
		abs, err := filepath.Abs(loc)
		if err != nil {
			return backupLocation{}, err
		}
		t, err := backup.OpenDirTree(abs, create)
		if err != nil {
			return backupLocation{}, err
		}
		return backupLocation{t, backup.DirSiblings{Parent: filepath.Dir(abs)}, filepath.Base(abs)}, nil
	}
	if stores == nil {
		return backupLocation{}, errors.New("an s3:// backup location needs the S3 settings (ANT_S3_*)")
	}
	api, ok := objectClient(stores)
	if !ok {
		return backupLocation{}, errors.New("an s3:// backup location needs S3 to be configured (ANT_S3_REGION, ANT_S3_BUCKET, ANT_S3_CREDENTIALS_FILE)")
	}
	parent, name := path.Split(key)
	parent = strings.Trim(parent, "/")
	loc2 := backupLocation{tree: backup.NewObjectTree(api, bucket, key), name: name}
	if parent != "" {
		loc2.sib = backup.ObjectSiblings{API: api, Bucket: bucket, Parent: parent}
	}
	return loc2, nil
}

// resolveBlobMode turns --blobs auto into a concrete mode: when blobs live in
// durable object storage the backup references them instead of copying them.
func resolveBlobMode(flagValue, activeBackend string) (string, error) {
	switch flagValue {
	case "", "auto":
		if activeBackend == "s3" {
			return backup.BlobsReference, nil
		}
		return backup.BlobsCopy, nil
	case backup.BlobsCopy, backup.BlobsReference:
		return flagValue, nil
	}
	return "", fmt.Errorf("--blobs must be auto, copy or reference")
}

// backupCommand handles `backup`, `backup verify` and `restore`. It runs before
// the normal startup path: backup must not migrate the source database, restore
// must migrate only after its own emptiness check, and verify needs nothing
// (except S3 settings for an s3:// location or --check-blobs).
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
	blobs := fs.String("blobs", "auto", "")
	keep := fs.Int("keep", 0, "")
	maxAge := fs.Duration("max-age", 0, "")
	maxDisk := fs.Int("max-disk-percent", 80, "")
	checkBlobs := fs.Bool("check-blobs", false, "")
	badFlags := func() bool {
		creating := mode == "backup"
		return *force && mode != "restore" || *expect != "" && creating || *checkBlobs && mode != "verify" ||
			!creating && (*blobs != "auto" || *keep != 0 || *maxAge != 0 || fs.Lookup("max-disk-percent").Value.String() != "80") ||
			*keep < 0 || *maxAge < 0 || *maxDisk < 0 || *maxDisk > 100
	}
	if fs.Parse(rest) != nil || fs.NArg() != 1 || *workers < 1 || *workers > 64 || badFlags() {
		logger.Error(backupUsage)
		return 1
	}
	target := fs.Arg(0)
	_, _, isS3, _ := backup.ParseS3Location(target)
	if mode == "verify" && !isS3 && !*checkBlobs {
		loc, err := openLocation(target, nil, false)
		if err != nil {
			logger.Error("backup verification failed", "reason", err)
			return 1
		}
		defer loc.tree.Close()
		return verifyAndEmit(ctx, logger, output, loc, backup.VerifyOptions{Expect: *expect, Workers: *workers})
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("configuration rejected", "reason", err)
		return 1
	}
	defer clear(cfg.RootKey)
	if mode == "verify" {
		stores, err := storage.New(cfg.Storage)
		if err != nil {
			logger.Error("storage configuration rejected", "reason", err)
			return 1
		}
		defer stores.Close()
		loc, err := openLocation(target, stores, false)
		if err != nil {
			logger.Error("backup verification failed", "reason", err)
			return 1
		}
		defer loc.tree.Close()
		opts := backup.VerifyOptions{Expect: *expect, Workers: *workers}
		if *checkBlobs {
			opts.Stores = stores
		}
		return verifyAndEmit(ctx, logger, output, loc, opts)
	}
	pool, err := database.Open(ctx, cfg.Database)
	if err != nil {
		logger.Error("startup failed", "reason", err)
		return 1
	}
	defer pool.Close()
	stores, _, err := openStores(pool, cfg, cfg.InstanceID)
	if err != nil {
		logger.Error("storage configuration rejected", "reason", err)
		return 1
	}
	defer stores.Close()
	loc, err := openLocation(target, stores, mode == "backup")
	if err != nil {
		logger.Error(mode+" location rejected", "reason", err)
		return 1
	}
	defer loc.tree.Close()
	if mode == "backup" {
		blobMode, err := resolveBlobMode(*blobs, cfg.Storage.Backend)
		if err != nil {
			logger.Error(backupUsage)
			return 1
		}
		if (*keep > 0 || *maxAge > 0) && loc.sib == nil {
			logger.Error("--keep/--max-age need a backup location inside a directory of backups, e.g. s3://bucket/prefix/backups/<name>")
			return 1
		}
		res, err := backup.CreateTree(ctx, pool, stores, loc.tree, backup.Options{Concurrency: *workers, Blobs: blobMode, MaxDiskPercent: *maxDisk})
		if err != nil {
			logger.Error("backup failed", "reason", err)
			return 1
		}
		for _, w := range res.Warnings {
			logger.Warn("backup warning", "reason", w)
		}
		var removed []string
		if policy := (backup.RetentionPolicy{Keep: *keep, MaxAge: *maxAge}); policy.Enabled() {
			// The new backup is complete and durable; a retention failure must not
			// turn it into a failed backup, but it must be loud.
			removed, err = backup.ApplyRetention(ctx, loc.sib, loc.name, time.Now().UTC(), policy)
			if err != nil {
				logger.Error("backup created but retention failed", "reason", err)
			}
		}
		return emit(logger, output, struct {
			Created bool `json:"created"`
			backup.Summary
			Removed  []string `json:"removed_old_backups"`
			Warnings []string `json:"warnings"`
			Notice   string   `json:"notice"`
		}{true, summaryOf(res), nonNil(removed), nonNil(res.Warnings), res.Manifest.Notice})
	}
	res, err := backup.RestoreTree(ctx, pool, stores, loc.tree, backup.RestoreOptions{Force: *force, Keys: cfg.SecretKeys, ExpectManifestSHA256: *expect, Concurrency: *workers})
	if err != nil {
		logger.Error("restore failed", "reason", err)
		if errors.Is(err, backup.ErrKeyring) {
			logger.Error("mount the keyring (ANT_ROOT_KEY_FILE, ANT_PREVIOUS_ROOT_KEYS_FILE) that protected the source deployment")
		}
		return 1
	}
	if res.DroppedCacheVersions > 0 {
		logger.Warn("restore dropped cache versions whose blobs were missing", "versions", res.DroppedCacheVersions, "missing_blobs", res.MissingBlobs)
	}
	return emit(logger, output, struct {
		Restored bool `json:"restored"`
		backup.RestoreSummary
	}{true, res})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func verifyAndEmit(ctx context.Context, logger *slog.Logger, output io.Writer, loc backupLocation, opts backup.VerifyOptions) int {
	summary, err := backup.VerifyTree(ctx, loc.tree, opts)
	if err != nil {
		logger.Error("backup verification failed", "reason", err)
		return 1
	}
	return emit(logger, output, struct {
		Verified bool `json:"verified"`
		backup.Summary
	}{true, summary})
}

func summaryOf(r backup.Result) backup.Summary {
	s := backup.Summary{ManifestSHA256: r.ManifestSHA256, SchemaVersion: r.Manifest.SchemaVersion, Tables: len(r.Manifest.Tables), Blobs: len(r.Manifest.Blobs), BlobBytes: r.Manifest.BlobBytes(), BlobMode: r.Manifest.Mode(), MissingBlobs: r.Manifest.MissingCount()}
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
