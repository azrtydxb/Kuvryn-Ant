// Package safedir decides whether a data directory is private enough to use.
package safedir

import (
	"io/fs"
	"os"
	"syscall"
)

// Private reports whether info describes a directory only this process (and
// its own group) can reach. Any access for "other" is rejected. Group access
// is accepted only when the directory's group is the process's effective gid or
// one of its supplementary groups (Kubernetes fsGroup makes volumes
// group-writable by design) and the directory is owned by this process's uid
// or root. Without group bits the historic behaviour applies.
func Private(info fs.FileInfo) bool {
	if info == nil || !info.IsDir() {
		return false
	}
	perm := info.Mode().Perm()
	if perm&0007 != 0 {
		return false
	}
	if perm&0070 == 0 {
		return true
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if uid := uint32(os.Geteuid()); st.Uid != uid && st.Uid != 0 {
		return false
	}
	return ownGroup(st.Gid)
}

func ownGroup(gid uint32) bool {
	if gid == uint32(os.Getegid()) {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if uint32(g) == gid {
			return true
		}
	}
	return false
}
