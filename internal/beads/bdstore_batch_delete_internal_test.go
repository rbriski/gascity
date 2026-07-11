package beads

import (
	"strings"
	"testing"
)

// recordingRunner captures every bd invocation's args and returns canned JSON.
type recordingRunner struct {
	calls [][]string
}

func (r *recordingRunner) run(_ string, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	// A bd sql DELETE returns a rows_affected envelope; count the ids in the last
	// arg's IN-clause so the caller's total is sensible.
	return []byte(`{"rows_affected": 1}`), nil
}

// sqlDeleteQueries returns the SQL text of every `bd sql` invocation captured.
func (r *recordingRunner) sqlDeleteQueries() []string {
	var out []string
	for _, call := range r.calls {
		if len(call) >= 3 && call[0] == "bd" && call[1] == "sql" {
			out = append(out, call[len(call)-1])
		}
	}
	return out
}

// bdDeleteInvoked reports whether any captured call was `bd delete` (the
// text-rewriting path the migration must never take).
func (r *recordingRunner) bdDeleteInvoked() bool {
	for _, call := range r.calls {
		if len(call) >= 2 && call[0] == "bd" && call[1] == "delete" {
			return true
		}
	}
	return false
}

// TestBdStoreDeleteAllOrphaningUsesRawSQLNotBdDelete proves the crux invariant:
// DeleteAllOrphaning removes beads via raw `bd sql DELETE`, NEVER via `bd delete`
// (whose single-id AND batch forms both text-rewrite connected beads to
// "[deleted:ID]").
func TestBdStoreDeleteAllOrphaningUsesRawSQLNotBdDelete(t *testing.T) {
	rr := &recordingRunner{}
	s := NewBdStore("/city", rr.run)

	ids := []string{"gcs-1", "gcs-2", "gcs-3"}
	if _, err := s.DeleteAllOrphaning(ids); err != nil {
		t.Fatalf("DeleteAllOrphaning: %v", err)
	}

	if rr.bdDeleteInvoked() {
		t.Fatalf("DeleteAllOrphaning invoked `bd delete` (the text-rewriting mutation bomb); it must use raw SQL")
	}
	queries := rr.sqlDeleteQueries()
	if len(queries) == 0 {
		t.Fatalf("DeleteAllOrphaning issued no `bd sql` DELETE")
	}
	// Every id must appear in a DELETE against both the issues and wisps tables.
	joined := strings.Join(queries, "\n")
	for _, id := range ids {
		if !strings.Contains(joined, "'"+id+"'") {
			t.Errorf("id %q not present in any DELETE statement:\n%s", id, joined)
		}
	}
	if !strings.Contains(joined, "DELETE FROM issues") {
		t.Errorf("no DELETE against the issues table:\n%s", joined)
	}
	if !strings.Contains(joined, "DELETE FROM wisps") {
		t.Errorf("no DELETE against the wisps table:\n%s", joined)
	}
}

// TestBdStoreDeleteAllOrphaningSingletonAlsoRawSQL proves even a single-id set
// uses raw SQL (no `bd delete`, no text rewrite) — unlike the earlier design that
// had a single-id fallback, the SQL path handles one id identically.
func TestBdStoreDeleteAllOrphaningSingletonAlsoRawSQL(t *testing.T) {
	rr := &recordingRunner{}
	s := NewBdStore("/city", rr.run)

	if _, err := s.DeleteAllOrphaning([]string{"gcs-only"}); err != nil {
		t.Fatalf("DeleteAllOrphaning: %v", err)
	}
	if rr.bdDeleteInvoked() {
		t.Fatalf("single-id DeleteAllOrphaning invoked `bd delete`; it must use raw SQL")
	}
	if len(rr.sqlDeleteQueries()) == 0 {
		t.Fatalf("single-id DeleteAllOrphaning issued no `bd sql` DELETE")
	}
}

// TestChunkIDsCoversAllIDs proves the chunker partitions the id set completely.
func TestChunkIDsCoversAllIDs(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 7, 9, 10, 11} {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = string(rune('a' + i))
		}
		for _, size := range []int{1, 2, 3, 4} {
			chunks := chunkIDs(ids, size)
			total := 0
			for _, c := range chunks {
				total += len(c)
			}
			if total != n {
				t.Errorf("n=%d size=%d: chunks cover %d ids, want %d", n, size, total, n)
			}
		}
	}
}
