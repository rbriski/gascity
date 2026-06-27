package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
