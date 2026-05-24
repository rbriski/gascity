package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenHQStoreAtCachesByStoreDir(t *testing.T) {
	if testing.Short() {
		t.Skip("opens real HQStore and starts its TTL goroutine")
	}

	dir := t.TempDir()
	s0, err := openHQStoreAt(dir, dir)
	if err != nil {
		t.Fatalf("openHQStoreAt initial: %v", err)
	}
	baseline := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		scopeRoot := dir
		if i%2 == 1 {
			scopeRoot = filepath.Join(dir, ".")
		}
		s, err := openHQStoreAt(scopeRoot, dir)
		if err != nil {
			t.Fatalf("openHQStoreAt call %d: %v", i, err)
		}
		if s != s0 {
			t.Fatalf("call %d returned a different *HQStore (cache miss)", i)
		}
	}

	if got := runtime.NumGoroutine(); got > baseline+1 {
		t.Fatalf("NumGoroutine grew: baseline=%d after-50-calls=%d (want <= baseline+1)", baseline, got)
	}
}
