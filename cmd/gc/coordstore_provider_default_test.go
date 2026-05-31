//go:build !sqlite_cgo

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoordstoreProviderDefaultBuildReturnsTagError(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\nprefix = \"ga\"\n\n[beads]\nprovider = \"sqlite\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	if got := rawBeadsProvider(cityDir); got != "sqlite" {
		t.Fatalf("rawBeadsProvider = %q, want sqlite", got)
	}
	if cityUsesBdStoreContract(cityDir) {
		t.Fatal("cityUsesBdStoreContract = true, want false for sqlite")
	}
	if got := beadsProvider(cityDir); got != "sqlite" {
		t.Fatalf("beadsProvider = %q, want sqlite", got)
	}
	if _, err := openStoreAtForCity(cityDir, cityDir); err == nil || !strings.Contains(err.Error(), "-tags sqlite_cgo") {
		t.Fatalf("openStoreAtForCity error = %v, want sqlite_cgo tag error", err)
	}
}
