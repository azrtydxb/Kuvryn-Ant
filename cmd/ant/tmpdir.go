package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// prepareStagingDir makes sure the directory named by TMPDIR exists and, for the
// sole serving process, removes staging files orphaned by a previous crash or
// eviction. Upload staging (os.CreateTemp("", "ant-*")) must live on the data
// volume: the pod's /tmp is a small emptyDir and large uploads evict the pod.
// Only ant- entries in the dedicated sibling "tmp" of the data directory are
// ever removed, never a shared system temp directory.
func prepareStagingDir(logger *slog.Logger, dataDir string, cleanStale bool) {
	dir := os.Getenv("TMPDIR")
	if dir == "" || dir == "/tmp" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Warn("cannot create staging directory", "dir", dir, "reason", err)
		return
	}
	if !cleanStale || dataDir == "" || filepath.Clean(dir) != filepath.Join(filepath.Dir(filepath.Clean(dataDir)), "tmp") {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ant-") && os.RemoveAll(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	if removed > 0 {
		logger.Info("removed stale staging files", "dir", dir, "count", removed)
	}
}
