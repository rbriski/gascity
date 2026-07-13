package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

type existingOnlyComposeFS struct{ fsys.FS }

func TestLoadWithIncludesDoesNotResolveUnusedImportsWithoutExtraPackIncludes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("GC_HOME", gcHome)
	ResetRemoteCacheValidationCache()
	t.Cleanup(ResetRemoteCacheValidationCache)

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	packToml := `[pack]
name = "bright-lights"
schema = 2

[imports.gastown]
source = "` + PublicGastownPackSource + `"
version = "` + PublicGastownPackVersion + `"
`
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimPrefix(PublicGastownPackVersion, "sha:")
	lockToml := strings.Join([]string{
		"schema = 1",
		"",
		`[packs."` + PublicGastownPackSource + `"]`,
		`version = "` + PublicGastownPackVersion + `"`,
		`commit = "` + commit + `"`,
		`fetched = "2026-01-01T00:00:00Z"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cityPath, "packs.lock"), []byte(lockToml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(existingOnlyComposeFS{FS: fsys.OSFS{}}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Fatalf("workspace name = %q, want bright-lights", cfg.Workspace.Name)
	}

	cacheDir := filepath.Join(gcHome, "cache", "repos", RepoCacheKey(PublicGastownPackSource, commit))
	if _, err := os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused import was resolved and materialized at %s: %v", cacheDir, err)
	}
}
