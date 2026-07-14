//go:build !windows

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func sessionReconcilerTraceAvailableBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("query trace filesystem space for %q: %w", path, err)
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
