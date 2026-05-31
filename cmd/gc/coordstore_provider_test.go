//go:build cgo && sqlite_cgo

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCoordstoreProviderOpensSQLiteStoreWithoutBdContract(t *testing.T) {
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
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "coordstore bead"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "ga-1" {
		t.Fatalf("created ID = %q, want ga-1", created.ID)
	}
	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if _, err := reopened.Get(created.ID); err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "coordstore", "beads.sqlite")); err != nil {
		t.Fatalf("coordstore db stat: %v", err)
	}
}
