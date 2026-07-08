package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// openJournalStoreForFence opens a temp journal-backed beads.Store. NewJournalStore
// registers the control-epoch fence vocabulary, so a fence append is accepted.
func openJournalStoreForFence(t *testing.T) *beads.JournalStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "fence-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	return beads.NewJournalStore(gs)
}

func createControlBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    "control bead",
		Type:     "task",
		Metadata: map[string]string{beadmeta.ControlEpochMetadataKey: "1"},
	})
	if err != nil {
		t.Fatalf("create control bead: %v", err)
	}
	return b
}

// capsStrippedStore wraps a beads.Store and hides the journal CAS capabilities
// (AppendLogStore / ConditionalVersionStore), simulating a wrapper that dropped
// the handle forwards. It is used to prove the fence fails LOUD for a
// journal-resident bead whose store cannot serialize the write, instead of
// silently degrading to an unfenced SetMetadata. The embedded interface is
// promoted for Get/SetMetadata, but the concrete AppendLogStore /
// ConditionalVersionStore methods on *JournalStore are NOT (an interface embed
// does not promote a concrete type's non-interface methods), so the caps probes
// return (nil, false).
type capsStrippedStore struct {
	beads.Store
}

// stealHeadOnce returns a fenceAfterHead seam that, exactly once (on the first
// call), appends a competing event directly to beadID's fence stream — stealing
// the head out from under the writer that just read it, so the writer's CAS
// misses and must retry. This deterministically exercises the cross-process
// conflict/retry path from a single goroutine (the in-process fenceLocks mutex
// means two same-process goroutines never actually collide on the CAS).
func stealHeadOnce(t *testing.T, store *beads.JournalStore, beadID string) (seam func(), cleanup func()) {
	t.Helper()
	var once sync.Once
	streamID := beads.ControlEpochFenceStreamID(beadID)
	seam = func() {
		once.Do(func() {
			head, err := store.StreamHead(context.Background(), streamID)
			if err != nil {
				t.Errorf("stealHeadOnce StreamHead: %v", err)
				return
			}
			if _, err := store.AppendEvent(context.Background(), streamID, beads.ControlFenceEngine, head, 0,
				[]graphstore.JournalEvent{beads.ControlEpochFenceEvent(beadID)}); err != nil {
				t.Errorf("stealHeadOnce AppendEvent: %v", err)
			}
		})
	}
	return seam, func() { fenceAfterHead = nil }
}

// TestFenceControlWriteInertOnLegacyStore proves that on a legacy store with
// NEITHER CAS capability (no journal append AND no ConditionalMetadataStore)
// fenceControlWrite is a pure pass-through for a legacy bead id: decideAndWrite
// runs exactly once, its effect is byte-identical to a direct SetMetadata, and no
// fence machinery is engaged. This is the only remaining non-loud path in the
// now-total fence (P5.2). A bare MemStore is now ConditionalMetadataStore-capable,
// so it takes the loud CAS-loop path — the caps-absent fallback is exercised by
// hiding the capability behind noCASStore (see control_fence_legacy_test.go).
func TestFenceControlWriteInertOnLegacyStore(t *testing.T) {
	store := noCASStore{Store: beads.NewMemStore()}
	if _, ok := beads.AppendLogStoreFor(store); ok {
		t.Fatalf("store unexpectedly exposes AppendLogStore; the inert test would be meaningless")
	}
	if _, ok := beads.ConditionalMetadataStoreFor(store); ok {
		t.Fatalf("store unexpectedly exposes ConditionalMetadataStore; the caps-absent inert test would be meaningless")
	}

	b, err := store.Create(beads.Bead{
		Title: "legacy control", Type: "task",
		Metadata: map[string]string{beadmeta.ControlEpochMetadataKey: "1"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if beads.IsJournalResidentID(b.ID) {
		t.Fatalf("store minted a journal-resident id %q; the legacy inert test would be meaningless", b.ID)
	}

	calls := 0
	err = fenceControlWrite(context.Background(), store, b.ID, func(context.Context) error {
		calls++
		return store.SetMetadata(b.ID, beadmeta.ControlEpochMetadataKey, "2")
	})
	if err != nil {
		t.Fatalf("fenceControlWrite on caps-absent legacy store: %v", err)
	}
	if calls != 1 {
		t.Fatalf("decideAndWrite ran %d times, want exactly 1 (pass-through)", calls)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "2" {
		t.Fatalf("epoch = %q, want %q (write must apply unchanged)",
			got.Metadata[beadmeta.ControlEpochMetadataKey], "2")
	}
}

// TestFenceControlWriteInertOnLegacyBeadIDOverJournalStore proves the branch
// discriminator is the bead id, not the store: even on a journal-capable store, a
// bead that is not journal-resident (id lacks the gcg-j marker) takes the LEGACY
// branch and NO journal fence event is appended (the fence stream head stays 0).
// The legacy branch may still serialize + metadata-CAS on a capable store, but it
// never touches the journal append stream.
func TestFenceControlWriteInertOnLegacyBeadIDOverJournalStore(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)
	const legacyID = "bd-legacy-42" // not a gcg-j journal-resident id

	if beads.IsJournalResidentID(legacyID) {
		t.Fatalf("test id %q unexpectedly classified journal-resident", legacyID)
	}

	ran := false
	if err := fenceControlWrite(ctx, store, legacyID, func(context.Context) error { ran = true; return nil }); err != nil {
		t.Fatalf("fenceControlWrite: %v", err)
	}
	if !ran {
		t.Fatalf("write did not run for a legacy bead id (fallback must run write)")
	}
	head, err := store.StreamHead(ctx, beads.ControlEpochFenceStreamID(legacyID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head != 0 {
		t.Fatalf("fence stream head = %d, want 0 (no CAS append for a legacy bead)", head)
	}
}

// TestFenceControlWriteLoudWhenJournalResidentButNoCaps is the MED kill: a
// journal-resident id whose store does NOT expose the journal CAS capabilities
// (a wrapper dropped the forward) must fail LOUD — a transient, non-workflow-
// killing error — and must NOT run decideAndWrite as a silent unfenced write.
func TestFenceControlWriteLoudWhenJournalResidentButNoCaps(t *testing.T) {
	ctx := context.Background()
	journal := openJournalStoreForFence(t)
	b := createControlBead(t, journal)
	if !beads.IsJournalResidentID(b.ID) {
		t.Fatalf("created bead %q is not journal-resident; the journal store must mint gcg-j ids", b.ID)
	}

	stripped := capsStrippedStore{Store: journal}
	if _, ok := beads.AppendLogStoreFor(stripped); ok {
		t.Fatalf("capsStrippedStore still exposes AppendLogStore; the loud-degradation test is meaningless")
	}

	ran := false
	err := fenceControlWrite(ctx, stripped, b.ID, func(context.Context) error {
		ran = true
		return stripped.SetMetadata(b.ID, beadmeta.ControlEpochMetadataKey, "9")
	})
	if err == nil {
		t.Fatalf("fenceControlWrite silently degraded to unfenced write for a journal-resident bead; want loud error")
	}
	if !errors.Is(err, errControlFenceUncapped) {
		t.Fatalf("err = %v, want errControlFenceUncapped", err)
	}
	if !IsTransientControllerError(err) {
		t.Fatalf("uncapped fence error must classify transient so the workflow is not closed; IsTransientControllerError=false for %v", err)
	}
	if ran {
		t.Fatalf("decideAndWrite ran despite missing caps — that is exactly the silent unfenced write we must refuse")
	}
	got, err := journal.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "1" {
		t.Fatalf("epoch = %q, want unchanged 1 (no unfenced write may have landed)", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
}

// TestFenceControlWriteConcurrentBothRunSerialized is the S0.4 kill, proven: two
// concurrent writers fence one journal-resident control bead. BOTH decideAndWrite
// run (serialized by the fence — fenceLocks makes their read-compare-write
// mutually exclusive) — NEITHER returns an error, and neither reaches a
// workflow-closing path. Each writer appends exactly one fence event, so the
// stream advances by two, and the surviving epoch is one of the two writers'
// values (never a torn interleave).
func TestFenceControlWriteConcurrentBothRunSerialized(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)
	b := createControlBead(t, store)
	if !beads.IsJournalResidentID(b.ID) {
		t.Fatalf("created bead %q is not journal-resident; the journal store must mint gcg-j ids", b.ID)
	}

	vals := [2]string{"epoch-A", "epoch-B"}
	var runs int32
	var mu sync.Mutex
	ran := map[string]bool{}
	results := [2]error{}
	var done sync.WaitGroup
	done.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer done.Done()
			results[i] = fenceControlWrite(ctx, store, b.ID, func(context.Context) error {
				mu.Lock()
				ran[vals[i]] = true
				mu.Unlock()
				atomic.AddInt32(&runs, 1)
				return store.SetMetadata(b.ID, beadmeta.ControlEpochMetadataKey, vals[i])
			})
		}()
	}
	done.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("writer %d returned %v, want nil (loser must retry, not error)", i, err)
		}
	}
	if runs != 2 {
		t.Fatalf("decideAndWrite ran %d times, want 2 (both writers serialized, neither dropped)", runs)
	}
	if !ran[vals[0]] || !ran[vals[1]] {
		t.Fatalf("both writers' decideAndWrite must have run; ran=%v", ran)
	}

	// The fence stream advanced by exactly two — one append per serialized writer.
	head, err := store.StreamHead(ctx, beads.ControlEpochFenceStreamID(b.ID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head != 2 {
		t.Fatalf("fence stream head = %d, want 2 (one CAS append per serialized writer)", head)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v := got.Metadata[beadmeta.ControlEpochMetadataKey]; v != vals[0] && v != vals[1] {
		t.Fatalf("final epoch = %q, want one of %v (no torn interleave)", v, vals)
	}
}

// TestFenceControlWriteStaggeredRegressionKilled proves the HIGH regression is
// gone: two writers race, one intending to advance the epoch to 5 and one to 4,
// each using the real decide-inside-the-fence logic (advance only if strictly
// greater). The final epoch is 5 and is NEVER regressed to 4 — whichever writer
// runs second re-reads the epoch inside the serialized section: if the 4-writer
// runs second it sees 5 and no-ops; if the 5-writer runs second it sees 4 and
// advances to 5. Either serialization order converges on 5. Neither writer errors.
func TestFenceControlWriteStaggeredRegressionKilled(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)
	b := createControlBead(t, store) // epoch starts at 1

	// advanceTo mirrors syncControlEpochToAttempt's decideAndWrite: re-read the
	// current epoch, advance only when the target is strictly greater.
	advanceTo := func(target int) func(context.Context) error {
		return func(context.Context) error {
			fresh, err := store.Get(b.ID)
			if err != nil {
				return err
			}
			cur, _ := strconv.Atoi(fresh.Metadata[beadmeta.ControlEpochMetadataKey])
			if target <= cur {
				return nil
			}
			return store.SetMetadata(b.ID, beadmeta.ControlEpochMetadataKey, strconv.Itoa(target))
		}
	}

	targets := [2]int{5, 4}
	results := [2]error{}
	var done sync.WaitGroup
	done.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer done.Done()
			results[i] = fenceControlWrite(ctx, store, b.ID, advanceTo(targets[i]))
		}()
	}
	done.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("writer %d (target %d) returned %v, want nil", i, targets[i], err)
		}
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "5" {
		t.Fatalf("final epoch = %q, want 5 (the 4-writer must never regress the 5)", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
}

// TestFenceControlWriteRetriesLostCASThenWrites proves the cross-process
// conflict/retry path: a competing writer steals the head between this writer's
// StreamHead read and its CAS append (stealHeadOnce), so the first append misses
// (graphstore.ErrWrongExpectedVersion). The fence must NOT error and must NOT
// drop the write — it re-reads the head, appends behind the thief, and runs
// decideAndWrite exactly once. The stream ends at 2 (thief + writer).
func TestFenceControlWriteRetriesLostCASThenWrites(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)
	b := createControlBead(t, store) // epoch starts at 1

	seam, cleanup := stealHeadOnce(t, store, b.ID)
	fenceAfterHead = seam
	t.Cleanup(cleanup)

	runs := 0
	err := fenceControlWrite(ctx, store, b.ID, func(context.Context) error {
		runs++
		return store.SetMetadata(b.ID, beadmeta.ControlEpochMetadataKey, "2")
	})
	if err != nil {
		t.Fatalf("fenceControlWrite must retry a lost CAS, not error: %v", err)
	}
	if runs != 1 {
		t.Fatalf("decideAndWrite ran %d times, want exactly 1 (the retry re-appends, it does not re-run the write twice)", runs)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "2" {
		t.Fatalf("epoch = %q, want 2 (write applied after retry)", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
	head, err := store.StreamHead(ctx, beads.ControlEpochFenceStreamID(b.ID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head != 2 {
		t.Fatalf("fence stream head = %d, want 2 (thief's append + writer's retried append)", head)
	}
}

// TestFenceControlWriteCrashWindowRedo proves the two-transaction crash window is
// convergent: the fence append commits but the SetMetadata is skipped (simulating
// a crash between the two transactions); a redo re-appends a harmless second
// fence event and this time completes the write, converging on the correct epoch.
func TestFenceControlWriteCrashWindowRedo(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)
	b := createControlBead(t, store)

	// First pass: acquire the slot but skip the write (the "crash").
	if err := fenceControlWrite(ctx, store, b.ID, func(context.Context) error {
		return nil // append committed, epoch metadata left stale
	}); err != nil {
		t.Fatalf("first (crash) pass: %v", err)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "1" {
		t.Fatalf("epoch after crash pass = %q, want still 1", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
	head, err := store.StreamHead(ctx, beads.ControlEpochFenceStreamID(b.ID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head != 1 {
		t.Fatalf("fence stream head after crash pass = %d, want 1", head)
	}

	// Redo: re-append (head advances to 2, harmless) and complete the write.
	if err := fenceControlWrite(ctx, store, b.ID, func(context.Context) error {
		return store.SetMetadata(b.ID, beadmeta.ControlEpochMetadataKey, "2")
	}); err != nil {
		t.Fatalf("redo pass: %v", err)
	}
	got, err = store.Get(b.ID)
	if err != nil {
		t.Fatalf("get after redo: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "2" {
		t.Fatalf("epoch after redo = %q, want 2 (redo converges)", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
	head, err = store.StreamHead(ctx, beads.ControlEpochFenceStreamID(b.ID))
	if err != nil {
		t.Fatalf("StreamHead after redo: %v", err)
	}
	if head != 2 {
		t.Fatalf("fence stream head after redo = %d, want 2 (redo appended a harmless second event)", head)
	}
}

// TestSyncControlEpochToAttemptIsFenced proves site 1 (syncControlEpochToAttempt)
// actually routes its epoch write through the fence: over a journal-resident
// control bead the epoch syncs to the recovered attempt's number AND the bead's
// control-epoch fence stream advances by one. If the fence wiring were deleted
// (a direct SetMetadata), the write would still land but the fence stream would
// stay at 0 — so this assertion fails closed on an unfenced site.
func TestSyncControlEpochToAttemptIsFenced(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)
	control := createControlBead(t, store) // gc.control_epoch = 1
	if !beads.IsJournalResidentID(control.ID) {
		t.Fatalf("control bead %q is not journal-resident", control.ID)
	}
	attempt, err := store.Create(beads.Bead{
		Title:    "recovered attempt",
		Type:     "task",
		Metadata: map[string]string{beadmeta.AttemptMetadataKey: "2"},
	})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	if err := syncControlEpochToAttempt(store, control, attempt); err != nil {
		t.Fatalf("syncControlEpochToAttempt: %v", err)
	}

	got, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "2" {
		t.Fatalf("synced epoch = %q, want 2", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
	head, err := store.StreamHead(ctx, beads.ControlEpochFenceStreamID(control.ID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head != 1 {
		t.Fatalf("fence stream head = %d, want 1 (site must route the epoch write through the fence)", head)
	}
}

// TestFenceControlWriteUncontendedJournalBead proves the fence is transparent for
// a single uncontended writer on a journal-resident bead: the write applies and
// the stream advances by one (the fence is a no-op cost when there is no race).
func TestFenceControlWriteUncontendedJournalBead(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)
	b := createControlBead(t, store)

	if err := fenceControlWrite(ctx, store, b.ID, func(context.Context) error {
		return store.SetMetadata(b.ID, beadmeta.ControlEpochMetadataKey, "2")
	}); err != nil {
		t.Fatalf("fenceControlWrite: %v", err)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "2" {
		t.Fatalf("epoch = %q, want 2", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
	head, err := store.StreamHead(ctx, beads.ControlEpochFenceStreamID(b.ID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head != 1 {
		t.Fatalf("fence stream head = %d, want 1", head)
	}
}

// TestSpawnNextAttemptIsFenced pins the highest-traffic epoch-write site: the
// molecule.Attach{Fence: fenceControlWrite} wiring in spawnNextAttempt. Driving
// a real spawn for a journal-resident control bead must route its epoch bump
// through the fence, so the control bead's control-epoch fence stream advances.
// If the Fence wiring at spawnNextAttempt were deleted, molecule.Attach would
// bump the epoch UNFENCED (runFenced runs the write directly with a nil fence)
// and the fence stream would stay at 0 — so this fails closed on an unwired
// spawn path, mirroring TestSyncControlEpochToAttemptIsFenced for site 1.
func TestSpawnNextAttemptIsFenced(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)

	root, err := store.Create(beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	spec := &formula.Step{ID: "work", Title: "Work step", Type: "task"}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal step spec: %v", err)
	}
	control, err := store.Create(beads.Bead{
		Title: "retry control",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:           beadmeta.KindRetry,
			beadmeta.RootBeadIDMetadataKey:     root.ID,
			beadmeta.StepRefMetadataKey:        "mol-spawn.work",
			beadmeta.StepIDMetadataKey:         "work",
			beadmeta.SourceStepSpecMetadataKey: string(specJSON),
			beadmeta.ControlEpochMetadataKey:   "1",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	if !beads.IsJournalResidentID(control.ID) {
		t.Fatalf("control bead %q is not journal-resident; the fenced-spawn assertion would be meaningless", control.ID)
	}

	if err := spawnNextAttempt(ctx, store, control, 2, ProcessOptions{}); err != nil {
		t.Fatalf("spawnNextAttempt: %v", err)
	}

	// The epoch bump must have routed through the fence: the stream advanced.
	head, err := store.StreamHead(ctx, beads.ControlEpochFenceStreamID(control.ID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head == 0 {
		t.Fatalf("fence stream head = 0, want > 0 (spawnNextAttempt must route its epoch bump through the fence; the Fence wiring is unwired)")
	}
	// And the epoch actually advanced (Attach bumps ExpectedEpoch 1 -> 2 inside the fence).
	got, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "2" {
		t.Fatalf("control epoch = %q, want 2 (Attach bumps the epoch inside the fence)", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
}

// stealHeadEvery returns a fenceAfterHead seam that, on EVERY call, appends a
// competing event to beadID's fence stream — stealing the head out from under
// the writer on every loop iteration, so the writer's CAS misses every time and
// the optimistic-retry loop exhausts its budget. Where stealHeadOnce (a single
// theft) exercises one retry, this drives fenceControlWrite all the way to its
// errControlFenceContended budget-exhaustion return. It appends via the store's
// own CAS at the fresh head, so it always wins the slot the writer just read.
// Not parallel-safe: it drives the fenceAfterHead global (do not add t.Parallel).
func stealHeadEvery(t *testing.T, store *beads.JournalStore, beadID string) (seam func(), cleanup func()) {
	t.Helper()
	streamID := beads.ControlEpochFenceStreamID(beadID)
	seam = func() {
		head, err := store.StreamHead(context.Background(), streamID)
		if err != nil {
			t.Errorf("stealHeadEvery StreamHead: %v", err)
			return
		}
		if _, err := store.AppendEvent(context.Background(), streamID, beads.ControlFenceEngine, head, 0,
			[]graphstore.JournalEvent{beads.ControlEpochFenceEvent(beadID)}); err != nil {
			t.Errorf("stealHeadEvery AppendEvent: %v", err)
		}
	}
	return seam, func() { fenceAfterHead = nil }
}

// TestFenceControlWriteExhaustsBudgetUnderPerpetualContention drives the
// optimistic-retry loop to its bound (control_fence.go:148): a competitor steals
// the fence head on EVERY iteration (stealHeadEvery), so the writer's CAS misses
// all controlFenceMaxAttempts times and fenceControlWrite returns
// errControlFenceContended. This is the only path that reaches the
// budget-exhaustion return. decideAndWrite must never run (the epoch is
// untouched — a loser never writes), and the returned error must classify
// TRANSIENT so the dispatcher re-dispatches the control bead rather than closing
// its workflow.
func TestFenceControlWriteExhaustsBudgetUnderPerpetualContention(t *testing.T) {
	ctx := context.Background()
	store := openJournalStoreForFence(t)
	b := createControlBead(t, store) // epoch starts at 1

	seam, cleanup := stealHeadEvery(t, store, b.ID)
	fenceAfterHead = seam
	t.Cleanup(cleanup)

	runs := 0
	err := fenceControlWrite(ctx, store, b.ID, func(context.Context) error {
		runs++
		return store.SetMetadata(b.ID, beadmeta.ControlEpochMetadataKey, "2")
	})
	if err == nil {
		t.Fatalf("fenceControlWrite returned nil, want errControlFenceContended after exhausting the retry budget")
	}
	if !errors.Is(err, errControlFenceContended) {
		t.Fatalf("err = %v, want errControlFenceContended", err)
	}
	if runs != 0 {
		t.Fatalf("decideAndWrite ran %d times, want 0 (a CAS loser must never write; the budget exhausts before decideAndWrite)", runs)
	}
	if !IsTransientControllerError(err) {
		t.Fatalf("contended fence error must classify transient so the workflow re-dispatches, not closes; IsTransientControllerError=false for %v", err)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[beadmeta.ControlEpochMetadataKey] != "1" {
		t.Fatalf("epoch = %q, want unchanged 1 (no write may land when the budget is exhausted)", got.Metadata[beadmeta.ControlEpochMetadataKey])
	}
	// The competitor appended exactly once per attempt — the writer never landed a slot.
	head, err := store.StreamHead(ctx, beads.ControlEpochFenceStreamID(b.ID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head != controlFenceMaxAttempts {
		t.Fatalf("fence stream head = %d, want %d (competitor stole the head on every attempt; writer landed none)", head, controlFenceMaxAttempts)
	}
}
