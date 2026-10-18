package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareStagingDir(t *testing.T) {
	root := t.TempDir()
	dir, data := filepath.Join(root, "tmp"), filepath.Join(root, "data")
	t.Setenv("TMPDIR", dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prepareStagingDir(logger, data, true)
	stale, keep := filepath.Join(dir, "ant-npm-1"), filepath.Join(dir, "other")
	for _, p := range []string{stale, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prepareStagingDir(logger, data, false)
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("non-serving process removed staging file")
	}
	prepareStagingDir(logger, filepath.Join(root, "elsewhere", "data"), true)
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("swept a directory that is not the data sibling")
	}
	prepareStagingDir(logger, data, true)
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale staging file kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("foreign file removed")
	}
}
