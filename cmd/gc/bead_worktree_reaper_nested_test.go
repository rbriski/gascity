package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// initPushedTestRepo creates a git repo with one commit that's already
// pushed to an "origin" remote, so HasUnpushedCommitsResult reports false —
// matching a real, safe-to-reap polecat worktree.
func initPushedTestRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "rig")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@test.com")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "commit", "--allow-empty", "-m", "init")

	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")
	runGit(t, repo, "remote", "add", "origin", bare)
	runGit(t, repo, "push", "origin", "HEAD:refs/heads/main")

	return repo
}

// TestReapClosedBeadWorktrees_NestedRoleDirs reproduces ga-s58: real
// deployments place per-bead polecat worktrees several directories below the
// rig root (rig/polecats/<agent-name>/<bead-id-slug>), not directly under
// it, because an agent's own work_dir template supplies the intermediate
// role/name segments. The reaper must find and remove these nested,
// closed-bead worktrees — not just ones living one level below the rig.
func TestReapClosedBeadWorktrees_NestedRoleDirs(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := initPushedTestRepo(t)

	// Mirrors production layout: <rig>/polecats/<agent>/<bead-id>-slug.
	wtPath := filepath.Join(cityPath, ".gc", "worktrees", "myrig", "polecats", "gastown.slit", "ga-abc123-some-title")
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, rigRoot, "worktree", "add", "--detach", wtPath, "HEAD")

	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-abc123",
		Status: "closed",
	}}, nil)

	enabled := true
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Daemon:    config.DaemonConfig{AutoReapClosedBeadWorktrees: &enabled},
		Rigs:      []config.Rig{{Name: "myrig", Path: rigRoot}},
	}

	var stderr bytes.Buffer
	reaped := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"myrig": rigStore}, events.Discard, &stderr)

	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1 (stderr: %s)", reaped, stderr.String())
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("nested worktree at %q still exists after reap", wtPath)
	}
}

// TestReapClosedBeadWorktrees_RemovesFromRigRootNotCityPath reproduces the
// second half of ga-s58: cityPath is never itself a git repository (it's the
// directory that *contains* per-rig checkouts), so invoking `git worktree
// remove` scoped to cityPath always fails with "not a git repository". The
// reaper must run worktree operations against the rig's own repo root.
func TestReapClosedBeadWorktrees_RemovesFromRigRootNotCityPath(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := initPushedTestRepo(t)

	wtPath := filepath.Join(cityPath, ".gc", "worktrees", "myrig", "ga-xyz999")
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, rigRoot, "worktree", "add", "--detach", wtPath, "HEAD")

	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-xyz999",
		Status: "closed",
	}}, nil)

	enabled := true
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Daemon:    config.DaemonConfig{AutoReapClosedBeadWorktrees: &enabled},
		Rigs:      []config.Rig{{Name: "myrig", Path: rigRoot}},
	}

	var stderr bytes.Buffer
	reaped := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"myrig": rigStore}, events.Discard, &stderr)

	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1 (stderr: %s)", reaped, stderr.String())
	}
}
