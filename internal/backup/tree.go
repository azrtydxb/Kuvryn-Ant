package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Tree is the storage a backup is written to and read from: a flat namespace of
// slash-separated file names ("manifest.json", "tables/x.copy",
// "blobs/<domain>/<digest>"). Directories implement it with os.Root (no symlink
// escapes); object storage implements it with one object per file.
type Tree interface {
	// Create starts a new file; the file exists only after Close succeeds.
	// Parent "directories" are created as needed. A writer that also has
	// Abort() is aborted on failure so no partial object remains.
	Create(ctx context.Context, name string) (io.WriteCloser, error)
	// PutAtomic stores data so that readers see all of it or none of it.
	PutAtomic(ctx context.Context, name string, data []byte) error
	// Open returns a regular file and its size.
	Open(ctx context.Context, name string) (io.ReadCloser, int64, error)
	// List returns every file name. Anything that is not a regular file is an
	// ErrIntegrity error.
	List(ctx context.Context) ([]string, error)
	// RemoveAll deletes every file at or below name.
	RemoveAll(ctx context.Context, name string) error
	Close() error
}

// emptyTree reports whether nothing exists at the backup location.
func emptyTree(ctx context.Context, t Tree) error {
	names, err := t.List(ctx)
	if err != nil || len(names) != 0 {
		return fmt.Errorf("%w: backup location must be empty", ErrTarget)
	}
	return nil
}

func abortWriter(w io.WriteCloser) {
	if a, ok := w.(interface{ Abort() }); ok {
		a.Abort()
		return
	}
	_ = w.Close()
}

// copyOut streams r into a new file of the tree, aborting it on failure.
func copyOut(ctx context.Context, t Tree, name string, fill func(io.Writer) error) error {
	w, err := t.Create(ctx, name)
	if err != nil {
		return err
	}
	if err = fill(w); err != nil {
		abortWriter(w)
		return err
	}
	return w.Close()
}

// DirTree is a backup in a local directory.
type DirTree struct {
	root *os.Root
	dir  string
}

// OpenDirTree opens an existing directory. With create it makes the directory
// (mode 0700) first.
func OpenDirTree(dir string, create bool) (*DirTree, error) {
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("%w: cannot create directory", ErrTarget)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open backup directory", ErrTarget)
	}
	return &DirTree{root: root, dir: dir}, nil
}

// Dir is the directory path, used to check free space on its filesystem.
func (d *DirTree) Dir() string { return d.dir }

func (d *DirTree) Close() error { return d.root.Close() }

func (d *DirTree) mkParents(name string) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(parent, "/") {
		cur = path.Join(cur, part)
		if err := d.root.Mkdir(cur, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

type syncFile struct{ f *os.File }

func (s syncFile) Write(p []byte) (int, error) { return s.f.Write(p) }
func (s syncFile) Close() error {
	err := s.f.Sync()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (d *DirTree) Create(_ context.Context, name string) (io.WriteCloser, error) {
	if err := d.mkParents(name); err != nil {
		return nil, err
	}
	f, err := d.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return syncFile{f}, nil
}

func (d *DirTree) PutAtomic(_ context.Context, name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := d.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = d.root.Remove(tmp)
		return err
	}
	return d.root.Rename(tmp, name)
}

func (d *DirTree) Open(_ context.Context, name string) (io.ReadCloser, int64, error) {
	info, err := d.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, ErrIntegrity
	}
	f, err := d.root.Open(name)
	if err != nil {
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func (d *DirTree) List(context.Context) ([]string, error) {
	var names []string
	err := fs.WalkDir(d.root.FS(), ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("%w: unexpected file %q", ErrIntegrity, p)
		}
		names = append(names, p)
		return nil
	})
	return names, err
}

func (d *DirTree) RemoveAll(_ context.Context, name string) error {
	if err := d.root.RemoveAll(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
