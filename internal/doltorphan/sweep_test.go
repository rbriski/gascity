package doltorphan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func mustChtimes(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes(%q): %v", path, err)
	}
}

// markedCandidate creates dir with a .dolt marker at the given depth below
// dir (depth 1 == dir/.dolt, depth 2 == dir/x/.dolt, depth 3 ==
// dir/x/y/.dolt) and sets dir's own mtime to now.Add(-age).
func markedCandidate(t *testing.T, root, name string, age time.Duration, now time.Time, depth int) string {
	t.Helper()
	dir := filepath.Join(root, name)
	markerParent := dir
	for i := 1; i < depth; i++ {
		markerParent = filepath.Join(markerParent, "d"+string(rune('0'+i)))
	}
	mustMkdirAll(t, filepath.Join(markerParent, ".dolt"))
	mustChtimes(t, dir, now.Add(-age))
	return dir
}

func noopLsof() (string, error) { return "", nil }

func TestSweep_RemovesOldMarkedUnheldCandidate(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := markedCandidate(t, root, "leaked-1", 20*time.Minute, now, 1)

	res := Sweep(SweepConfig{
		Roots:   []string{root},
		MinAge:  10 * time.Minute,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
	})

	if len(res.Removed) != 1 || res.Removed[0] != candidate {
		t.Fatalf("Removed = %v, want [%s]", res.Removed, candidate)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("candidate still exists after sweep, stat err = %v", err)
	}
	if len(res.RemoveErrors) != 0 {
		t.Fatalf("RemoveErrors = %v, want none", res.RemoveErrors)
	}
}

func TestSweep_SparesCandidateYoungerThanMinAge(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := markedCandidate(t, root, "fresh-1", 5*time.Minute, now, 1)

	res := Sweep(SweepConfig{
		Roots:   []string{root},
		MinAge:  10 * time.Minute,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
	})

	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", res.Removed)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate should still exist: %v", err)
	}
}

func TestSweep_ZeroMinAgeReapsFreshCandidate(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := markedCandidate(t, root, "fresh-2", time.Second, now, 1)

	res := Sweep(SweepConfig{
		Roots:   []string{root},
		MinAge:  0,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
	})

	if len(res.Removed) != 1 || res.Removed[0] != candidate {
		t.Fatalf("Removed = %v, want [%s] (MinAge=0 means any age qualifies)", res.Removed, candidate)
	}
}

func TestSweep_SparesCandidateWithoutMarker(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(root, "no-marker")
	mustMkdirAll(t, dir)
	mustChtimes(t, dir, now.Add(-time.Hour))

	res := Sweep(SweepConfig{
		Roots:   []string{root},
		MinAge:  10 * time.Minute,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
	})

	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (no .dolt marker)", res.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("candidate should still exist: %v", err)
	}
}

func TestSweep_MarkerFoundAtEachDepthUpToThree(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	var candidates []string
	for depth := 1; depth <= 3; depth++ {
		candidates = append(candidates, markedCandidate(t, root, "depth"+string(rune('0'+depth)), time.Hour, now, depth))
	}

	res := Sweep(SweepConfig{
		Roots:   []string{root},
		MinAge:  10 * time.Minute,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
	})

	if len(res.Removed) != len(candidates) {
		t.Fatalf("Removed = %v, want all of %v", res.Removed, candidates)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); !os.IsNotExist(err) {
			t.Errorf("candidate %s still exists after sweep", c)
		}
	}
}

func TestSweep_MarkerBeyondMaxDepthNotFound(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(root, "too-deep")
	// marker at depth 4: too-deep/d1/d2/d3/.dolt
	mustMkdirAll(t, filepath.Join(dir, "d1", "d2", "d3", ".dolt"))
	mustChtimes(t, dir, now.Add(-time.Hour))

	res := Sweep(SweepConfig{
		Roots:   []string{root},
		MinAge:  10 * time.Minute,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
	})

	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (.dolt marker is beyond maxdepth 3)", res.Removed)
	}
}

func TestSweep_SparesCandidateHeldOpenByLsof(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	held := markedCandidate(t, root, "held", time.Hour, now, 1)
	free := markedCandidate(t, root, "free", time.Hour, now, 1)

	lsofOutput := "dolt    1234 user   12u   REG  8,1  4096 999 " + filepath.Join(held, "noms", "chunks", "0") + "\n"

	res := Sweep(SweepConfig{
		Roots:  []string{root},
		MinAge: 10 * time.Minute,
		Now:    func() time.Time { return now },
		RunLsof: func() (string, error) {
			return lsofOutput, nil
		},
	})

	if len(res.Removed) != 1 || res.Removed[0] != free {
		t.Fatalf("Removed = %v, want [%s] (held candidate must be spared)", res.Removed, free)
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatalf("held candidate should still exist: %v", err)
	}
}

func TestSweep_FailsClosedWhenLsofErrors(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	candidate := markedCandidate(t, root, "leaked-2", time.Hour, now, 1)

	res := Sweep(SweepConfig{
		Roots:  []string{root},
		MinAge: 10 * time.Minute,
		Now:    func() time.Time { return now },
		RunLsof: func() (string, error) {
			return "", errors.New("lsof: command not found")
		},
	})

	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (must fail closed when lsof errors)", res.Removed)
	}
	if !res.LsofUnavailable {
		t.Fatalf("LsofUnavailable = false, want true")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate should still exist: %v", err)
	}
}

func TestSweep_EmptyRootsNeverCallsLsof(t *testing.T) {
	res := Sweep(SweepConfig{
		Roots:  nil,
		MinAge: 10 * time.Minute,
		Now:    time.Now,
		RunLsof: func() (string, error) {
			t.Fatal("RunLsof should not be called when there are no candidates")
			return "", nil
		},
	})
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", res.Removed)
	}
}

func TestSweep_UnreadableRootSkippedGracefully(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	missing := filepath.Join(root, "does-not-exist")

	res := Sweep(SweepConfig{
		Roots:   []string{missing},
		MinAge:  10 * time.Minute,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
	})

	if len(res.Removed) != 0 || len(res.RemoveErrors) != 0 {
		t.Fatalf("res = %+v, want zero value for an unreadable root", res)
	}
}

func TestSweep_RemoveAllErrorRecordedNotFatal(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	failing := markedCandidate(t, root, "fails-to-remove", time.Hour, now, 1)
	ok := markedCandidate(t, root, "removable", time.Hour, now, 1)

	boom := errors.New("boom")
	res := Sweep(SweepConfig{
		Roots:   []string{root},
		MinAge:  10 * time.Minute,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
		RemoveAll: func(path string) error {
			if path == failing {
				return boom
			}
			return os.RemoveAll(path)
		},
	})

	if len(res.Removed) != 1 || res.Removed[0] != ok {
		t.Fatalf("Removed = %v, want [%s]", res.Removed, ok)
	}
	if len(res.RemoveErrors) != 1 || res.RemoveErrors[0].Path != failing || !errors.Is(res.RemoveErrors[0].Err, boom) {
		t.Fatalf("RemoveErrors = %+v, want one entry for %s wrapping %v", res.RemoveErrors, failing, boom)
	}
}

func TestSweep_MultipleRoots(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	a := markedCandidate(t, rootA, "a", time.Hour, now, 1)
	b := markedCandidate(t, rootB, "b", time.Hour, now, 1)

	res := Sweep(SweepConfig{
		Roots:   []string{rootA, rootB},
		MinAge:  10 * time.Minute,
		Now:     func() time.Time { return now },
		RunLsof: noopLsof,
	})

	if len(res.Removed) != 2 {
		t.Fatalf("Removed = %v, want both %s and %s", res.Removed, a, b)
	}
}
