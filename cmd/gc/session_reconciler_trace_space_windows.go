//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func sessionReconcilerTraceAvailableBytes(path string) (uint64, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("query trace filesystem space for %q: %w", path, err)
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(pathPointer, &available, nil, nil); err != nil {
		return 0, fmt.Errorf("query trace filesystem space for %q: %w", path, err)
	}
	return available, nil
}
