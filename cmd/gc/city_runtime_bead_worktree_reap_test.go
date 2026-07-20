package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestCityRuntimeTick_SkipsClosedBeadWorktreeReapWhenDisabled verifies that
// the runtime tick does not invoke reapClosedBeadWorktrees when
// DaemonConfig.AutoReapClosedBeadWorktrees is explicitly false.
//
// Acceptance: ga-xxsd7k.2 — runtime tick skips the reaper when the daemon
// field is false.
func TestCityRuntimeTick_SkipsClosedBeadWorktreeReapWhenDisabled(t *testing.T) {
	cityPath := t.TempDir()

	// Create a worktree directory that the reaper would target if enabled.
	wtDir := filepath.Join(cityPath, ".gc", "worktrees", "mrig", "ga-abc123")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("creating worktree dir: %v", err)
	}

	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-abc123",
		Status: "closed",
	}}, nil)

	disabled := false
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Daemon:    config.DaemonConfig{AutoReapClosedBeadWorktrees: &disabled},
	}

	cityStore := beads.NewMemStore()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"mrig": rigStore},
		wg:                  fixedWispGC{},
		rec:                 events.Discard,
		logPrefix:           "test",
		stdout:              io.Discard,
		stderr:              &stderr,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cityPath, &prevPoolRunning, "test")

	// Worktree dir must survive — reaper was skipped.
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Error("worktree dir was removed, want skipped when auto_reap_closed_bead_worktrees=false")
	}
	if s := stderr.String(); strings.Contains(s, "reapClosedBeadWorktrees") {
		t.Errorf("stderr = %q, want no reaper output when disabled", s)
	}
}

// TestCityRuntimeTick_AttemptsClosedBeadWorktreeReapWhenEnabled verifies that
// the runtime tick invokes reapClosedBeadWorktrees when
// DaemonConfig.AutoReapClosedBeadWorktrees is explicitly true.
//
// The reaper's skipping log confirms the gate fires. The worktree remains
// because it has an uncommitted file, so the safety checks refuse removal.
//
// Acceptance: ga-xxsd7k.2 — runtime tick records reap_closed_bead_worktrees
// phase when enabled.
func TestCityRuntimeTick_AttemptsClosedBeadWorktreeReapWhenEnabled(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "rig")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, rigRoot, "init")
	runGit(t, rigRoot, "config", "user.email", "test@test.com")
	runGit(t, rigRoot, "config", "user.name", "Test")
	runGit(t, rigRoot, "commit", "--allow-empty", "-m", "init")

	wtDir := filepath.Join(cityPath, ".gc", "worktrees", "mrig", "ga-abc123")
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		t.Fatalf("creating worktree parent dir: %v", err)
	}
	runGit(t, rigRoot, "worktree", "add", "--detach", wtDir, "HEAD")
	if err := os.WriteFile(filepath.Join(wtDir, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("writing dirty file: %v", err)
	}

	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-abc123",
		Status: "closed",
	}}, nil)

	enabled := true
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Daemon:    config.DaemonConfig{AutoReapClosedBeadWorktrees: &enabled},
		Rigs:      []config.Rig{{Name: "mrig", Path: rigRoot}},
	}

	cityStore := beads.NewMemStore()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"mrig": rigStore},
		wg:                  fixedWispGC{},
		rec:                 events.Discard,
		logPrefix:           "test",
		stdout:              io.Discard,
		stderr:              &stderr,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cityPath, &prevPoolRunning, "test")

	// The reaper logs "reapClosedBeadWorktrees: skipping ..." because the
	// non-git dir is treated as dirty. This proves the gate fires when enabled.
	if !strings.Contains(stderr.String(), "reapClosedBeadWorktrees: skipping") {
		t.Errorf("stderr = %q, want reaper skipping-log proving gate fires when enabled", stderr.String())
	}
}
