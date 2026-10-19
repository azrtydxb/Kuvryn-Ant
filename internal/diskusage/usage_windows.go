//go:build windows

// Package diskusage reports filesystem capacity for the data directory and
// backup targets.
package diskusage

import "errors"

type Usage struct {
	Total, Free uint64
}

func Of(string) (Usage, error) {
	return Usage{}, errors.New("disk usage is not supported on this platform")
}
