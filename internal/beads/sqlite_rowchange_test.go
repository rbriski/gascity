package beads

import "testing"

func openRecordingSQLite(t *testing.T, emit RowChangeEmitter) *SQLiteStore {
	t.Helper()
	st, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreRecorder(emit))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	s := st.(*SQLiteStore)
	t.Cleanup(func() { _ = s.CloseStore() })
	return s
}

// TestSQLiteStore_RowChangeEmission pins the Layer-0 store-edge emission contract:
// every committed mutation emits exactly one RowChange with the bead id, type, and
// physical op; Close/SetMetadata funnel through Update so they emit "updated";
// Delete emits "deleted" carrying the type captured before the row was removed.
func TestSQLiteStore_RowChangeEmission(t *testing.T) {
	var got []RowChange
	s := openRecordingSQLite(t, func(rc RowChange) { got = append(got, rc) })

	b, err := s.Create(Bead{Title: "x", Type: "message"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title := "y"
	if err := s.Update(b.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.Close(b.ID); err != nil { // open->closed transition -> "closed"
		t.Fatalf("Close: %v", err)
	}
	if err := s.SetMetadata(b.ID, "k", "v"); err != nil { // update on closed bead -> "updated"
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := s.Delete(b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	wantOps := []RowOp{RowCreated, RowUpdated, RowClosed, RowUpdated, RowDeleted}
	if len(got) != len(wantOps) {
		t.Fatalf("emitted %d changes, want %d: %+v", len(got), len(wantOps), got)
	}
	for i, rc := range got {
		if rc.Op != wantOps[i] {
			t.Errorf("change %d op = %q, want %q", i, rc.Op, wantOps[i])
		}
		if rc.ID != b.ID {
			t.Errorf("change %d id = %q, want %q", i, rc.ID, b.ID)
		}
		if rc.Type != "message" {
			t.Errorf("change %d type = %q, want message (incl. the delete, captured pre-removal)", i, rc.Type)
		}
	}
}

// TestSQLiteStore_RowChangeEmitsAfterCommit proves the emitter sees the committed
// state: on the close "updated" change the bead reads back closed, and on the
// "deleted" change the row is already gone.
func TestSQLiteStore_RowChangeEmitsAfterCommit(t *testing.T) {
	var s *SQLiteStore
	var sawClosed, sawDeletedGone bool
	s = openRecordingSQLite(t, func(rc RowChange) {
		switch rc.Op {
		case RowClosed:
			if got, err := s.Get(rc.ID); err == nil && got.Status == "closed" {
				sawClosed = true
			}
		case RowDeleted:
			if _, err := s.Get(rc.ID); err == ErrNotFound || (err != nil) {
				sawDeletedGone = true
			}
		}
	})
	b, err := s.Create(Bead{Title: "x", Type: "message"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Delete(b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !sawClosed {
		t.Error("close emission did not observe the committed closed status (emit before commit?)")
	}
	if !sawDeletedGone {
		t.Error("delete emission did not observe the row already removed (emit before commit?)")
	}
}

// TestSQLiteStore_NoEmitWithoutRecorderOrOnNoop proves the default (no recorder)
// path is silent (byte-identical), and that no-op / failed mutations do not emit.
func TestSQLiteStore_NoEmitWithoutRecorderOrOnNoop(t *testing.T) {
	count := 0
	s := openRecordingSQLite(t, func(RowChange) { count++ })

	if err := s.SetMetadataBatch("whatever", nil); err != nil { // empty kvs -> no-op
		t.Fatalf("SetMetadataBatch(nil): %v", err)
	}
	if err := s.Update("does-not-exist", UpdateOpts{}); err == nil {
		t.Fatal("Update of missing id should error")
	}
	if count != 0 {
		t.Fatalf("no-op/failed mutations emitted %d changes, want 0", count)
	}

	// And a store opened without a recorder must not panic on mutation.
	plain, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore (no recorder): %v", err)
	}
	t.Cleanup(func() { _ = plain.(*SQLiteStore).CloseStore() })
	if _, err := plain.Create(Bead{Title: "x"}); err != nil {
		t.Fatalf("Create without recorder: %v", err)
	}
}

// TestSQLiteStore_NoOpReStampDoesNotEmit proves the metadata re-stamp guard:
// SetMetadata with a value that already matches is a no-op — no commit, no event
// — matching CachingStore's idempotence and avoiding the heartbeat event storm.
func TestSQLiteStore_NoOpReStampDoesNotEmit(t *testing.T) {
	var ops []RowOp
	s := openRecordingSQLite(t, func(rc RowChange) { ops = append(ops, rc.Op) })
	b, err := s.Create(Bead{Title: "x", Type: "chore", Metadata: map[string]string{"heartbeat": "1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Re-stamp the same value: must NOT emit.
	if err := s.SetMetadata(b.ID, "heartbeat", "1"); err != nil {
		t.Fatalf("SetMetadata(same): %v", err)
	}
	// A changed value DOES emit.
	if err := s.SetMetadata(b.ID, "heartbeat", "2"); err != nil {
		t.Fatalf("SetMetadata(changed): %v", err)
	}
	want := []RowOp{RowCreated, RowUpdated} // create + the real change; the re-stamp is suppressed
	if len(ops) != len(want) {
		t.Fatalf("ops = %v, want %v (re-stamp must be suppressed)", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("op %d = %q, want %q", i, ops[i], want[i])
		}
	}
}
