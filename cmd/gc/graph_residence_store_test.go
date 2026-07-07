package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// residenceLegStore is a call-recording beads.Store fake over an in-memory store.
// It records every method that residence-routing tests assert on, delegating to
// the embedded MemStore for real behavior. getErr, when set, makes Get return a
// hard (non-ErrNotFound) failure so the write-fails-loudly discipline can be
// exercised. It implements GraphApplyStore (MemStore does not) so the routing
// applier can pick a leg by anchor residence.
type residenceLegStore struct {
	*beads.MemStore
	name   string
	calls  []string
	getErr error
}

// newResidenceLegStore builds a leg whose MemStore mints ids from seqStart+1.
// Real legs mint structurally disjoint ids (journal "gcg-j<seq>" vs bd's shape);
// the two test legs must likewise not collide, so callers offset the legacy leg.
func newResidenceLegStore(name string, seqStart int) *residenceLegStore {
	return &residenceLegStore{MemStore: beads.NewMemStoreFrom(seqStart, nil, nil), name: name}
}

func (r *residenceLegStore) rec(op string) { r.calls = append(r.calls, op) }

func (r *residenceLegStore) has(op string) bool { return slices.Contains(r.calls, op) }

func (r *residenceLegStore) Get(id string) (beads.Bead, error) {
	r.rec("Get")
	if r.getErr != nil {
		return beads.Bead{}, r.getErr
	}
	return r.MemStore.Get(id)
}

func (r *residenceLegStore) Create(b beads.Bead) (beads.Bead, error) {
	r.rec("Create")
	return r.MemStore.Create(b)
}

func (r *residenceLegStore) Update(id string, opts beads.UpdateOpts) error {
	r.rec("Update")
	return r.MemStore.Update(id, opts)
}

func (r *residenceLegStore) Close(id string) error {
	r.rec("Close")
	return r.MemStore.Close(id)
}

func (r *residenceLegStore) Reopen(id string) error {
	r.rec("Reopen")
	return r.MemStore.Reopen(id)
}

func (r *residenceLegStore) SetMetadata(id, key, value string) error {
	r.rec("SetMetadata")
	return r.MemStore.SetMetadata(id, key, value)
}

func (r *residenceLegStore) SetMetadataBatch(id string, kvs map[string]string) error {
	r.rec("SetMetadataBatch")
	return r.MemStore.SetMetadataBatch(id, kvs)
}

func (r *residenceLegStore) Delete(id string) error {
	r.rec("Delete")
	return r.MemStore.Delete(id)
}

func (r *residenceLegStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	r.rec("CloseAll")
	return r.MemStore.CloseAll(ids, metadata)
}

func (r *residenceLegStore) DepAdd(issueID, dependsOnID, depType string) error {
	r.rec("DepAdd")
	return r.MemStore.DepAdd(issueID, dependsOnID, depType)
}

func (r *residenceLegStore) DepRemove(issueID, dependsOnID string) error {
	r.rec("DepRemove")
	return r.MemStore.DepRemove(issueID, dependsOnID)
}

func (r *residenceLegStore) DepList(id, direction string) ([]beads.Dep, error) {
	r.rec("DepList")
	return r.MemStore.DepList(id, direction)
}

func (r *residenceLegStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	r.rec("Children")
	return r.MemStore.Children(parentID, opts...)
}

func (r *residenceLegStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	r.rec("List")
	return r.MemStore.List(query)
}

func (r *residenceLegStore) ListOpen(status ...string) ([]beads.Bead, error) {
	r.rec("ListOpen")
	return r.MemStore.ListOpen(status...)
}

func (r *residenceLegStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	r.rec("Ready")
	return r.MemStore.Ready(query...)
}

func (r *residenceLegStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	r.rec("ListByLabel")
	return r.MemStore.ListByLabel(label, limit, opts...)
}

func (r *residenceLegStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	r.rec("ListByAssignee")
	return r.MemStore.ListByAssignee(assignee, status, limit)
}

func (r *residenceLegStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	r.rec("ListByMetadata")
	return r.MemStore.ListByMetadata(filters, limit, opts...)
}

func (r *residenceLegStore) Ping() error {
	r.rec("Ping")
	return r.MemStore.Ping()
}

// appliedPlan records that ApplyGraphPlan ran on this leg, plus the plan.
type appliedPlan struct {
	leg  string
	plan *beads.GraphApplyPlan
}

// residenceApplyLeg extends residenceLegStore with a GraphApplyStore
// implementation that records which leg materialized a plan.
type residenceApplyLeg struct {
	*residenceLegStore
	applied *[]appliedPlan
}

func newResidenceApplyLeg(name string, seqStart int, applied *[]appliedPlan) *residenceApplyLeg {
	return &residenceApplyLeg{residenceLegStore: newResidenceLegStore(name, seqStart), applied: applied}
}

//nolint:unparam // the error return is mandated by the beads.GraphApplyStore interface.
func (r *residenceApplyLeg) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	*r.applied = append(*r.applied, appliedPlan{leg: r.name, plan: plan})
	ids := make(map[string]string, len(plan.Nodes))
	for _, node := range plan.Nodes {
		ids[node.Key] = "applied-" + node.Key
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

// plant creates a bead in a leg and returns its minted id.
func plant(t *testing.T, store *residenceLegStore, title string) string {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: title})
	if err != nil {
		t.Fatalf("planting %q: %v", title, err)
	}
	return b.ID
}

func TestGraphResidenceRoutingRootAtomic(t *testing.T) {
	journal := newResidenceLegStore("journal", 0)
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStore(journal, legacy)

	jRoot := plant(t, journal, "r1")
	jChild, err := journal.Create(beads.Bead{Title: "r1-child", ParentID: jRoot})
	if err != nil {
		t.Fatalf("journal child: %v", err)
	}
	lRoot := plant(t, legacy, "r2")
	lChild, err := legacy.Create(beads.Bead{Title: "r2-child", ParentID: lRoot})
	if err != nil {
		t.Fatalf("legacy child: %v", err)
	}
	// Reset call logs so only the router-driven ops below are asserted.
	journal.calls = nil
	legacy.calls = nil

	// Every by-id op on a journal-resident id reaches only the journal leg.
	if _, err := router.Get(jRoot); err != nil {
		t.Fatalf("Get(journal root): %v", err)
	}
	if err := router.Update(jRoot, beads.UpdateOpts{}); err != nil {
		t.Fatalf("Update(journal root): %v", err)
	}
	if err := router.SetMetadata(jRoot, "k", "v"); err != nil {
		t.Fatalf("SetMetadata(journal root): %v", err)
	}
	if _, err := router.DepList(jRoot, "down"); err != nil {
		t.Fatalf("DepList(journal root): %v", err)
	}
	if _, err := router.Children(jRoot); err != nil {
		t.Fatalf("Children(journal root): %v", err)
	}
	if err := router.Close(jChild.ID); err != nil {
		t.Fatalf("Close(journal child): %v", err)
	}
	if legacy.has("Update") || legacy.has("SetMetadata") || legacy.has("DepList") ||
		legacy.has("Children") || legacy.has("Close") {
		t.Fatalf("journal-resident ops leaked to the legacy leg: %v", legacy.calls)
	}
	if !journal.has("Update") || !journal.has("SetMetadata") || !journal.has("Close") {
		t.Fatalf("journal-resident ops missing on the journal leg: %v", journal.calls)
	}

	journal.calls, legacy.calls = nil, nil
	// Every by-id op on a legacy id reaches only the legacy leg (the journal
	// probe Get is expected, but no journal write/read op).
	if err := router.Update(lRoot, beads.UpdateOpts{}); err != nil {
		t.Fatalf("Update(legacy root): %v", err)
	}
	if err := router.Close(lChild.ID); err != nil {
		t.Fatalf("Close(legacy child): %v", err)
	}
	if journal.has("Update") || journal.has("Close") {
		t.Fatalf("legacy-resident writes leaked to the journal leg: %v", journal.calls)
	}
	if !legacy.has("Update") || !legacy.has("Close") {
		t.Fatalf("legacy-resident writes missing on the legacy leg: %v", legacy.calls)
	}

	journal.calls, legacy.calls = nil, nil
	// Create routing: child of a journal parent → journal; child of a legacy
	// parent → legacy; a new root (no parent) → legacy (P1.5 new-root policy).
	if _, err := router.Create(beads.Bead{Title: "new-journal-child", ParentID: jRoot}); err != nil {
		t.Fatalf("Create(journal child): %v", err)
	}
	if !journal.has("Create") {
		t.Fatalf("child of journal parent did not route to the journal leg: %v", journal.calls)
	}
	journal.calls, legacy.calls = nil, nil
	if _, err := router.Create(beads.Bead{Title: "new-legacy-child", ParentID: lRoot}); err != nil {
		t.Fatalf("Create(legacy child): %v", err)
	}
	if !legacy.has("Create") || journal.has("Create") {
		t.Fatalf("child of legacy parent misrouted: journal=%v legacy=%v", journal.calls, legacy.calls)
	}
	journal.calls, legacy.calls = nil, nil
	if _, err := router.Create(beads.Bead{Title: "brand-new-root"}); err != nil {
		t.Fatalf("Create(new root): %v", err)
	}
	if !legacy.has("Create") || journal.has("Create") {
		t.Fatalf("new root did not mint in the legacy leg: journal=%v legacy=%v", journal.calls, legacy.calls)
	}
}

func TestGraphResidenceRoutingFanOutMergesBothLegs(t *testing.T) {
	journal := newResidenceLegStore("journal", 0)
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStore(journal, legacy)

	legacyID := plant(t, legacy, "legacy-open")
	journalID := plant(t, journal, "journal-open")

	ready, err := router.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 2 {
		t.Fatalf("Ready returned %d beads, want 2: %+v", len(ready), ready)
	}
	// Legacy-first ordering: the legacy row precedes the appended journal row.
	if ready[0].ID != legacyID || ready[1].ID != journalID {
		t.Fatalf("fan-out ordering = [%s %s], want legacy-first [%s %s]",
			ready[0].ID, ready[1].ID, legacyID, journalID)
	}

	// List also merges both legs.
	all, err := router.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List returned %d beads, want 2", len(all))
	}

	// A hard error on either leg fails the whole fan-out call.
	boom := errors.New("leg boom")
	failing := &failOnReady{residenceLegStore: newResidenceLegStore("failing", 0), err: boom}
	failRouter := newResidenceRoutingGraphStore(newResidenceLegStore("journal2", 0), failing)
	if _, err := failRouter.Ready(); !errors.Is(err, boom) {
		t.Fatalf("hard error on the legacy leg did not fail Ready: %v", err)
	}
}

// TestGraphResidenceOverlaysStoreIdentifiesLegacyLeg pins the HIGH-1 overlay
// contract: the router advertises that it overlays its legacy (non-journal) leg
// — the store whose beads its fan-out already returns — so the API projection
// and delete arms drop the redundant separate entry. Neither the journal leg
// nor an unrelated store is reported as overlaid.
func TestGraphResidenceOverlaysStoreIdentifiesLegacyLeg(t *testing.T) {
	journal := newResidenceLegStore("journal", 0)
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStore(journal, legacy)

	if !router.OverlaysStore(legacy) {
		t.Fatal("OverlaysStore(legacy) = false, want true (legacy leg is overlaid)")
	}
	if router.OverlaysStore(journal) {
		t.Fatal("OverlaysStore(journal) = true, want false (journal leg is not a separate overlaid entry)")
	}
	if router.OverlaysStore(newResidenceLegStore("other", 2000)) {
		t.Fatal("OverlaysStore(unrelated) = true, want false")
	}
	if router.OverlaysStore(nil) {
		t.Fatal("OverlaysStore(nil) = true, want false")
	}
	// The generic beads.StoreOverlaps helper resolves the capability the same way.
	if !beads.StoreOverlaps(router, legacy) {
		t.Fatal("beads.StoreOverlaps(router, legacy) = false, want true")
	}
}

// failOnReady makes Ready return a hard error, to prove fan-out fails loudly.
type failOnReady struct {
	*residenceLegStore
	err error
}

func (f *failOnReady) Ready(_ ...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, f.err
}

// TestGraphResidenceForwardsJournalCASCapsToJournalLeg pins the MED fix: the
// residence router forwards the journal CAS capabilities (AppendLogStore /
// ConditionalVersionStore) that back the control-epoch fence to its JOURNAL leg.
// Without these forwards a journal-resident control bead reached through the
// router would probe caps as absent and the fence would treat it as a wiring bug
// (or, before the fence hardening, silently write unfenced). The second half
// proves the forward is honest: a router whose journal leg lacks the caps
// reports absent rather than fabricating a stub.
func TestGraphResidenceForwardsJournalCASCapsToJournalLeg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "residence-caps"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	journal := beads.NewJournalStore(gs)
	legacy := newResidenceLegStore("legacy", 1000)

	router := newResidenceRoutingGraphStore(journal, legacy)
	if _, ok := beads.AppendLogStoreFor(router); !ok {
		t.Fatal("router does not forward AppendLogStore to the journal leg (fence would degrade)")
	}
	if _, ok := beads.ConditionalVersionStoreFor(router); !ok {
		t.Fatal("router does not forward ConditionalVersionStore to the journal leg (fence would degrade)")
	}

	// Honest absence: a journal leg without the caps must not be papered over.
	memJournal := newResidenceLegStore("journal", 0)
	memRouter := newResidenceRoutingGraphStore(memJournal, legacy)
	if _, ok := beads.AppendLogStoreFor(memRouter); ok {
		t.Fatal("router fabricated an AppendLogStore when the journal leg exposes none")
	}
	if _, ok := beads.ConditionalVersionStoreFor(memRouter); ok {
		t.Fatal("router fabricated a ConditionalVersionStore when the journal leg exposes none")
	}
}

func TestGraphResidenceRoutingHardProbeErrorFailsWrites(t *testing.T) {
	boom := errors.New("journal probe unavailable")
	journal := newResidenceLegStore("journal", 0)
	journal.getErr = boom
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStore(journal, legacy)

	if err := router.Update("gcg-1", beads.UpdateOpts{}); !errors.Is(err, boom) {
		t.Fatalf("Update did not surface the hard probe error: %v", err)
	}
	if err := router.Close("gcg-1"); !errors.Is(err, boom) {
		t.Fatalf("Close did not surface the hard probe error: %v", err)
	}
	if err := router.Delete("gcg-1"); !errors.Is(err, boom) {
		t.Fatalf("Delete did not surface the hard probe error: %v", err)
	}
	if legacy.has("Update") || legacy.has("Close") || legacy.has("Delete") {
		t.Fatalf("a write ran on the legacy leg despite an unknowable residence: %v", legacy.calls)
	}
}

func TestGraphResidenceCrossResidenceDepAddRejected(t *testing.T) {
	journal := newResidenceLegStore("journal", 0)
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStore(journal, legacy)

	jID := plant(t, journal, "journal-bead")
	lID := plant(t, legacy, "legacy-bead")
	journal.calls, legacy.calls = nil, nil

	err := router.DepAdd(jID, lID, "blocks")
	if !errors.Is(err, errCrossResidenceDependency) {
		t.Fatalf("cross-residence DepAdd err = %v, want errCrossResidenceDependency", err)
	}
	if journal.has("DepAdd") || legacy.has("DepAdd") {
		t.Fatalf("a cross-residence DepAdd wrote a leg: journal=%v legacy=%v", journal.calls, legacy.calls)
	}
}

func TestGraphResidenceApplyGraphPlanRoutesByAnchor(t *testing.T) {
	var applied []appliedPlan
	journal := newResidenceApplyLeg("journal", 0, &applied)
	legacy := newResidenceApplyLeg("legacy", 1000, &applied)
	router := newResidenceRoutingGraphStore(journal, legacy)

	jAnchor := plant(t, journal.residenceLegStore, "journal-anchor")
	lAnchor := plant(t, legacy.residenceLegStore, "legacy-anchor")

	handle, ok := router.GraphApplyHandle()
	if !ok {
		t.Fatal("router did not expose a graph-apply handle")
	}

	// Anchored in the journal leg (edge FromID) → journal applier.
	applied = nil
	journalPlan := &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{{Key: "n1", Title: "step"}},
		Edges: []beads.GraphApplyEdge{{FromID: jAnchor, ToKey: "n1", Type: "blocks"}},
	}
	if _, err := handle.ApplyGraphPlan(context.Background(), journalPlan); err != nil {
		t.Fatalf("journal-anchored apply: %v", err)
	}
	if len(applied) != 1 || applied[0].leg != "journal" {
		t.Fatalf("journal-anchored plan routed to %+v, want journal", applied)
	}

	// Anchored in the legacy leg (node ParentID) → legacy applier.
	applied = nil
	legacyPlan := &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{{Key: "n1", Title: "step", ParentID: lAnchor}},
	}
	if _, err := handle.ApplyGraphPlan(context.Background(), legacyPlan); err != nil {
		t.Fatalf("legacy-anchored apply: %v", err)
	}
	if len(applied) != 1 || applied[0].leg != "legacy" {
		t.Fatalf("legacy-anchored plan routed to %+v, want legacy", applied)
	}

	// Un-anchored (a brand-new root) → legacy applier (P1.5 new-root policy).
	applied = nil
	rootPlan := &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{{Key: "root", Title: "root"}, {Key: "child", Title: "child", ParentKey: "root"}},
	}
	if _, err := handle.ApplyGraphPlan(context.Background(), rootPlan); err != nil {
		t.Fatalf("un-anchored apply: %v", err)
	}
	if len(applied) != 1 || applied[0].leg != "legacy" {
		t.Fatalf("un-anchored plan routed to %+v, want legacy", applied)
	}

	// Mixed anchors → rejected, neither leg materializes.
	applied = nil
	mixedPlan := &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{{Key: "n1", Title: "step", ParentID: lAnchor}},
		Edges: []beads.GraphApplyEdge{{FromID: jAnchor, ToKey: "n1", Type: "blocks"}},
	}
	if _, err := handle.ApplyGraphPlan(context.Background(), mixedPlan); !errors.Is(err, errCrossResidenceDependency) {
		t.Fatalf("mixed-anchor apply err = %v, want errCrossResidenceDependency", err)
	}
	if len(applied) != 0 {
		t.Fatalf("mixed-anchor plan materialized a leg: %+v", applied)
	}
}

func TestGraphJournalSeamInertOnV1V2Paths(t *testing.T) {
	// An opted city at P1 exit: the scope is present but the journal is empty.
	// v1/v2 traffic (an un-anchored ApplyGraphPlan — the molecule materialization
	// shape) must land wholly on the legacy leg, and global reads must equal the
	// legacy leg's own results.
	var applied []appliedPlan
	journal := newResidenceApplyLeg("journal", 0, &applied)
	legacy := newResidenceApplyLeg("legacy", 1000, &applied)
	router := newResidenceRoutingGraphStore(journal, legacy)

	// Seed the legacy leg with a v2-style molecule bead; leave journal empty.
	legacyRoot := plant(t, legacy.residenceLegStore, "mol-root")

	handle, _ := router.GraphApplyHandle()
	molPlan := &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "mol"},
			{Key: "step", Title: "step", ParentKey: "root"},
		},
	}
	if _, err := handle.ApplyGraphPlan(context.Background(), molPlan); err != nil {
		t.Fatalf("molecule-shaped apply: %v", err)
	}
	if len(applied) != 1 || applied[0].leg != "legacy" {
		t.Fatalf("molecule materialization did not land on the legacy leg: %+v", applied)
	}

	routerReady, err := router.Ready()
	if err != nil {
		t.Fatalf("router Ready: %v", err)
	}
	legacyReady, err := legacy.Ready()
	if err != nil {
		t.Fatalf("legacy Ready: %v", err)
	}
	if len(routerReady) != len(legacyReady) || len(routerReady) != 1 || routerReady[0].ID != legacyRoot {
		t.Fatalf("router Ready %+v != legacy Ready %+v (empty journal must be inert)", routerReady, legacyReady)
	}
}

// cannedReadLeg returns preset rows from the global-read fan-out methods,
// bypassing MemStore.Create (which stamps CreatedAt=now), so a test can pin
// exact priorities and creation times. Copies are returned so the router's
// in-place merge-sort never mutates the canned fixtures across calls.
type cannedReadLeg struct {
	*residenceLegStore
	ready []beads.Bead
	list  []beads.Bead
}

func (l *cannedReadLeg) Ready(_ ...beads.ReadyQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), l.ready...), nil
}

func (l *cannedReadLeg) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), l.list...), nil
}

// TestGraphResidenceFanOutGlobalSortAndLimit pins MEDIUM-3: a fan-out over both
// legs merge-sorts into one global order (not legacy-then-journal) and caps the
// union at the query Limit (not up to 2×Limit). Journal rows must be able to
// sort BEFORE legacy rows by priority/created_at.
func TestGraphResidenceFanOutGlobalSortAndLimit(t *testing.T) {
	p := func(i int) *int { return &i }
	ts := func(s int) time.Time { return time.Unix(int64(s), 0).UTC() }

	legacy := &cannedReadLeg{
		residenceLegStore: newResidenceLegStore("legacy", 1000),
		ready: []beads.Bead{
			{ID: "L1", Priority: p(2), CreatedAt: ts(30)},
			{ID: "L2", Priority: p(0), CreatedAt: ts(50)},
		},
		list: []beads.Bead{
			{ID: "L1", CreatedAt: ts(30)},
			{ID: "L2", CreatedAt: ts(50)},
		},
	}
	journal := &cannedReadLeg{
		residenceLegStore: newResidenceLegStore("journal", 0),
		ready: []beads.Bead{
			{ID: "J1", Priority: p(1), CreatedAt: ts(10)},
			{ID: "J2", Priority: p(0), CreatedAt: ts(20)},
		},
		list: []beads.Bead{
			{ID: "J1", CreatedAt: ts(10)},
			{ID: "J2", CreatedAt: ts(20)},
		},
	}
	router := newResidenceRoutingGraphStore(journal, legacy)

	// Ready order is (priority asc, created asc, id). Global: J2(p0,t20),
	// L2(p0,t50), J1(p1,t10), L1(p2,t30). Limit 3 truncates the 4-row union.
	ready, err := router.Ready(beads.ReadyQuery{Limit: 3})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if gotIDs := beadIDs(ready); !slices.Equal(gotIDs, []string{"J2", "L2", "J1"}) {
		t.Fatalf("Ready global order/cap = %v, want [J2 L2 J1]", gotIDs)
	}

	// List with SortCreatedAsc: J1(t10), J2(t20), L1(t30), L2(t50). Limit 3.
	list, err := router.List(beads.ListQuery{AllowScan: true, Sort: beads.SortCreatedAsc, Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotIDs := beadIDs(list); !slices.Equal(gotIDs, []string{"J1", "J2", "L1"}) {
		t.Fatalf("List global order/cap = %v, want [J1 J2 L1]", gotIDs)
	}
}

func beadIDs(rows []beads.Bead) []string {
	ids := make([]string, len(rows))
	for i, b := range rows {
		ids[i] = b.ID
	}
	return ids
}

// handleTrackingLeg records whether a read went through the leg's explicit LIVE
// tier handle vs a plain (cache-tier) Store.Get, so a test can prove residence-
// routed live reads reach the owning leg's live handle rather than degrading to
// a cached read.
type handleTrackingLeg struct {
	*beads.MemStore
	liveGets  []string
	plainGets []string
}

func newHandleTrackingLeg(seqStart int) *handleTrackingLeg {
	return &handleTrackingLeg{MemStore: beads.NewMemStoreFrom(seqStart, nil, nil)}
}

// Get is the plain Store.Get — the cache-tier path a logical wrapper would use.
func (l *handleTrackingLeg) Get(id string) (beads.Bead, error) {
	l.plainGets = append(l.plainGets, id)
	return l.MemStore.Get(id)
}

func (l *handleTrackingLeg) Handles() beads.StoreHandles {
	return beads.StoreHandles{
		Cached: trackingTierReader{l: l, live: false},
		Live:   trackingTierReader{l: l, live: true},
		Writer: l,
	}
}

type trackingTierReader struct {
	l    *handleTrackingLeg
	live bool
}

func (r trackingTierReader) Get(id string) (beads.Bead, error) {
	if r.live {
		r.l.liveGets = append(r.l.liveGets, id)
	}
	return r.l.MemStore.Get(id)
}

func (r trackingTierReader) List(q beads.ListQuery) ([]beads.Bead, error) {
	return r.l.List(q)
}

func (r trackingTierReader) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	return r.l.Ready(q...)
}

func (r trackingTierReader) DepList(id, direction string) ([]beads.Dep, error) {
	return r.l.DepList(id, direction)
}

// TestGraphResidenceHandlesRouteLiveReadsToOwningLeg pins MEDIUM-1b:
// HandlesFor(router).Live.Get residence-routes to the OWNING leg's live handle —
// a journal id to the journal leg's live handle, a legacy id to the legacy leg's
// live handle — instead of degrading a legacy read to the legacy leg's plain
// (cache-tier) Get. The plain-Get degradation is exactly what breaks
// wisp_autoclose.go's live parent/attachment reads.
func TestGraphResidenceHandlesRouteLiveReadsToOwningLeg(t *testing.T) {
	journal := newHandleTrackingLeg(0)
	legacy := newHandleTrackingLeg(1000)
	jb, err := journal.Create(beads.Bead{Title: "journal-bead"})
	if err != nil {
		t.Fatalf("plant journal bead: %v", err)
	}
	lb, err := legacy.Create(beads.Bead{Title: "legacy-bead"})
	if err != nil {
		t.Fatalf("plant legacy bead: %v", err)
	}
	router := newResidenceRoutingGraphStore(journal, legacy)
	journal.liveGets, journal.plainGets = nil, nil
	legacy.liveGets, legacy.plainGets = nil, nil

	handles := beads.HandlesFor(router)

	if _, err := handles.Live.Get(jb.ID); err != nil {
		t.Fatalf("Live.Get(journal id): %v", err)
	}
	if !slices.Contains(journal.liveGets, jb.ID) {
		t.Fatalf("journal id did not reach the journal leg's live handle: liveGets=%v", journal.liveGets)
	}

	if _, err := handles.Live.Get(lb.ID); err != nil {
		t.Fatalf("Live.Get(legacy id): %v", err)
	}
	if !slices.Contains(legacy.liveGets, lb.ID) {
		t.Fatalf("legacy id did not reach the legacy leg's LIVE handle: liveGets=%v", legacy.liveGets)
	}
	if slices.Contains(legacy.plainGets, lb.ID) {
		t.Fatalf("legacy id degraded to a plain/cache-tier Get: plainGets=%v", legacy.plainGets)
	}
}
