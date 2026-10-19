//go:build !windows

// Package diskusage reports filesystem capacity for the data directory and
// backup targets.
package diskusage

import "syscall"

// Usage is the capacity of the filesystem holding a path. Free is the space
// available to unprivileged processes (what df reports as Avail).
type Usage struct {
	Total, Free uint64
}

// Of returns the usage of the filesystem that holds path.
func Of(path string) (Usage, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return Usage{}, err
	}
	bs := uint64(s.Bsize)
	return Usage{Total: uint64(s.Blocks) * bs, Free: uint64(s.Bavail) * bs}, nil
}
