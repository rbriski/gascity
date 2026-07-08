package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

// P5.2 — fence totality on the LEGACY (non-journal-resident) control path.
//
// These tests pin the S0.4 kill for legacy v1/v2 control-epoch writes: the fence's
// legacy branch is now lock + bounded store-CAS retry + decide-inside-the-fence,
// structurally identical to the journal branch, so a lost race is LOUD
// (ErrMetadataCASConflict) and convergent instead of a silent overwrite. The only
// remaining non-loud path is a store that supports NEITHER CAS capability.

// noCASStore wraps a beads.Store and HIDES the ConditionalMetadataStore capability
// by embedding the Store interface (whose method set excludes SetMetadataIf, and
// which is not a ConditionalMetadataHandleProvider). It models the only store
// class the now-total fence cannot make loud — one that can neither append-CAS
// (journal) nor metadata-CAS — so ConditionalMetadataStoreFor returns (nil,false)
// and the legacy branch takes the byte-identical raw check-then-act fallback.
type noCASStore struct{ beads.Store }

// casCall records one metadata write for spy assertions.
type casCall struct {
	id, key, expected, next string
	swapped                 bool
}

// instrumentedCASStore embeds a *MemStore (so it IS ConditionalMetadataStore-capable
// and forwards every Store method) and records each conditional (SetMetadataIf) and
// unconditional (SetMetadata) metadata write. A test uses it to prove single-writer
// byte-identity (exactly one SetMetadataIf, zero SetMetadata on the epoch key) and
// conflict LOUDNESS (a swapped=false was actually observed — no silent overwrite).
// beforeFirstIf, if set, runs exactly once immediately before the first
// SetMetadataIf's real CAS: the deterministic cross-process racer that advances the
// epoch out from under the writer, forcing the CAS to miss.
type instrumentedCASStore struct {
	*beads.MemStore
	mu            sync.Mutex
	ifCalls       []casCall
	setCalls      []casCall
	injectOnce    sync.Once
	beforeFirstIf func()
}

func (s *instrumentedCASStore) SetMetadataIf(ctx context.Context, id, key, expected, next string) (bool, error) {
	s.injectOnce.Do(func() {
		if s.beforeFirstIf != nil {
			s.beforeFirstIf()
		}
	})
	swapped, err := s.MemStore.SetMetadataIf(ctx, id, key, expected, next)
	s.mu.Lock()
	s.ifCalls = append(s.ifCalls, casCall{id, key, expected, next, swapped})
	s.mu.Unlock()
	return swapped, err
}

func (s *instrumentedCASStore) SetMetadata(id, key, value string) error {
	s.mu.Lock()
	s.setCalls = append(s.setCalls, casCall{id: id, key: key, next: value})
	s.mu.Unlock()
	return s.MemStore.SetMetadata(id, key, value)
}

// epochIfCalls returns the recorded SetMetadataIf calls for the control-epoch key.
func (s *instrumentedCASStore) epochIfCalls() []casCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []casCall
	for _, c := range s.ifCalls {
		if c.key == beadmeta.ControlEpochMetadataKey {
			out = append(out, c)
		}
	}
	return out
}

// epochSetCalls returns the recorded unconditional SetMetadata calls for the
// control-epoch key. On a capable store the fence must never take this path.
func (s *instrumentedCASStore) epochSetCalls() []casCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []casCall
	for _, c := range s.setCalls {
		if c.key == beadmeta.ControlEpochMetadataKey {
			out = append(out, c)
		}
	}
	return out
}

// countConflicts reports how many recorded epoch CAS attempts LOST the race
// (swapped=false). A nonzero count proves the write was loud, not a silent
// overwrite — the totality property.
func (s *instrumentedCASStore) countConflicts() int {
	n := 0
	for _, c := range s.epochIfCalls() {
		if !c.swapped {
			n++
		}
	}
	return n
}

// alwaysConflictStore is a capable legacy store whose CAS NEVER succeeds: every
// SetMetadataIf reports swapped=false (a perpetual cross-process contender). It
// drives the legacy fence to its retry-budget exhaustion (errControlFenceContended).
type alwaysConflictStore struct{ *beads.MemStore }

func (s *alwaysConflictStore) SetMetadataIf(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func newLegacyControlBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    "legacy control",
		Type:     "task",
		Metadata: map[string]string{beadmeta.ControlEpochMetadataKey: "1"},
	})
	if err != nil {
		t.Fatalf("create legacy control bead: %v", err)
	}
	if beads.IsJournalResidentID(b.ID) {
		t.Fatalf("store minted a journal-resident id %q; the legacy-branch test would be meaningless", b.ID)
	}
	return b
}

func getEpoch(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b.Metadata[beadmeta.ControlEpochMetadataKey]
}

// legacyAttachRecipe mirrors the molecule test's makeWorkflowRecipe: a workflow
// root step plus one child named "attempt", so molecule.Attach instantiates a real
// sub-DAG.
func legacyAttachRecipe() *formula.Recipe {
	const name = "attempt"
	return &formula.Recipe{
		Name: name,
		Steps: []formula.RecipeStep{
			{ID: name, Title: name, Type: "task", IsRoot: true, Metadata: map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			}},
			{ID: name + ".run", Title: "Step run", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: name + ".run", DependsOnID: name, Type: "parent-child"},
		},
	}
}

// setupLegacyAttachControl creates a workflow root and a legacy control child at
// epoch 1, ready for molecule.Attach over the legacy fence path.
func setupLegacyAttachControl(t *testing.T, store beads.Store) (root, control beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	control, err = store.Create(beads.Bead{
		Title: "control",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:   root.ID,
			beadmeta.ControlEpochMetadataKey: "1",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	if beads.IsJournalResidentID(control.ID) {
		t.Fatalf("control id %q is journal-resident; the legacy test is meaningless", control.ID)
	}
	return root, control
}

// --- Single-writer byte-identity (the byte-identity proof) ------------------

// TestFenceLegacySite1SingleWriterByteIdentity proves funnel A
// (syncControlEpochToAttempt) is byte-identical for one uncontended writer on a
// capable legacy store: the epoch lands at the attempt number, via EXACTLY one
// SetMetadataIf(expected=current, next=attempt) that swaps, and ZERO unconditional
// SetMetadata on the epoch key. The stored value is identical to the pre-P5
// SetMetadata(next); the only delta is the conditional form.
func TestFenceLegacySite1SingleWriterByteIdentity(t *testing.T) {
	store := &instrumentedCASStore{MemStore: beads.NewMemStore()}
	control := newLegacyControlBead(t, store)
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

	if got := getEpoch(t, store, control.ID); got != "2" {
		t.Fatalf("epoch = %q, want 2", got)
	}
	ifc := store.epochIfCalls()
	if len(ifc) != 1 {
		t.Fatalf("epoch SetMetadataIf calls = %d (%+v), want exactly 1 (single-writer byte-identity)", len(ifc), ifc)
	}
	if ifc[0] != (casCall{control.ID, beadmeta.ControlEpochMetadataKey, "1", "2", true}) {
		t.Fatalf("epoch CAS = %+v, want conditional 1→2 that swapped", ifc[0])
	}
	if n := len(store.epochSetCalls()); n != 0 {
		t.Fatalf("epoch SetMetadata calls = %d, want 0 (capable store must use the CAS, not the fallback)", n)
	}
}

// TestFenceLegacySite2SingleWriterByteIdentity proves funnel B (molecule.Attach's
// post-instantiate bump, molecule.go:232) is byte-identical for one uncontended
// writer on a capable legacy store driven through the real fence: the epoch bumps
// ExpectedEpoch→ExpectedEpoch+1 via EXACTLY one SetMetadataIf(expected, next) that
// swaps, and ZERO unconditional epoch SetMetadata.
func TestFenceLegacySite2SingleWriterByteIdentity(t *testing.T) {
	store := &instrumentedCASStore{MemStore: beads.NewMemStore()}
	_, control := setupLegacyAttachControl(t, store)

	_, err := molecule.Attach(context.Background(), store, legacyAttachRecipe(), control.ID, molecule.AttachOptions{
		ExpectedEpoch: 1,
		Fence:         fenceControlWrite,
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if got := getEpoch(t, store, control.ID); got != "2" {
		t.Fatalf("epoch = %q, want 2", got)
	}
	ifc := store.epochIfCalls()
	if len(ifc) != 1 {
		t.Fatalf("epoch SetMetadataIf calls = %d (%+v), want exactly 1", len(ifc), ifc)
	}
	if ifc[0] != (casCall{control.ID, beadmeta.ControlEpochMetadataKey, "1", "2", true}) {
		t.Fatalf("epoch CAS = %+v, want conditional 1→2 that swapped", ifc[0])
	}
	if n := len(store.epochSetCalls()); n != 0 {
		t.Fatalf("epoch SetMetadata calls = %d, want 0", n)
	}
}

// TestFenceLegacySite3DuplicatePathBumpsThroughFence proves funnel B via the
// duplicate/idempotent path (advanceAttachEpochIfNeeded → bumpEpochIfCurrent): a
// second Attach with a matching idempotency key finds the existing sub-DAG and
// still advances the epoch through the fence's CAS, byte-identically.
func TestFenceLegacySite3DuplicatePathBumpsThroughFence(t *testing.T) {
	store := &instrumentedCASStore{MemStore: beads.NewMemStore()}
	root, control := setupLegacyAttachControl(t, store)

	// Pre-create an existing sub-DAG root carrying the idempotency key so the next
	// Attach takes the duplicate path (findExistingAttach → advanceAttachEpochIfNeeded).
	const key = "attempt:1"
	if _, err := store.Create(beads.Bead{
		Title: "existing sub-dag root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.IdempotencyKeyMetadataKey: key,
			beadmeta.RootBeadIDMetadataKey:     root.ID,
		},
	}); err != nil {
		t.Fatalf("create existing sub-dag: %v", err)
	}

	res, err := molecule.Attach(context.Background(), store, legacyAttachRecipe(), control.ID, molecule.AttachOptions{
		ExpectedEpoch:  1,
		IdempotencyKey: key,
		Fence:          fenceControlWrite,
	})
	if err != nil {
		t.Fatalf("Attach (duplicate path): %v", err)
	}
	if !res.Duplicate {
		t.Fatalf("expected duplicate result on the idempotent path")
	}
	if got := getEpoch(t, store, control.ID); got != "2" {
		t.Fatalf("epoch = %q, want 2 (duplicate path must still advance through the fence)", got)
	}
	ifc := store.epochIfCalls()
	if len(ifc) != 1 || ifc[0] != (casCall{control.ID, beadmeta.ControlEpochMetadataKey, "1", "2", true}) {
		t.Fatalf("epoch CAS calls = %+v, want one conditional 1→2 that swapped", ifc)
	}
}

// --- Capability-absent fallback (the only remaining non-loud path) ----------

// TestFenceLegacyCapsAbsentFallbackByteIdentical proves the ONE remaining non-loud
// path: a legacy store that supports NEITHER CAS capability takes the fence's raw
// check-then-act fallback, byte-identical to the pre-P5 legacy branch — the
// decision runs exactly once, no lock/loop is observable via a CAS, and the plain
// SetMetadata write lands unchanged.
func TestFenceLegacyCapsAbsentFallbackByteIdentical(t *testing.T) {
	inner := beads.NewMemStore()
	store := noCASStore{Store: inner}
	if _, ok := beads.ConditionalMetadataStoreFor(store); ok {
		t.Fatalf("noCASStore unexpectedly exposes ConditionalMetadataStore; the fallback test is meaningless")
	}
	if _, ok := beads.AppendLogStoreFor(store); ok {
		t.Fatalf("noCASStore unexpectedly exposes AppendLogStore")
	}
	control := newLegacyControlBead(t, store)

	calls := 0
	err := fenceControlWrite(context.Background(), store, control.ID, func(ctx context.Context) error {
		calls++
		return beads.SetMetadataConditionally(ctx, store, control.ID, beadmeta.ControlEpochMetadataKey, "1", "2")
	})
	if err != nil {
		t.Fatalf("fenceControlWrite on caps-absent legacy store: %v", err)
	}
	if calls != 1 {
		t.Fatalf("decideAndWrite ran %d times, want exactly 1 (raw check-then-act fallback)", calls)
	}
	if got := getEpoch(t, store, control.ID); got != "2" {
		t.Fatalf("epoch = %q, want 2 (fallback write must apply unchanged)", got)
	}
}

// --- Totality + cross-process conflict (the S0.4 kill) ----------------------

// TestFenceLegacySite1ConflictRetriesAndAdvances proves the cross-process CAS
// conflict/retry path at funnel A: a competing writer advances the epoch out from
// under the writer between its read and its CAS (beforeFirstIf), so the first CAS
// LOSES loudly (swapped=false). The fence re-decides behind the winner, targets a
// still-higher epoch, and converges — no error, no silent loss, no regression.
func TestFenceLegacySite1ConflictRetriesAndAdvances(t *testing.T) {
	mem := beads.NewMemStore()
	store := &instrumentedCASStore{MemStore: mem}
	control := newLegacyControlBead(t, store)
	attempt5, err := store.Create(beads.Bead{Title: "attempt5", Type: "task", Metadata: map[string]string{beadmeta.AttemptMetadataKey: "5"}})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	// A concurrent controller syncs the epoch to 3 just before our CAS.
	store.beforeFirstIf = func() { _ = mem.SetMetadata(control.ID, beadmeta.ControlEpochMetadataKey, "3") }

	if err := syncControlEpochToAttempt(store, control, attempt5); err != nil {
		t.Fatalf("syncControlEpochToAttempt: %v", err)
	}

	if got := getEpoch(t, store, control.ID); got != "5" {
		t.Fatalf("final epoch = %q, want 5 (freshest valid after conflict+retry)", got)
	}
	if store.countConflicts() != 1 {
		t.Fatalf("observed %d loud CAS conflicts, want exactly 1 (the write must be loud, not silent)", store.countConflicts())
	}
	ifc := store.epochIfCalls()
	if len(ifc) != 2 || ifc[0].swapped || !ifc[1].swapped || ifc[1].expected != "3" || ifc[1].next != "5" {
		t.Fatalf("epoch CAS sequence = %+v, want [1→5 lost, 3→5 won]", ifc)
	}
}

// TestFenceLegacySite1StaggeredRegressionKilled proves the HIGH regression is gone
// on the legacy path: an attempt-4 writer races a concurrent bump to 5 (injected
// before its CAS). The 4-writer's CAS loses loudly, it re-reads 5, sees 4 <= 5, and
// NO-OPS — the epoch is never regressed to 4. Converges on 5, no error.
func TestFenceLegacySite1StaggeredRegressionKilled(t *testing.T) {
	mem := beads.NewMemStore()
	store := &instrumentedCASStore{MemStore: mem}
	control := newLegacyControlBead(t, store)
	attempt4, err := store.Create(beads.Bead{Title: "attempt4", Type: "task", Metadata: map[string]string{beadmeta.AttemptMetadataKey: "4"}})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	store.beforeFirstIf = func() { _ = mem.SetMetadata(control.ID, beadmeta.ControlEpochMetadataKey, "5") }

	if err := syncControlEpochToAttempt(store, control, attempt4); err != nil {
		t.Fatalf("syncControlEpochToAttempt: %v", err)
	}

	if got := getEpoch(t, store, control.ID); got != "5" {
		t.Fatalf("final epoch = %q, want 5 (the 4-writer must never regress the 5)", got)
	}
	if store.countConflicts() != 1 {
		t.Fatalf("observed %d loud CAS conflicts, want exactly 1", store.countConflicts())
	}
	// The retry re-decided to a no-op (4 <= 5) — no second successful CAS write.
	for _, c := range store.epochIfCalls() {
		if c.swapped {
			t.Fatalf("a CAS swapped %s→%s, want none after the conflict (loser must no-op, not write)", c.expected, c.next)
		}
	}
}

// TestFenceLegacySite2ConflictLoserNoOps proves the totality + no-double-bump at
// funnel B: a concurrent processor bumps the attach epoch 1→2 just before this
// writer's CAS. The bump loses loudly and re-reads 2 != ExpectedEpoch 1, so it
// no-ops — the epoch is not double-bumped to 3, and Attach returns success.
func TestFenceLegacySite2ConflictLoserNoOps(t *testing.T) {
	mem := beads.NewMemStore()
	store := &instrumentedCASStore{MemStore: mem}
	_, control := setupLegacyAttachControl(t, store)
	store.beforeFirstIf = func() { _ = mem.SetMetadata(control.ID, beadmeta.ControlEpochMetadataKey, "2") }

	_, err := molecule.Attach(context.Background(), store, legacyAttachRecipe(), control.ID, molecule.AttachOptions{
		ExpectedEpoch: 1,
		Fence:         fenceControlWrite,
	})
	if err != nil {
		t.Fatalf("Attach must not error on a lost epoch CAS: %v", err)
	}
	if got := getEpoch(t, store, control.ID); got != "2" {
		t.Fatalf("final epoch = %q, want 2 (loser no-ops; no double-bump to 3)", got)
	}
	if store.countConflicts() != 1 {
		t.Fatalf("observed %d loud CAS conflicts, want exactly 1 (the bump must be loud, not silent)", store.countConflicts())
	}
}

// TestFenceLegacyBudgetExhaustionIsTransient drives the legacy retry loop to its
// bound: a store whose CAS never succeeds (alwaysConflictStore) makes every attempt
// lose, so fenceControlWrite exhausts controlFenceMaxAttempts and returns
// errControlFenceContended — TRANSIENT, so the dispatcher re-dispatches rather than
// closing the workflow. No epoch write ever lands.
func TestFenceLegacyBudgetExhaustionIsTransient(t *testing.T) {
	mem := beads.NewMemStore()
	store := &alwaysConflictStore{MemStore: mem}
	control := newLegacyControlBead(t, store)

	runs := 0
	err := fenceControlWrite(context.Background(), store, control.ID, func(ctx context.Context) error {
		runs++
		return beads.SetMetadataConditionally(ctx, store, control.ID, beadmeta.ControlEpochMetadataKey, "1", "2")
	})
	if !errors.Is(err, errControlFenceContended) {
		t.Fatalf("err = %v, want errControlFenceContended after exhausting the budget", err)
	}
	if !IsTransientControllerError(err) {
		t.Fatalf("contended legacy fence error must classify transient; IsTransientControllerError=false for %v", err)
	}
	if runs != controlFenceMaxAttempts {
		t.Fatalf("decideAndWrite ran %d times, want %d (each attempt re-decides then loses the CAS)", runs, controlFenceMaxAttempts)
	}
	if got := getEpoch(t, store, control.ID); got != "1" {
		t.Fatalf("epoch = %q, want unchanged 1 (a perpetual CAS loser never writes)", got)
	}
}

// TestLegacyFenceErrorsNeverCloseWorkflow confirms the loser-never-kills-the-workflow
// constraint: both fence errors a legacy CAS loser can surface — errControlFenceContended
// (budget exhaustion) and a bubbled beads.ErrMetadataCASConflict (defense-in-depth for
// a nil-fence write) — take markControllerSpawnError's TRANSIENT branch (returns true,
// bead stays open), never the hard branch that closes the workflow.
func TestLegacyFenceErrorsNeverCloseWorkflow(t *testing.T) {
	transientErrs := map[string]error{
		"budget exhaustion":    errControlFenceContended,
		"bubbled CAS conflict": fmt.Errorf("incrementing epoch on x: %w", beads.ErrMetadataCASConflict),
	}

	for name, cerr := range transientErrs {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Title: "control", Type: "task"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			pending := markControllerSpawnError(store, b.ID, cerr, ProcessOptions{})
			if !pending {
				t.Fatalf("markControllerSpawnError returned false (hard close) for %v; a CAS loser must re-dispatch, not close", cerr)
			}
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Status == "closed" {
				t.Fatalf("bead closed on a transient fence error %v; the workflow must survive", cerr)
			}
		})
	}
}

// --- Concurrency stress (the -count=100 -race bar) --------------------------

// TestFenceLegacyInProcessRaceConverges is the S0.4 kill under real concurrency:
// two goroutines race the same legacy control bead through the fence (site 1),
// intending epochs 5 and 4. fenceLocks serializes them in-process, so whichever
// runs second re-reads inside the fence and either advances (5 after 4) or no-ops
// (4 after 5). NEITHER errors, NEITHER closes the workflow, and the epoch converges
// on 5 — never regressed, never torn. Run under -count=100 -race.
func TestFenceLegacyInProcessRaceConverges(t *testing.T) {
	store := beads.NewMemStore()
	control := newLegacyControlBead(t, store)
	attempts := [2]string{"5", "4"}
	var beads2 [2]beads.Bead
	for i, n := range attempts {
		b, err := store.Create(beads.Bead{Title: "attempt", Type: "task", Metadata: map[string]string{beadmeta.AttemptMetadataKey: n}})
		if err != nil {
			t.Fatalf("create attempt: %v", err)
		}
		beads2[i] = b
	}

	results := [2]error{}
	var done sync.WaitGroup
	done.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer done.Done()
			results[i] = syncControlEpochToAttempt(store, control, beads2[i])
		}()
	}
	done.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("writer %d (attempt %s) returned %v, want nil (loser must converge, not error)", i, attempts[i], err)
		}
	}
	if got := getEpoch(t, store, control.ID); got != "5" {
		t.Fatalf("final epoch = %q, want 5 (converged, no regression)", got)
	}
}

// --- Bonus (item 3): the P2.3 cross-process metadata-record residual, closed ---

// injectingJournalStore embeds a *JournalStore (preserving ALL its capabilities —
// AppendLogStore, ConditionalVersionStore, ConditionalMetadataStore, Store) and
// injects, exactly once before the first SetMetadataIf's real CAS, a competing
// epoch write. It simulates the cross-process writer whose separate metadata
// transaction committed between this writer winning the append slot and its epoch
// CAS — the exact window the P2.3 fence left open.
type injectingJournalStore struct {
	*beads.JournalStore
	injectOnce    sync.Once
	beforeFirstIf func()
}

func (s *injectingJournalStore) SetMetadataIf(ctx context.Context, id, key, expected, next string) (bool, error) {
	s.injectOnce.Do(func() {
		if s.beforeFirstIf != nil {
			s.beforeFirstIf()
		}
	})
	return s.JournalStore.SetMetadataIf(ctx, id, key, expected, next)
}

// TestFenceJournalBranchRetriesCrossProcessMetadataCAS proves the bonus: the
// journal branch's epoch write now goes through beads.SetMetadataConditionally
// (a store-level SetMetadataIf CAS), so a cross-process writer that commits the
// epoch metadata between this writer's re-read and its CAS makes the write a loud
// ErrMetadataCASConflict — which the journal branch RETRIES (re-append + re-decide)
// instead of silently overwriting. The former "cross-process metadata race remains
// a P2 limitation" residual is closed. If the branch did NOT retry, decideAndWrite
// would return the conflict as an error (non-nil) and the epoch would not converge.
func TestFenceJournalBranchRetriesCrossProcessMetadataCAS(t *testing.T) {
	ctx := context.Background()
	journal := openJournalStoreForFence(t)
	store := &injectingJournalStore{JournalStore: journal}
	control := createControlBead(t, store) // journal-resident, epoch "1"
	if !beads.IsJournalResidentID(control.ID) {
		t.Fatalf("control bead %q is not journal-resident", control.ID)
	}
	// A concurrent controller commits epoch 7 just before our CAS.
	store.beforeFirstIf = func() {
		_ = journal.SetMetadata(control.ID, beadmeta.ControlEpochMetadataKey, "7")
	}

	// Closure mirrors the epoch-write funnels: read current, advance to a higher
	// target via the conditional write.
	target := 9
	err := fenceControlWrite(ctx, store, control.ID, func(ctx context.Context) error {
		cur, _ := strconv.Atoi(getEpoch(t, store, control.ID))
		if target <= cur {
			return nil
		}
		return beads.SetMetadataConditionally(ctx, store, control.ID,
			beadmeta.ControlEpochMetadataKey, strconv.Itoa(cur), strconv.Itoa(target))
	})
	if err != nil {
		t.Fatalf("journal-branch fence must retry a lost metadata CAS, not error: %v", err)
	}
	if got := getEpoch(t, store, control.ID); got != "9" {
		t.Fatalf("final epoch = %q, want 9 (converged past the injected 7 after the conflict retry)", got)
	}
	// Two appends: the initial acquisition + the re-acquisition after the metadata
	// CAS conflict. head==1 would mean the branch never retried (residual open).
	head, err := journal.StreamHead(ctx, beads.ControlEpochFenceStreamID(control.ID))
	if err != nil {
		t.Fatalf("StreamHead: %v", err)
	}
	if head != 2 {
		t.Fatalf("fence stream head = %d, want 2 (metadata CAS conflict must trigger a re-append + re-decide)", head)
	}
}

// --- MEDIUM-1: raw-expected CAS converges on a non-canonical stored epoch -----

// newNonCanonicalControlBead creates a legacy control bead whose gc.control_epoch
// is a non-canonical string (an external/hand writer's value like "02" or " 2"),
// asserting the id is non-journal-resident so the legacy fence path is exercised.
func newNonCanonicalControlBead(t *testing.T, store beads.Store, epoch string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    "legacy control",
		Type:     "task",
		Metadata: map[string]string{beadmeta.ControlEpochMetadataKey: epoch},
	})
	if err != nil {
		t.Fatalf("create non-canonical control bead: %v", err)
	}
	if beads.IsJournalResidentID(b.ID) {
		t.Fatalf("store minted a journal-resident id %q; the legacy-branch test would be meaningless", b.ID)
	}
	if got := getEpoch(t, store, b.ID); got != epoch {
		t.Fatalf("seeded epoch = %q, want the verbatim non-canonical %q (store must not normalize it)", got, epoch)
	}
	return b
}

// TestFenceLegacySite1NonCanonicalEpochConverges is the livelock-convergence pin
// for MEDIUM-1 at funnel A (syncControlEpochToAttempt). An external writer left the
// epoch non-canonical ("02"). The CAS conditions on the RAW observed value, matching
// verbatim, swapping to the canonical next, and converging in ONE attempt — where
// re-canonicalizing expected to "2" would miss every retry (stored "02" != "2"),
// exhaust the budget, and return errControlFenceContended forever (the livelock).
func TestFenceLegacySite1NonCanonicalEpochConverges(t *testing.T) {
	store := &instrumentedCASStore{MemStore: beads.NewMemStore()}
	control := newNonCanonicalControlBead(t, store, "02")
	attempt3, err := store.Create(beads.Bead{Title: "attempt3", Type: "task", Metadata: map[string]string{beadmeta.AttemptMetadataKey: "3"}})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	if err := syncControlEpochToAttempt(store, control, attempt3); err != nil {
		t.Fatalf("syncControlEpochToAttempt on non-canonical epoch: %v "+
			"(a re-canonicalized expected would livelock to errControlFenceContended)", err)
	}

	if got := getEpoch(t, store, control.ID); got != "3" {
		t.Fatalf("epoch = %q, want canonical 3 (the swap must normalize the non-canonical 02)", got)
	}
	ifc := store.epochIfCalls()
	if len(ifc) != 1 || ifc[0] != (casCall{control.ID, beadmeta.ControlEpochMetadataKey, "02", "3", true}) {
		t.Fatalf("epoch CAS calls = %+v, want exactly one conditional 02→3 that swapped "+
			"(expected must be the RAW observed value, not strconv.Itoa(current))", ifc)
	}
}

// TestFenceLegacySite1NonCanonicalEqualAttemptNoOps proves the DECISION still uses
// the PARSED int even when the stored epoch is non-canonical: an attempt equal to
// the parsed current (" 2" parses to 2, attempt 2) hits the regression-kill no-op
// BEFORE any CAS. The raw " 2" is left untouched and ZERO SetMetadataIf fires —
// which could only happen if the guard compared parsed 2 <= 2, not the strings.
func TestFenceLegacySite1NonCanonicalEqualAttemptNoOps(t *testing.T) {
	store := &instrumentedCASStore{MemStore: beads.NewMemStore()}
	control := newNonCanonicalControlBead(t, store, " 2")
	attempt2, err := store.Create(beads.Bead{Title: "attempt2", Type: "task", Metadata: map[string]string{beadmeta.AttemptMetadataKey: "2"}})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	if err := syncControlEpochToAttempt(store, control, attempt2); err != nil {
		t.Fatalf("syncControlEpochToAttempt: %v", err)
	}

	if got := getEpoch(t, store, control.ID); got != " 2" {
		t.Fatalf("epoch = %q, want unchanged %q (equal attempt must no-op, not rewrite)", got, " 2")
	}
	if n := len(store.epochIfCalls()); n != 0 {
		t.Fatalf("epoch SetMetadataIf calls = %d, want 0 (equal attempt no-ops on the parsed int before any write)", n)
	}
}

// TestFenceLegacySite2NonCanonicalEpochConverges is the MEDIUM-1 convergence pin at
// funnel B (molecule.Attach's post-instantiate bump). A control bead left at a
// non-canonical epoch "01" is advanced through the fence: bumpEpochIfCurrent
// conditions the CAS on the RAW observed "01" (not strconv.Itoa(expectedEpoch)="1"),
// so it swaps to the canonical "2" in one attempt instead of livelocking, and Attach
// succeeds. The pre-instantiate epoch pre-check parses "01" to 1 == ExpectedEpoch 1.
func TestFenceLegacySite2NonCanonicalEpochConverges(t *testing.T) {
	store := &instrumentedCASStore{MemStore: beads.NewMemStore()}
	root, err := store.Create(beads.Bead{
		Title: "workflow", Type: "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	control, err := store.Create(beads.Bead{
		Title: "control", Type: "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:   root.ID,
			beadmeta.ControlEpochMetadataKey: "01",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	if beads.IsJournalResidentID(control.ID) {
		t.Fatalf("control id %q is journal-resident; the legacy test is meaningless", control.ID)
	}

	if _, err := molecule.Attach(context.Background(), store, legacyAttachRecipe(), control.ID, molecule.AttachOptions{
		ExpectedEpoch: 1,
		Fence:         fenceControlWrite,
	}); err != nil {
		t.Fatalf("Attach on non-canonical epoch: %v (a re-canonicalized expected would livelock)", err)
	}

	if got := getEpoch(t, store, control.ID); got != "2" {
		t.Fatalf("epoch = %q, want canonical 2 (the bump must swap the non-canonical 01)", got)
	}
	ifc := store.epochIfCalls()
	if len(ifc) != 1 || ifc[0] != (casCall{control.ID, beadmeta.ControlEpochMetadataKey, "01", "2", true}) {
		t.Fatalf("epoch CAS calls = %+v, want one conditional 01→2 that swapped", ifc)
	}
}

// --- MEDIUM-2: unsupported-cap error from the legacy fence path is transient ---

// unsupportedCapStore is ConditionalMetadataStore-capable at the TYPE level (so the
// legacy fence takes the CAS-loop branch, not the caps-absent fallback), but its
// SetMetadataIf reports the backing leg lacks the capability — exactly what a
// CachingStore or residence-router wrapper over a non-CAS leg returns. The error
// escapes the fence's caps probe, so IsTransientControllerError must classify it
// transient or markControllerSpawnError would hard-close the workflow.
type unsupportedCapStore struct{ *beads.MemStore }

func (unsupportedCapStore) SetMetadataIf(context.Context, string, string, string, string) (bool, error) {
	return false, beads.ErrConditionalMetadataUnsupported
}

// TestFenceLegacyUnsupportedCapEscapesTransientNotClose pins MEDIUM-2: a wrapper that
// advertises ConditionalMetadataStore but whose backing leg lacks the cap surfaces
// beads.ErrConditionalMetadataUnsupported out of the legacy fence path. That error
// must (1) bubble unchanged, (2) classify TRANSIENT, and (3) take
// markControllerSpawnError's pending branch — the workflow re-dispatches, never a
// setOutcomeAndClose(OutcomeFail) workflow-kill.
func TestFenceLegacyUnsupportedCapEscapesTransientNotClose(t *testing.T) {
	store := unsupportedCapStore{MemStore: beads.NewMemStore()}
	if _, ok := beads.ConditionalMetadataStoreFor(store); !ok {
		t.Fatalf("unsupportedCapStore must advertise ConditionalMetadataStore so the error escapes the caps probe")
	}
	control := newLegacyControlBead(t, store)

	err := fenceControlWrite(context.Background(), store, control.ID, func(ctx context.Context) error {
		return beads.SetMetadataConditionally(ctx, store, control.ID, beadmeta.ControlEpochMetadataKey, "1", "2")
	})
	if !errors.Is(err, beads.ErrConditionalMetadataUnsupported) {
		t.Fatalf("err = %v, want ErrConditionalMetadataUnsupported bubbling from the legacy fence", err)
	}
	if !IsTransientControllerError(err) {
		t.Fatalf("unsupported-cap error must classify transient so the workflow re-dispatches; "+
			"IsTransientControllerError=false for %v", err)
	}

	pending := markControllerSpawnError(store, control.ID, err, ProcessOptions{})
	if !pending {
		t.Fatalf("markControllerSpawnError returned false (hard close) for %v; a wiring-gap error must re-dispatch, not close", err)
	}
	got, gerr := store.Get(control.ID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if got.Status == "closed" {
		t.Fatalf("bead closed on an unsupported-cap fence error; the workflow must survive")
	}
}
