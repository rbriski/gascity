package splittest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// TestPlainMemStoreAcceptsCrossStoreDep documents the leniency gap this
// package closes: a plain MemStore appends a dependency edge without
// resolving either endpoint, so a cross-store dep that hard-fails on the
// production bd backend ("no issue found") silently succeeds in-process. If
// this test ever FAILS, MemStore has learned endpoint validation and the
// strict DepAdd guard may be redundant — re-evaluate, don't delete blindly.
func TestPlainMemStoreAcceptsCrossStoreDep(t *testing.T) {
	work := beads.NewMemStore()
	infra := beads.NewMemStoreHonoringIDs()

	workBead, err := work.Create(beads.Bead{Title: "work item"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	infraBead, err := infra.Create(beads.Bead{ID: "gcg-1", Title: "infra step"})
	if err != nil {
		t.Fatalf("create infra bead: %v", err)
	}

	// The infra bead does NOT exist in the work store, yet the dep lands.
	if err := work.DepAdd(workBead.ID, infraBead.ID, "blocks"); err != nil {
		t.Fatalf("plain MemStore rejected a cross-store dep — the leniency this package closes has been fixed upstream: %v", err)
	}
	deps, err := work.DepList(workBead.ID, "down")
	if err != nil {
		t.Fatalf("dep list: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != infraBead.ID {
		t.Fatalf("expected the lenient cross-store dep row to exist, got %+v", deps)
	}
}

func TestStrictDepAddRejectsCrossStoreDep(t *testing.T) {
	work, infra := NewSplitStores(t)

	workBead, err := work.Create(beads.Bead{Title: "work item"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	infraBead, err := infra.Create(beads.Bead{Title: "graph step"})
	if err != nil {
		t.Fatalf("create infra bead: %v", err)
	}

	for _, tc := range []struct {
		name    string
		store   beads.Store
		issue   string
		depends string
		missing string
	}{
		{name: "work store, infra endpoint missing", store: work, issue: workBead.ID, depends: infraBead.ID, missing: infraBead.ID},
		{name: "infra store, work endpoint missing", store: infra, issue: infraBead.ID, depends: workBead.ID, missing: workBead.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.store.DepAdd(tc.issue, tc.depends, "blocks")
			if err == nil {
				t.Fatal("cross-store DepAdd succeeded; want bd-shaped rejection")
			}
			if !strings.Contains(err.Error(), "no issue found") {
				t.Fatalf("error not shaped like the bd backend: %v", err)
			}
			if !strings.Contains(err.Error(), tc.missing) {
				t.Fatalf("error does not name the missing endpoint %s: %v", tc.missing, err)
			}
			// bd's real failure is a subprocess stderr string, never a typed
			// not-found; the strict error must not be friendlier than production.
			if errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("strict DepAdd error wraps beads.ErrNotFound; production bd errors are text-only: %v", err)
			}
			deps, listErr := tc.store.DepList(tc.issue, "down")
			if listErr != nil {
				t.Fatalf("dep list: %v", listErr)
			}
			if len(deps) != 0 {
				t.Fatalf("rejected dep still landed in the leaf: %+v", deps)
			}
		})
	}
}

func TestStrictDepAddPermitsSameStoreDepAndBlocksReady(t *testing.T) {
	work, _ := NewSplitStores(t)

	blocker, err := work.Create(beads.Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := work.Create(beads.Bead{Title: "blocked"})
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}
	if err := work.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("same-store DepAdd rejected: %v", err)
	}

	// The readiness invariant must hold through the wrapper: the dep row
	// exists and gates Ready until the blocker closes (the warm-tick
	// demand/readiness surface, not just row presence).
	ready, err := work.Ready()
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if containsBeadID(ready, blocked.ID) {
		t.Fatalf("blocked bead %s is ready while its same-store dep is open", blocked.ID)
	}
	if err := work.Close(blocker.ID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	ready, err = work.Ready()
	if err != nil {
		t.Fatalf("ready after close: %v", err)
	}
	if !containsBeadID(ready, blocked.ID) {
		t.Fatalf("bead %s not ready after its dep closed; ready=%+v", blocked.ID, ready)
	}
}

func TestStrictDepAddPreservesParentChildShortCircuit(t *testing.T) {
	_, infra := NewSplitStores(t)

	// The parent lives in ANOTHER store: only the child's ParentID field
	// records it here. BdStore.DepAdd returns nil for a parent-child dep that
	// restates ParentID, before bd ever resolves the endpoints — the strict
	// wrapper must not be stricter than production on this path.
	child, err := infra.Create(beads.Bead{Title: "step", ParentID: "gc-999"})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := infra.DepAdd(child.ID, "gc-999", "parent-child"); err != nil {
		t.Fatalf("parent-child short-circuit lost: %v", err)
	}
	deps, err := infra.DepList(child.ID, "down")
	if err != nil {
		t.Fatalf("dep list: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("short-circuited parent-child dep minted a row: %+v", deps)
	}

	// A parent-child dep that does NOT match ParentID gets no carve-out: the
	// missing endpoint is rejected like any other cross-store reference.
	err = infra.DepAdd(child.ID, "gc-1000", "parent-child")
	if err == nil || !strings.Contains(err.Error(), "no issue found") {
		t.Fatalf("non-matching parent-child dep with absent endpoint: got %v, want bd-shaped rejection", err)
	}
}

func TestStrictCreateRejectsForeignPrefixExplicitID(t *testing.T) {
	work, infra := NewSplitStores(t)

	if _, err := work.Create(beads.Bead{ID: "gcg-oops", Title: "infra row in work store"}); err == nil {
		t.Fatal("work store minted an infra-prefixed row; want foreign-prefix rejection")
	}
	if _, err := infra.Create(beads.Bead{ID: "gc-42", Title: "work row in infra store"}); err == nil {
		t.Fatal("infra store minted a work-prefixed row; want foreign-prefix rejection")
	}

	// Own-prefix explicit ids and store-minted ids stay accepted.
	kept, err := infra.Create(beads.Bead{ID: "gcg-stable", Title: "explicit infra id"})
	if err != nil {
		t.Fatalf("own-prefix explicit id rejected: %v", err)
	}
	if kept.ID != "gcg-stable" {
		t.Fatalf("explicit infra id not honored: got %s", kept.ID)
	}
	minted, err := work.Create(beads.Bead{Title: "minted"})
	if err != nil {
		t.Fatalf("store-minted create rejected: %v", err)
	}
	if !strings.HasPrefix(minted.ID, "gc-") {
		t.Fatalf("work store minted %s, want gc- prefix", minted.ID)
	}
}

func TestStrictCreateWithForeignIDBypassesGuard(t *testing.T) {
	_, infra := NewSplitStores(t)

	creator, ok := infra.(beads.ForeignIDCreator)
	if !ok {
		t.Fatal("strict store lost the beads.ForeignIDCreator capability")
	}
	// The migration path: copy a legacy HQ/rig-era bead into the infra store
	// KEEPING its foreign-prefix id (bd create --id ... --force).
	legacy, err := creator.CreateWithForeignID(beads.Bead{ID: "ga-legacy-7", Title: "migrated"})
	if err != nil {
		t.Fatalf("forced foreign-prefix create rejected: %v", err)
	}
	if legacy.ID != "ga-legacy-7" {
		t.Fatalf("forced create re-minted the id: got %s", legacy.ID)
	}

	if _, err := Strict(bareLeaf{beads.NewMemStore()}).(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{ID: "x-1", Title: "t"}); err == nil {
		t.Fatal("leaf without ForeignIDCreator: want loud error, got success")
	}
}

func TestStrictTxCreateIsGuardedAndMints(t *testing.T) {
	work, infra := NewSplitStores(t)

	err := work.Tx("tx-foreign", func(tx beads.Tx) error {
		_, err := tx.Create(beads.Bead{ID: "gcg-side-door", Title: "foreign row via tx"})
		return err
	})
	if err == nil {
		t.Fatal("Tx.Create minted a foreign-prefix row; want the same guard as Create")
	}

	var minted beads.Bead
	if err := infra.Tx("tx-mint", func(tx beads.Tx) error {
		b, err := tx.Create(beads.Bead{Title: "tx-minted step"})
		minted = b
		return err
	}); err != nil {
		t.Fatalf("tx create: %v", err)
	}
	if !strings.HasPrefix(minted.ID, "gcg-") {
		t.Fatalf("infra Tx.Create minted %s, want gcg- prefix", minted.ID)
	}
	if _, err := infra.Get(minted.ID); err != nil {
		t.Fatalf("tx-minted bead not readable: %v", err)
	}
}

func TestStrictCreateFailsLoudWhenLeafMintsOutsideNamespace(t *testing.T) {
	// An id-clobbering leaf (plain MemStore mints gc-<n>) declared with a
	// different prefix violates the residence invariant on every create —
	// the post-check must catch it instead of letting a foreign-prefix row
	// sit in the store.
	misconfigured := StrictWithPrefix(beads.NewMemStore(), "gcg")
	_, err := misconfigured.Create(beads.Bead{Title: "row lands as gc-1"})
	if err == nil {
		t.Fatal("leaf minted outside the declared namespace and the wrapper stayed quiet")
	}
	if !strings.Contains(err.Error(), "outside its declared id namespace") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

func TestStrictWispTierTransparent(t *testing.T) {
	_, infra := NewSplitStores(t)

	// Production molecules materialize as EPHEMERAL wisps (gcg-wisp-*, the
	// wisps table), not main-tier rows — the tier the live incidents
	// happened in. The wrapper must keep that tier fully reachable.
	wisp, err := infra.Create(beads.Bead{ID: "gcg-wisp-w1", Title: "step wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("create wisp: %v", err)
	}
	if wisp.ID != "gcg-wisp-w1" || !wisp.Ephemeral {
		t.Fatalf("wisp shape lost through wrapper: %+v", wisp)
	}
	if _, err := infra.Get(wisp.ID); err != nil {
		t.Fatalf("wisp not readable by id: %v", err)
	}

	durable, err := infra.Create(beads.Bead{Title: "durable step"})
	if err != nil {
		t.Fatalf("create durable: %v", err)
	}

	issues, err := infra.List(beads.ListQuery{Status: "open", TierMode: beads.TierIssues})
	if err != nil {
		t.Fatalf("list issues tier: %v", err)
	}
	if containsBeadID(issues, wisp.ID) {
		t.Fatalf("ephemeral wisp leaked into the issues tier: %+v", issues)
	}
	wisps, err := infra.List(beads.ListQuery{Status: "open", TierMode: beads.TierWisps})
	if err != nil {
		t.Fatalf("list wisp tier: %v", err)
	}
	if !containsBeadID(wisps, wisp.ID) || containsBeadID(wisps, durable.ID) {
		t.Fatalf("wisp tier query wrong through wrapper: %+v", wisps)
	}
	both, err := infra.List(beads.ListQuery{Status: "open", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("list both tiers: %v", err)
	}
	if !containsBeadID(both, wisp.ID) || !containsBeadID(both, durable.ID) {
		t.Fatalf("TierBoth misses a tier through wrapper: %+v", both)
	}

	// Warm-tick readiness: the wisp is ready work on its own tier…
	readyWisps, err := infra.Ready(beads.ReadyQuery{TierMode: beads.TierWisps})
	if err != nil {
		t.Fatalf("ready wisps: %v", err)
	}
	if !containsBeadID(readyWisps, wisp.ID) {
		t.Fatalf("open wisp not ready on the wisp tier: %+v", readyWisps)
	}
	// …same-store wisp deps work, and cross-store wisp deps fail loud like
	// any other cross-store reference.
	if err := infra.DepAdd(wisp.ID, durable.ID, "blocks"); err != nil {
		t.Fatalf("same-store wisp dep rejected: %v", err)
	}
	err = infra.DepAdd(wisp.ID, "gc-77", "blocks")
	if err == nil || !strings.Contains(err.Error(), "no issue found") {
		t.Fatalf("cross-store wisp dep: got %v, want bd-shaped rejection", err)
	}
}

// --- capability-survival fixtures -----------------------------------------
//
// Each fake leaf embeds a store and adds exactly one optional capability, so
// the tests can prove the type-asserted capability survives Strict() (or is
// honestly absent). bareLeaf is the opposite: an interface embed that strips
// every optional capability from a MemStore, standing in for a minimal leaf.

type bareLeaf struct{ beads.Store }

type countingLeaf struct {
	*beads.MemStore
	counts int
}

func (c *countingLeaf) Count(_ context.Context, _ beads.ListQuery, _ ...string) (int, error) {
	c.counts++
	return 42, nil
}

type graphLeaf struct {
	*beads.MemStore
	applied int
}

func (g *graphLeaf) ApplyGraphPlan(_ context.Context, _ *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	g.applied++
	return &beads.GraphApplyResult{IDs: map[string]string{}}, nil
}

type storageLeaf struct {
	*beads.MemStore
	lastStorage beads.StorageClass
}

func (s *storageLeaf) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	s.lastStorage = storage
	return s.Create(b)
}

type closerLeaf struct {
	*beads.MemStore
	closed int
}

func (c *closerLeaf) CloseStore() error {
	c.closed++
	return nil
}

type backingLeaf struct {
	*beads.MemStore
	backing beads.Store
}

func (b *backingLeaf) Backing() beads.Store { return b.backing }

type prefixLeaf struct {
	*beads.MemStore
	prefix string
}

func (p *prefixLeaf) IDPrefix() string { return p.prefix }

func TestStrictForwardsHandlesWithStrictWriter(t *testing.T) {
	work, infra := NewSplitStores(t)

	infraBead, err := infra.Create(beads.Bead{Title: "elsewhere"})
	if err != nil {
		t.Fatalf("create infra bead: %v", err)
	}
	workBead, err := work.Create(beads.Bead{Title: "here"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	handles := beads.HandlesFor(work)
	if err := handles.Writer.DepAdd(workBead.ID, infraBead.ID, "blocks"); err == nil {
		t.Fatal("HandlesFor Writer.DepAdd bypassed the strict cross-store guard")
	}
	if _, err := handles.Writer.Create(beads.Bead{ID: "gcg-h1", Title: "foreign via writer"}); err == nil {
		t.Fatal("HandlesFor Writer.Create bypassed the foreign-prefix guard")
	}
	got, err := handles.Live.Get(workBead.ID)
	if err != nil || got.ID != workBead.ID {
		t.Fatalf("Handles live reader broken: %v %+v", err, got)
	}
}

func TestStrictForwardsIDPrefix(t *testing.T) {
	work, infra := NewSplitStores(t)

	owner := storeref.PrefixOwner("gcg-anything", []beads.Store{work, infra})
	if owner != infra {
		t.Fatalf("PrefixOwner(gcg-…) = %T, want the infra store", owner)
	}
	if owner := storeref.PrefixOwner("gc-1", []beads.Store{infra, work}); owner != work {
		t.Fatalf("PrefixOwner(gc-1) = %T, want the work store", owner)
	}

	// Strict() infers the prefix from a leaf that exposes one.
	inferred := Strict(&prefixLeaf{MemStore: beads.NewMemStore(), prefix: "GCM-"})
	if got := inferred.(storeref.HasIDPrefix).IDPrefix(); got != "gcm" {
		t.Fatalf("inferred prefix = %q, want normalized \"gcm\"", got)
	}
	// A leaf without one reports "", which storeref skips like a missing accessor.
	if got := Strict(bareLeaf{beads.NewMemStore()}).(storeref.HasIDPrefix).IDPrefix(); got != "" {
		t.Fatalf("bare leaf prefix = %q, want empty", got)
	}
}

func TestStrictForwardsCounter(t *testing.T) {
	leaf := &countingLeaf{MemStore: beads.NewMemStore()}
	strict := Strict(leaf)

	n, err := strict.(beads.Counter).Count(context.Background(), beads.ListQuery{AllowScan: true})
	if err != nil || n != 42 {
		t.Fatalf("Count through wrapper = %d, %v; want leaf's 42", n, err)
	}
	if leaf.counts != 1 {
		t.Fatalf("leaf Count called %d times, want 1", leaf.counts)
	}

	_, err = Strict(bareLeaf{beads.NewMemStore()}).(beads.Counter).Count(context.Background(), beads.ListQuery{AllowScan: true})
	if !errors.Is(err, beads.ErrCountUnsupported) {
		t.Fatalf("counter-less leaf: got %v, want ErrCountUnsupported (List fallback signal)", err)
	}
}

func TestStrictForwardsGraphApply(t *testing.T) {
	leaf := &graphLeaf{MemStore: beads.NewMemStore()}
	applier, ok := beads.GraphApplyFor(Strict(leaf))
	if !ok {
		t.Fatal("GraphApplyFor lost the leaf's graph-apply capability")
	}
	if _, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{}); err != nil {
		t.Fatalf("forwarded ApplyGraphPlan: %v", err)
	}
	if leaf.applied != 1 {
		t.Fatalf("leaf applier called %d times, want 1", leaf.applied)
	}

	if _, ok := beads.GraphApplyFor(Strict(beads.NewMemStore())); ok {
		t.Fatal("wrapper falsely claims graph-apply for a MemStore leaf")
	}
}

func TestStrictForwardsStorageCreateOnlyWhenLeafHasIt(t *testing.T) {
	leaf := &storageLeaf{MemStore: beads.NewMemStore()}
	strict := StrictWithPrefix(leaf, "gc")

	storageStore, ok := strict.(beads.StorageCreateStore)
	if !ok {
		t.Fatal("StorageCreateStore capability stripped from a storage-capable leaf")
	}
	if _, err := storageStore.CreateWithStorage(beads.Bead{ID: "gcg-w1", Title: "foreign"}, beads.StorageEphemeral); err == nil {
		t.Fatal("CreateWithStorage bypassed the foreign-prefix guard")
	}
	if _, err := storageStore.CreateWithStorage(beads.Bead{Title: "wisp"}, beads.StorageEphemeral); err != nil {
		t.Fatalf("CreateWithStorage through wrapper: %v", err)
	}
	if leaf.lastStorage != beads.StorageEphemeral {
		t.Fatalf("storage class not forwarded: got %q", leaf.lastStorage)
	}

	// A MemStore leaf must NOT gain the claim: createWithStoragePolicy only
	// falls back to flag-based Create when the assertion fails, and that
	// fallback is how MemStore fixtures get their wisp tier.
	if _, ok := Strict(beads.NewMemStore()).(beads.StorageCreateStore); ok {
		t.Fatal("wrapper falsely claims StorageCreateStore for a MemStore leaf")
	}
}

func TestStrictForwardsLeafWriteCapabilities(t *testing.T) {
	work, _ := NewSplitStores(t)

	b, err := work.Create(beads.Bead{Title: "assigned", Assignee: "polecat-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	status := "in_progress"
	if err := work.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("update: %v", err)
	}
	released, err := work.(beads.ConditionalAssignmentReleaser).ReleaseIfCurrent(b.ID, "polecat-1")
	if err != nil || !released {
		t.Fatalf("ReleaseIfCurrent through wrapper = %v, %v; want released", released, err)
	}

	if _, err := work.(beads.BatchDeleter).DeleteAllOrphaning([]string{b.ID}); err != nil {
		t.Fatalf("DeleteAllOrphaning through wrapper: %v", err)
	}
	if _, err := work.Get(b.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("bead survived batch delete: %v", err)
	}

	bare := Strict(bareLeaf{beads.NewMemStore()})
	if _, err := bare.(beads.ConditionalAssignmentReleaser).ReleaseIfCurrent("gc-1", "x"); !errors.Is(err, beads.ErrConditionalReleaseUnsupported) {
		t.Fatalf("releaser-less leaf: got %v, want ErrConditionalReleaseUnsupported", err)
	}
	if _, err := bare.(beads.BatchDeleter).DeleteAllOrphaning([]string{"gc-1"}); err == nil {
		t.Fatal("batch delete on a leaf without the capability must error, never fall back")
	}
}

func TestStrictForwardsDepListBatch(t *testing.T) {
	work, _ := NewSplitStores(t)
	a, _ := work.Create(beads.Bead{Title: "a"})
	b, _ := work.Create(beads.Bead{Title: "b"})
	if err := work.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("dep add: %v", err)
	}

	type depBatchLister interface {
		DepListBatch(ids []string) (map[string][]beads.Dep, error)
	}
	got, err := work.(depBatchLister).DepListBatch([]string{a.ID})
	if err != nil || len(got[a.ID]) != 1 {
		t.Fatalf("DepListBatch through wrapper = %+v, %v", got, err)
	}

	// A leaf without the batch capability gets the same per-id fallback the
	// dispatch scope-skip walk runs itself.
	bare := Strict(bareLeaf{work})
	got, err = bare.(depBatchLister).DepListBatch([]string{a.ID})
	if err != nil || len(got[a.ID]) != 1 {
		t.Fatalf("DepListBatch fallback = %+v, %v", got, err)
	}
}

func TestStrictForwardsLifecycleCapabilities(t *testing.T) {
	leaf := &closerLeaf{MemStore: beads.NewMemStore()}
	if err := Strict(leaf).(interface{ CloseStore() error }).CloseStore(); err != nil {
		t.Fatalf("CloseStore through wrapper: %v", err)
	}
	if leaf.closed != 1 {
		t.Fatalf("leaf CloseStore called %d times, want 1", leaf.closed)
	}
	if err := Strict(beads.NewMemStore()).(interface{ CloseStore() error }).CloseStore(); err != nil {
		t.Fatalf("CloseStore on a leaf without one must no-op: %v", err)
	}

	if beads.StoreSupportsAtomicTx(Strict(beads.NewMemStore())) {
		t.Fatal("wrapper claims atomic Tx for a non-atomic leaf")
	}

	backing := beads.NewMemStore()
	seeded, err := backing.Create(beads.Bead{Title: "live row"})
	if err != nil {
		t.Fatalf("seed backing: %v", err)
	}
	live, err := beads.ReadyLive(Strict(&backingLeaf{MemStore: beads.NewMemStore(), backing: backing}))
	if err != nil {
		t.Fatalf("ReadyLive through wrapper: %v", err)
	}
	if !containsBeadID(live, seeded.ID) {
		t.Fatalf("ReadyLive did not reach the leaf's backing store: %+v", live)
	}

	if err := Strict(beads.NewMemStore()).(beads.ParentProjectionWaiter).WaitForParentProjection(context.Background(), "gc-1", "", "gc-2"); err != nil {
		t.Fatalf("synchronous leaf projection wait must converge immediately: %v", err)
	}
}

func containsBeadID(items []beads.Bead, id string) bool {
	for _, b := range items {
		if b.ID == id {
			return true
		}
	}
	return false
}
