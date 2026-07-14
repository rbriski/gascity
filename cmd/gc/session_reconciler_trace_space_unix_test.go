//go:build !windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionReconcilerTraceAvailableBytes(t *testing.T) {
	dir := t.TempDir()
	available, err := sessionReconcilerTraceAvailableBytes(dir)
	if err != nil {
		t.Fatalf("sessionReconcilerTraceAvailableBytes(%q): %v", dir, err)
	}
	if available == 0 {
		t.Fatalf("sessionReconcilerTraceAvailableBytes(%q) = 0, want positive available capacity", dir)
	}

	missing := filepath.Join(dir, "missing")
	if _, err := sessionReconcilerTraceAvailableBytes(missing); err == nil {
		t.Fatalf("sessionReconcilerTraceAvailableBytes(%q) error = nil, want error", missing)
	} else if !strings.Contains(err.Error(), missing) {
		t.Fatalf("sessionReconcilerTraceAvailableBytes(%q) error = %q, want path context", missing, err)
	}
}
