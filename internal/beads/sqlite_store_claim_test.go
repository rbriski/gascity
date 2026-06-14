package beads

import (
	"errors"
	"sync"
	"testing"
)

func TestSQLiteClaimSetsInProgressAndAssignee(t *testing.T) {
	s := openTestSQLiteStore(t)
	created, err := s.Create(Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, ok, err := s.Claim(created.ID, "w1")
	if err != nil || !ok {
		t.Fatalf("Claim = (%+v, %v, %v), want ok", got, ok, err)
	}
	if got.Assignee != "w1" || got.Status != "in_progress" {
		t.Fatalf("claimed bead = assignee %q status %q, want w1/in_progress", got.Assignee, got.Status)
	}
	persisted, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Assignee != "w1" || persisted.Status != "in_progress" {
		t.Fatalf("persisted = assignee %q status %q, want w1/in_progress", persisted.Assignee, persisted.Status)
	}
}

func TestSQLiteClaimIdempotentForSameAssignee(t *testing.T) {
	s := openTestSQLiteStore(t)
	created, _ := s.Create(Bead{Title: "work", Type: "task"})
	if _, ok, err := s.Claim(created.ID, "w1"); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.Claim(created.ID, "w1"); err != nil || !ok {
		t.Fatalf("re-claim by same assignee must be idempotent ok: ok=%v err=%v", ok, err)
	}
}

func TestSQLiteClaimConflictForDifferentAssignee(t *testing.T) {
	s := openTestSQLiteStore(t)
	created, _ := s.Create(Bead{Title: "work", Type: "task"})
	if _, ok, err := s.Claim(created.ID, "w1"); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	got, ok, err := s.Claim(created.ID, "w2")
	if err != nil {
		t.Fatalf("conflict must not error: %v", err)
	}
	if ok {
		t.Fatal("second claim by a different assignee = ok, want conflict (false)")
	}
	if got.ID != "" {
		t.Fatalf("conflict should return zero bead, got %+v", got)
	}
}

func TestSQLiteClaimNotFound(t *testing.T) {
	s := openTestSQLiteStore(t)
	if _, ok, err := s.Claim("does-not-exist", "w1"); ok || !errors.Is(err, ErrNotFound) {
		t.Fatalf("Claim(missing) = (ok=%v, err=%v), want ErrNotFound", ok, err)
	}
}

func TestSQLiteClaimSingleWinnerUnderConcurrency(t *testing.T) {
	s := openTestSQLiteStore(t)
	created, _ := s.Create(Bead{Title: "contended", Type: "task"})

	const racers = 16
	var wg sync.WaitGroup
	wins := make([]bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// distinct assignee per racer so exactly one can win
			_, ok, err := s.Claim(created.ID, assigneeName(i))
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			wins[i] = ok
		}(i)
	}
	wg.Wait()

	won := 0
	winner := -1
	for i, w := range wins {
		if w {
			won++
			winner = i
		}
	}
	if won != 1 {
		t.Fatalf("expected exactly one winner, got %d", won)
	}
	final, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Assignee != assigneeName(winner) {
		t.Fatalf("final assignee %q != winner %q", final.Assignee, assigneeName(winner))
	}
}

func TestSQLiteClaimReleaseRoundTrip(t *testing.T) {
	s := openTestSQLiteStore(t)
	created, _ := s.Create(Bead{Title: "work", Type: "task"})
	if _, ok, err := s.Claim(created.ID, "w1"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	released, err := s.ReleaseIfCurrent(created.ID, "w1")
	if err != nil || !released {
		t.Fatalf("ReleaseIfCurrent: released=%v err=%v", released, err)
	}
	// After release the bead is claimable again, by anyone.
	if _, ok, err := s.Claim(created.ID, "w2"); err != nil || !ok {
		t.Fatalf("re-claim after release: ok=%v err=%v", ok, err)
	}
}

func assigneeName(i int) string {
	return "worker-" + string(rune('a'+i))
}
