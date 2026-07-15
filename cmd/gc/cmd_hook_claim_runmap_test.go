package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSanitizeRunMapKey(t *testing.T) {
	cases := map[string]string{
		"gc__review-synthesizer-mc-1kkqd": "gc__review-synthesizer-mc-1kkqd",
		"keep.dot_under-dash":             "keep.dot_under-dash",
		"a/b c":                           "a_b_c",
		"actor@beads.test":                "actor_beads.test",
		"x:y|z":                           "x_y_z",
	}
	for in, want := range cases {
		if got := sanitizeRunMapKey(in); got != want {
			t.Errorf("sanitizeRunMapKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteRunMapWritesPerKeyAtomically(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_RUNMAP_DIR", dir)

	// duplicate sanitized key ("sess/name" twice), an empty key, and a distinct key
	writeRunMap("run-123", "bead-456", "sess/name", "sess/name", "", "actor@x")

	for _, stem := range []string{"sess_name", "actor_x"} {
		raw, err := os.ReadFile(filepath.Join(dir, stem+".json"))
		if err != nil {
			t.Fatalf("expected %s.json: %v", stem, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("bad json in %s.json: %v", stem, err)
		}
		if m["run_id"] != "run-123" {
			t.Errorf("%s.json run_id = %v, want run-123", stem, m["run_id"])
		}
		if m["bead_id"] != "bead-456" {
			t.Errorf("%s.json bead_id = %v, want bead-456", stem, m["bead_id"])
		}
	}

	// no leftover .tmp files (atomic rename completed)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestWriteRunMapEmptyRunIDNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_RUNMAP_DIR", dir)
	writeRunMap("", "bead-456", "sess")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("empty runID should write nothing, got %d entries", len(entries))
	}
}

func TestWriteRunMapHonorsDirOverride(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "runmap")
	t.Setenv("GC_RUNMAP_DIR", sub)
	writeRunMap("run-9", "bead-9", "only")
	if _, err := os.Stat(filepath.Join(sub, "only.json")); err != nil {
		t.Fatalf("expected override dir to be created and written: %v", err)
	}
}

func TestRunMapTTL(t *testing.T) {
	t.Setenv("GC_RUNMAP_TTL", "")
	if got := runMapTTL(); got != 48*time.Hour {
		t.Errorf("default runMapTTL = %v, want 48h", got)
	}
	t.Setenv("GC_RUNMAP_TTL", "2h")
	if got := runMapTTL(); got != 2*time.Hour {
		t.Errorf("override runMapTTL = %v, want 2h", got)
	}
	// A bad/zero duration falls back to the default rather than disabling reaping.
	t.Setenv("GC_RUNMAP_TTL", "garbage")
	if got := runMapTTL(); got != 48*time.Hour {
		t.Errorf("bad GC_RUNMAP_TTL should fall back to 48h, got %v", got)
	}
}

func TestPruneRunMapReapsStaleKeepsFresh(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.json")
	stale := filepath.Join(dir, "stale.json")
	notJSON := filepath.Join(dir, "keep.txt") // non-.json is never touched
	for _, f := range []string{fresh, stale, notJSON} {
		if err := os.WriteFile(f, []byte(`{"run_id":"r"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	old := now.Add(-100 * time.Hour)
	for _, f := range []string{stale, notJSON} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	pruneRunMap(dir, now, 48*time.Hour)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale .json should be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh .json should be kept: %v", err)
	}
	if _, err := os.Stat(notJSON); err != nil {
		t.Errorf("non-.json file should never be pruned: %v", err)
	}
}

func TestWriteRunMapPrunesStaleOnWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_RUNMAP_DIR", dir)
	t.Setenv("GC_RUNMAP_TTL", "1h")
	stale := filepath.Join(dir, "dead-session.json")
	if err := os.WriteFile(stale, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	writeRunMap("run-x", "bead-x", "live-session")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file should be pruned on write, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "live-session.json")); err != nil {
		t.Errorf("live session's file should be written: %v", err)
	}
}
