package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/filelock"
)

func openManagedDoltLifecycleLock(cityPath string) (*os.File, managedDoltRuntimeLayout, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, err
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		return nil, managedDoltRuntimeLayout{}, fmt.Errorf("create managed dolt lock dir: %w", err)
	}
	f, err := os.OpenFile(layout.LockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, fmt.Errorf("open managed dolt lock: %w", err)
	}
	return f, layout, nil
}

func tryManagedDoltLifecycleLock(f *os.File) (bool, error) {
	if f == nil {
		return false, fmt.Errorf("nil managed dolt lock file")
	}
	acquired, err := filelock.TryLock(f, filelock.Exclusive)
	if err != nil {
		return false, fmt.Errorf("lock managed dolt lifecycle: %w", err)
	}
	return acquired, nil
}

func releaseManagedDoltLifecycleLock(f *os.File) {
	if f == nil {
		return
	}
	_ = filelock.Unlock(f)
	_ = f.Close()
}
