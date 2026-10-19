package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Siblings are the backups that live next to one another under the same parent
// (a directory of backups, or a ".../backups/" object prefix).
type Siblings interface {
	// Names lists candidate backup names (immediate children).
	Names(ctx context.Context) ([]string, error)
	// Open returns a Tree for one sibling.
	Open(name string) (Tree, error)
	// Remove deletes one sibling entirely.
	Remove(ctx context.Context, name string) error
}

// RetentionPolicy bounds how many backups are kept. Zero values disable the
// corresponding rule. The newest backup is never removed.
type RetentionPolicy struct {
	Keep   int
	MaxAge time.Duration
}

func (p RetentionPolicy) Enabled() bool { return p.Keep > 0 || p.MaxAge > 0 }

// ApplyRetention removes old backups next to `current` (which was just created
// and always survives). Only directories holding a valid, complete manifest are
// candidates: anything else (other data, incomplete backups, unreadable
// manifests) is left alone. It returns the names it removed.
func ApplyRetention(ctx context.Context, sib Siblings, current string, now time.Time, p RetentionPolicy) ([]string, error) {
	if !p.Enabled() {
		return nil, nil
	}
	names, err := sib.Names(ctx)
	if err != nil {
		return nil, err
	}
	type entry struct {
		name string
		at   time.Time
	}
	var all []entry
	for _, n := range names {
		if n == current {
			all = append(all, entry{n, now})
			continue
		}
		t, err := sib.Open(n)
		if err != nil {
			continue
		}
		m, _, err := readManifest(ctx, t)
		t.Close()
		if err != nil {
			continue
		}
		all = append(all, entry{n, m.CreatedAt})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.After(all[j].at)
		}
		return all[i].name > all[j].name
	})
	var removed []string
	for i, e := range all {
		if i == 0 || e.name == current {
			continue
		}
		if p.Keep > 0 && i >= p.Keep || p.MaxAge > 0 && now.Sub(e.at) > p.MaxAge {
			if err := sib.Remove(ctx, e.name); err != nil {
				return removed, fmt.Errorf("cannot remove old backup %q: %w", e.name, err)
			}
			removed = append(removed, e.name)
		}
	}
	return removed, nil
}

// DirSiblings are the backup directories inside parent.
type DirSiblings struct{ Parent string }

func (d DirSiblings) Names(context.Context) ([]string, error) {
	entries, err := os.ReadDir(d.Parent)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() { // a symlink's type is not ModeDir, so links are never followed
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func (d DirSiblings) Open(name string) (Tree, error) {
	return OpenDirTree(filepath.Join(d.Parent, name), false)
}

func (d DirSiblings) Remove(_ context.Context, name string) error {
	return os.RemoveAll(filepath.Join(d.Parent, name))
}

// ObjectSiblings are the backups under one object prefix.
type ObjectSiblings struct {
	API    ObjectAPI
	Bucket string
	Parent string // key prefix of the directory of backups
}

func (o ObjectSiblings) Names(ctx context.Context) ([]string, error) {
	return childNames(ctx, o.API, o.Bucket, o.Parent)
}

func (o ObjectSiblings) Open(name string) (Tree, error) {
	return NewObjectTree(o.API, o.Bucket, o.Parent+"/"+name), nil
}

func (o ObjectSiblings) Remove(ctx context.Context, name string) error {
	t := NewObjectTree(o.API, o.Bucket, o.Parent+"/"+name)
	names, err := t.List(ctx)
	if err != nil {
		return err
	}
	// Manifest first: the backup stops being "complete" before its parts go.
	if err := t.RemoveAll(ctx, manifestName); err != nil {
		return err
	}
	for _, n := range names {
		if err := t.RemoveAll(ctx, n); err != nil {
			return err
		}
	}
	return nil
}
