package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// uncomparableReadyStore deliberately has an uncomparable dynamic value. A
// beads.Store interface containing this value panics if code compares it or
// uses it as a map key. Store identity in one reconcile pass must instead come
// from the explicit logical scope supplied by the caller.
type uncomparableReadyStore struct {
	beads.Store
	marker []byte
}

func (s uncomparableReadyStore) ReadyDemandStoreIdentity() string {
	return string(s.marker)
}

type opaqueUncomparableReadyStore struct {
	beads.Store
	marker []byte
}

type readyIdentityError struct {
	code string
}

func (e *readyIdentityError) Error() string { return "ready identity error: " + e.code }

type readyIdentityErrorStore struct {
	beads.Store
	err error
}

type canonicalizationAttemptStore struct {
	*readyQueryRecordingStore
	commit    bool
	updateErr error
	updates   atomic.Int64
}

func (s *canonicalizationAttemptStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates.Add(1)
	if s.commit {
		if err := s.readyQueryRecordingStore.Update(id, opts); err != nil {
			return err
		}
	}
	return s.updateErr
}

type readyResultStore struct {
	beads.Store
	rows  []beads.Bead
	err   error
	calls *atomic.Int64
}

func (s *readyResultStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.calls != nil {
		s.calls.Add(1)
	}
	rows := make([]beads.Bead, len(s.rows))
	for i := range s.rows {
		rows[i] = beads.CloneBead(s.rows[i])
	}
	return rows, s.err
}

type blockingReadySnapshotStore struct {
	beads.Store
	rows    []beads.Bead
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type atomicReadyCountingStore struct {
	*beads.MemStore
	calls atomic.Int64
}

type atomicReadyErrorStore struct {
	*beads.MemStore
	calls atomic.Int64
	err   error
}

func (s *atomicReadyErrorStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	s.calls.Add(1)
	return nil, s.err
}

type openListAfterFirstErrorStore struct {
	*beads.MemStore
	openLists atomic.Int64
	err       error
}

func (s *openListAfterFirstErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status == "open" && query.Type == "" && s.openLists.Add(1) > 1 {
		return nil, s.err
	}
	return s.MemStore.List(query)
}

type postRepairOpenListReader struct {
	beads.Store
	err     error
	partial bool
	calls   atomic.Int64
}

func (s *postRepairOpenListReader) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil || query.Status != "open" {
		return rows, err
	}
	s.calls.Add(1)
	if s.partial {
		return rows, &beads.PartialResultError{Op: "post-repair List(open)", Err: s.err}
	}
	return nil, s.err
}

type partialOpenAfterFirstListReader struct {
	beads.Store
	cause     error
	openCalls atomic.Int64
}

func (s *partialOpenAfterFirstListReader) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil || query.Status != "open" || query.Type != "" {
		return rows, err
	}
	if s.openCalls.Add(1) > 1 {
		return rows, &beads.PartialResultError{Op: "untouched partial List(open)", Err: s.cause}
	}
	return rows, nil
}

func (s *atomicReadyCountingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.calls.Add(1)
	return s.MemStore.Ready(query...)
}

func (s *blockingReadySnapshotStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return append([]beads.Bead(nil), s.rows...), nil
}

func (s readyIdentityErrorStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, s.err
}

// readyPartialLiveStore returns its rows alongside a PartialResultError, to
// exercise the cached controller-demand read's tier-merge branch.
type readyPartialLiveStore struct {
	beads.Store
	rows []beads.Bead
}

func (s *readyPartialLiveStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	out := append([]beads.Bead(nil), s.rows...)
	return out, &beads.PartialResultError{Op: "bd ready", Err: errors.New("skipped corrupt bead")}
}

// seedReadyWork creates an open, unblocked work bead assigned to assignee.
func seedReadyWork(t *testing.T, store beads.Store, title, assignee string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "open",
		Assignee: assignee,
	})
	if err != nil {
		t.Fatalf("create ready work %q: %v", title, err)
	}
	return b
}

func readyIDs(rows []beads.Bead) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestReadyDemandCacheCollapsesReadyFanout proves the per-pass cache turns the
// N-assignee live-Ready fan-out (plus the scale-check and named-session probes)
// into at most one backing read per tier for a single store, instead of one
// read per assignee/probe. This is the core of the reconcile-tick perf fix:
// before the cache one pass issued ~60 sequential /beads/ready reads.
func TestReadyDemandCacheCollapsesReadyFanout(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	for _, assignee := range []string{"worker-a", "worker-b", "worker-c", "worker-d"} {
		seedReadyWork(t, store, "work for "+assignee, assignee)
	}

	cache := newReadyDemandCache()

	// Assigned-work probe: one live read per assignee in the legacy path.
	for _, assignee := range []string{"worker-a", "worker-b", "worker-c", "worker-d"} {
		if _, err := cache.liveReady(readyDemandCityStoreScope(), store, beads.ReadyQuery{Assignee: assignee, Limit: 5}); err != nil {
			t.Fatalf("liveReady(%q): %v", assignee, err)
		}
	}
	// Assigned-work no-assignee probe.
	if _, err := cache.liveReady(readyDemandCityStoreScope(), store, beads.ReadyQuery{Limit: 5}); err != nil {
		t.Fatalf("liveReady(no assignee): %v", err)
	}
	// Scale-check + named-session probes: full ready set, repeated per group.
	for i := 0; i < 3; i++ {
		if _, err := cache.controllerDemandReady(readyDemandCityStoreScope(), store); err != nil {
			t.Fatalf("controllerDemandReady #%d: %v", i, err)
		}
	}

	// A plain store has no explicit cached/live split, so both the live snapshot
	// and the cached snapshot resolve to store.Ready — at most two backing reads
	// total, regardless of how many assignees or probe groups asked.
	if got := len(store.readyQueries); got != 1 {
		t.Fatalf("backing Ready reads = %d, want exactly 1 for an untouched store across the whole demand phase: %#v", got, store.readyQueries)
	}
}

func TestReadyDemandCacheUsesExplicitIdentityForUncomparableStore(t *testing.T) {
	backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	seedReadyWork(t, backing, "work", "worker-a")
	store := uncomparableReadyStore{Store: backing, marker: []byte("not comparable")}
	cache := newReadyDemandCache()

	for i := 0; i < 3; i++ {
		rows, err := cache.liveReady(readyDemandCityStoreScope(), store, beads.ReadyQuery{Assignee: "worker-a", Limit: 1})
		if err != nil {
			t.Fatalf("liveReady #%d: %v", i, err)
		}
		if len(rows) != 1 {
			t.Fatalf("liveReady #%d rows = %d, want 1", i, len(rows))
		}
	}
	if got := len(backing.readyQueries); got != 1 {
		t.Fatalf("backing Ready reads = %d, want 1", got)
	}
}

func TestReadyDemandCacheReturnsDeeplyImmutableBoundedViews(t *testing.T) {
	priority := 7
	blocked := false
	deferUntil := time.Now().UTC().Add(time.Hour)
	row := beads.Bead{
		ID:         "work-1",
		Status:     "open",
		Assignee:   "worker-a",
		Priority:   &priority,
		IsBlocked:  &blocked,
		DeferUntil: &deferUntil,
		Metadata:   map[string]string{"route": "original"},
		Labels:     []string{"original"},
		Needs:      []string{"blocks:dep-1"},
		Dependencies: []beads.Dep{{
			IssueID:     "work-1",
			DependsOnID: "dep-1",
			Type:        "blocks",
		}},
	}
	store := &readyStaticStore{Store: beads.NewMemStore(), ready: []beads.Bead{row, {ID: "work-2", Status: "open", Assignee: "worker-a"}}}
	cache := newReadyDemandCache()

	first, err := cache.liveReady(readyDemandCityStoreScope(), store, beads.ReadyQuery{Assignee: "worker-a", Limit: 1})
	if err != nil {
		t.Fatalf("first liveReady: %v", err)
	}
	if len(first) != 1 || cap(first) != 1 {
		t.Fatalf("first view len/cap = %d/%d, want 1/1", len(first), cap(first))
	}
	first[0].ID = "mutated"
	first[0].Metadata["route"] = "mutated"
	first[0].Labels[0] = "mutated"
	first[0].Needs[0] = "mutated"
	first[0].Dependencies[0].DependsOnID = "mutated"
	*first[0].Priority = 99
	*first[0].IsBlocked = true
	mutatedTime := time.Time{}
	*first[0].DeferUntil = mutatedTime
	// Mutating the provider-owned row after publication must not mutate the
	// installed memo either.
	store.ready[0].Metadata["route"] = "provider-mutated"

	second, err := cache.liveReady(readyDemandCityStoreScope(), store, beads.ReadyQuery{Assignee: "worker-a", Limit: 1})
	if err != nil {
		t.Fatalf("second liveReady: %v", err)
	}
	got := second[0]
	if got.ID != "work-1" || got.Metadata["route"] != "original" || got.Labels[0] != "original" || got.Needs[0] != "blocks:dep-1" || got.Dependencies[0].DependsOnID != "dep-1" {
		t.Fatalf("second view was mutated through an alias: %#v", got)
	}
	if got.Priority == nil || *got.Priority != 7 || got.IsBlocked == nil || *got.IsBlocked || got.DeferUntil == nil || !got.DeferUntil.Equal(deferUntil) {
		t.Fatalf("second view pointer fields were mutated through an alias: %#v", got)
	}

	full, err := cache.controllerDemandReady(readyDemandCityStoreScope(), store)
	if err != nil {
		t.Fatalf("controllerDemandReady: %v", err)
	}
	full[0].Metadata["route"] = "full-view-mutated"
	full = append(full, beads.Bead{ID: "consumer-appended"})
	if len(full) != 3 {
		t.Fatalf("consumer append len = %d, want 3", len(full))
	}
	fullAgain, err := cache.controllerDemandReady(readyDemandCityStoreScope(), store)
	if err != nil {
		t.Fatalf("controllerDemandReady again: %v", err)
	}
	if len(fullAgain) != 2 || fullAgain[0].Metadata["route"] != "original" {
		t.Fatalf("full snapshot was mutated through returned view: %#v", fullAgain)
	}
}

func TestReadyDemandCacheInvalidatesOnlyAttemptedStore(t *testing.T) {
	storeA := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	storeB := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	seedReadyWork(t, storeA, "a-before", "")
	seedReadyWork(t, storeB, "b-before", "")
	cache := newReadyDemandCache()

	if _, err := cache.liveReady(readyDemandRigStoreScope("a"), storeA, beads.ReadyQuery{Limit: 10}); err != nil {
		t.Fatalf("pre snapshot A: %v", err)
	}
	if _, err := cache.liveReady(readyDemandRigStoreScope("b"), storeB, beads.ReadyQuery{Limit: 10}); err != nil {
		t.Fatalf("pre snapshot B: %v", err)
	}
	seedReadyWork(t, storeA, "a-after", "")
	seedReadyWork(t, storeB, "b-external-after-publication", "")
	cache.storeHandle(readyDemandRigStoreScope("a"), storeA).invalidateBeforeMutationAttempt()

	rowsA, err := cache.controllerDemandReady(readyDemandRigStoreScope("a"), storeA)
	if err != nil {
		t.Fatalf("post snapshot A: %v", err)
	}
	rowsB, err := cache.controllerDemandReady(readyDemandRigStoreScope("b"), storeB)
	if err != nil {
		t.Fatalf("reused snapshot B: %v", err)
	}
	if len(rowsA) != 2 {
		t.Fatalf("post snapshot A rows = %v, want both generations", readyIDs(rowsA))
	}
	if len(rowsB) != 1 {
		t.Fatalf("untouched snapshot B rows = %v, want frozen pre-write generation", readyIDs(rowsB))
	}
	if got := len(storeA.readyQueries); got != 2 {
		t.Fatalf("store A Ready reads = %d, want pre+post = 2", got)
	}
	if got := len(storeB.readyQueries); got != 1 {
		t.Fatalf("store B Ready reads = %d, want reused pre = 1", got)
	}
	nextPassRows, err := newReadyDemandCache().controllerDemandReady(readyDemandRigStoreScope("b"), storeB)
	if err != nil {
		t.Fatalf("next-pass snapshot B: %v", err)
	}
	if len(nextPassRows) != 2 {
		t.Fatalf("next-pass snapshot B rows = %v, want external write visible", readyIDs(nextPassRows))
	}
}

func TestReadyDemandCachePreservesErrorIdentityAcrossConsumers(t *testing.T) {
	want := &readyIdentityError{code: "RC-PERF-002"}
	store := readyIdentityErrorStore{Store: beads.NewMemStore(), err: want}
	cache := newReadyDemandCache()

	for i := 0; i < 3; i++ {
		_, err := cache.liveReady(readyDemandCityStoreScope(), store, beads.ReadyQuery{Limit: 1})
		if !errors.Is(err, want) {
			t.Fatalf("liveReady #%d error = %v, want errors.Is sentinel", i, err)
		}
		var typed *readyIdentityError
		if !errors.As(err, &typed) || typed != want {
			t.Fatalf("liveReady #%d lost errors.As identity: %v", i, err)
		}
	}
	_, err := cache.controllerDemandReady(readyDemandCityStoreScope(), store)
	if !errors.Is(err, want) {
		t.Fatalf("controllerDemandReady error = %v, want errors.Is sentinel", err)
	}
	var typed *readyIdentityError
	if !errors.As(err, &typed) || typed != want {
		t.Fatalf("controllerDemandReady lost errors.As identity: %v", err)
	}
}

func TestReadyDemandCacheReportsOneDemandErrorPerStoreGeneration(t *testing.T) {
	want := &readyIdentityError{code: "one-diagnostic-per-generation"}
	store := readyIdentityErrorStore{Store: beads.NewMemStore(), err: want}
	cache := newReadyDemandCache()
	handle := cache.storeHandle(readyDemandCityStoreScope(), store)
	targets := []defaultScaleCheckTarget{{
		template:   "worker",
		storeKey:   "city",
		store:      store,
		readyStore: handle,
	}}

	_, _, scalePartial, scaleErrs := defaultScaleCheckCountsAndDemand(targets, cache)
	_, namedPartial, namedErrs := defaultNamedSessionDemand(targets, &config.City{}, "test-city", cache)
	if !scalePartial["worker"] || !namedPartial["worker"] {
		t.Fatalf("semantic partials lost while deduplicating diagnostics: scale=%v named=%v", scalePartial, namedPartial)
	}
	allErrs := append(append([]error(nil), scaleErrs...), namedErrs...)
	if len(allErrs) != 1 {
		t.Fatalf("reported demand errors = %d, want one for one store/generation: scale=%v named=%v", len(allErrs), scaleErrs, namedErrs)
	}
	if !errors.Is(allErrs[0], want) {
		t.Fatalf("reported error = %v, want errors.Is sentinel", allErrs[0])
	}

	handle.invalidateBeforeMutationAttempt()
	_, _, _, nextErrs := defaultScaleCheckCountsAndDemand(targets, cache)
	if len(nextErrs) != 1 || !errors.Is(nextErrs[0], want) {
		t.Fatalf("next generation errors = %v, want one new report preserving identity", nextErrs)
	}
}

func TestReadyDemandCacheReportsOneOpenErrorPerStoreGeneration(t *testing.T) {
	wantErr := &readyIdentityError{code: "one-open-diagnostic-per-generation"}
	base := beads.NewMemStore()
	reader := &postRepairOpenListReader{Store: base, err: wantErr}
	store := &controllerDemandHandlesStore{
		Store: base,
		handles: beads.StoreHandles{
			Cached: reader,
			Live:   reader,
			Writer: base,
		},
	}
	cache := newReadyDemandCache()
	handleA := cache.storeHandle(readyDemandCityStoreScope(), store)
	handleB := cache.storeHandle(readyDemandRigStoreScope("a"), store)
	if !sameReadyDemandStore(handleA, handleB) {
		t.Fatal("exact pointer alias did not share one open-work generation")
	}

	_, firstReport, firstErr := handleA.openWorkSnapshotForReport()
	_, secondReport, secondErr := handleB.openWorkSnapshotForReport()
	if !firstReport || secondReport {
		t.Fatalf("report claims = first:%v second:%v, want true/false", firstReport, secondReport)
	}
	if !errors.Is(firstErr, wantErr) || !errors.Is(secondErr, wantErr) {
		t.Fatalf("open-work errors lost identity: first=%v second=%v", firstErr, secondErr)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("open-work List calls = %d, want 1", got)
	}

	handleA.invalidateBeforeMutationAttempt()
	_, nextReport, nextErr := handleB.openWorkSnapshotForReport()
	if !nextReport || !errors.Is(nextErr, wantErr) {
		t.Fatalf("next generation report=%v error=%v, want one new identity-preserving report", nextReport, nextErr)
	}
	if got := reader.calls.Load(); got != 2 {
		t.Fatalf("open-work List calls after invalidation = %d, want 2", got)
	}
}

func TestCollectAssignedWorkReportsOneReadyErrorPerAliasedStoreGeneration(t *testing.T) {
	wantErr := errors.New("unique-aliased-assignment-ready-error")
	store := &atomicReadyErrorStore{MemStore: beads.NewMemStore(), err: wantErr}
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "rig-A", Path: "/tmp/rig-A"},
		{Name: "rig-B", Path: "/tmp/rig-B"},
	}}
	cache := newReadyDemandCache()
	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	_, _, _, _, partial := collectAssignedWorkBeadsWithStores(
		cfg,
		store,
		map[string]beads.Store{"rig-A": store, "rig-B": store},
		nil,
		newSessionBeadSnapshot(nil),
		cache,
	)
	if !partial {
		t.Fatal("StoreQueryPartial source flag = false, want true for Ready error")
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("aliased backing Ready calls = %d, want 1", got)
	}
	if got := strings.Count(logs.String(), wantErr.Error()); got != 1 {
		t.Fatalf("assignment Ready diagnostic count = %d, want 1 per physical store/generation; logs=%s", got, logs.String())
	}
}

func TestReadyDemandCacheExplicitValueIdentityControlsAliasing(t *testing.T) {
	backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	seedReadyWork(t, backing, "shared", "")
	cache := newReadyDemandCache()

	first := uncomparableReadyStore{Store: backing, marker: []byte("first")}
	second := uncomparableReadyStore{Store: backing, marker: []byte("second")}
	firstHandle := cache.storeHandle(readyDemandRigStoreScope("a"), first)
	secondHandle := cache.storeHandle(readyDemandRigStoreScope("b"), second)
	if sameReadyDemandStore(firstHandle, secondHandle) {
		t.Fatal("distinct explicit identity tokens aliased one handle")
	}
	if _, err := firstHandle.controllerDemandReady(); err != nil {
		t.Fatal(err)
	}
	if _, err := secondHandle.controllerDemandReady(); err != nil {
		t.Fatal(err)
	}
	if got := len(backing.readyQueries); got != 2 {
		t.Fatalf("distinct explicit identities issued %d Ready reads, want 2", got)
	}
}

func TestReadyDemandCacheOpaqueValueIdentityIsExposedToTraceAccounting(t *testing.T) {
	backing := beads.NewMemStore()
	store := opaqueUncomparableReadyStore{Store: backing, marker: []byte("opaque")}
	cache := newReadyDemandCache()
	cache.storeHandle(readyDemandCityStoreScope(), store)
	cache.storeHandle(readyDemandRigStoreScope("a"), store)

	if got := cache.opaqueScopeCount(); got != 2 {
		t.Fatalf("opaque identity scope count = %d, want 2 independently treated scopes", got)
	}

	emptyIdentity := uncomparableReadyStore{Store: backing, marker: []byte("  ")}
	cache.storeHandle(readyDemandRigStoreScope("empty-token"), emptyIdentity)
	if got := cache.invalidIdentityScopeCount(); got != 1 {
		t.Fatalf("invalid declared identity scope count = %d, want 1", got)
	}
}

func TestDefaultScaleDemandKeepsLogicalStoreRefsWhenSnapshotHandleIsShared(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	ids := make(map[string]string)
	for _, target := range []string{"rig-A/planner", "rig-B/planner"} {
		created, err := store.Create(beads.Bead{
			Title:    target,
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": target},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[target] = created.ID
	}
	cache := newReadyDemandCache()
	handleA := cache.storeHandle(readyDemandRigStoreScope("rig-A"), store)
	handleB := cache.storeHandle(readyDemandRigStoreScope("rig-B"), store)
	if !sameReadyDemandStore(handleA, handleB) {
		t.Fatal("exact pointer alias did not share a snapshot handle")
	}
	targets := []defaultScaleCheckTarget{
		{template: "rig-A/planner", storeKey: "rig-A", store: store, readyStore: handleA},
		{template: "rig-B/planner", storeKey: "rig-B", store: store, readyStore: handleB},
	}

	counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(targets, cache)
	if len(errs) != 0 || len(partial) != 0 {
		t.Fatalf("shared-handle demand returned partial=%v errors=%v", partial, errs)
	}
	for _, target := range []string{"rig-A/planner", "rig-B/planner"} {
		if counts[target] != 1 {
			t.Errorf("count[%q] = %d, want 1", target, counts[target])
		}
		wantRef := strings.Split(target, "/")[0]
		if got := demand[target].StoreRefs[ids[target]]; got != wantRef {
			t.Errorf("StoreRefs[%q][%q] = %q, want logical ref %q", target, ids[target], got, wantRef)
		}
	}
	if got := len(store.readyQueries); got != 1 {
		t.Fatalf("shared physical Ready reads = %d, want 1", got)
	}
}

func TestDefaultScaleDemandPreservesTargetStoreOrder(t *testing.T) {
	storeWithID := func(id string) *readyQueryRecordingStore {
		return &readyQueryRecordingStore{MemStore: beads.NewMemStoreFrom(1, []beads.Bead{{
			ID:       id,
			Title:    id,
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		}}, nil)}
	}
	storeA := storeWithID("work-a")
	storeB := storeWithID("work-b")
	targets := []defaultScaleCheckTarget{
		{template: "worker", storeKey: "rig:a", store: storeA},
		{template: "worker", storeKey: "rig:b", store: storeB},
	}

	for i := 0; i < 100; i++ {
		_, demand, partial, errs := defaultScaleCheckCountsAndDemand(targets, newReadyDemandCache())
		if len(errs) != 0 || len(partial) != 0 {
			t.Fatalf("iteration %d returned partial=%v errors=%v", i, partial, errs)
		}
		if got := demand["worker"].WorkBeadIDs; !reflect.DeepEqual(got, []string{"work-a", "work-b"}) {
			t.Fatalf("iteration %d WorkBeadIDs = %v, want target-store order [work-a work-b]", i, got)
		}
	}
}

func TestReadyDemandCacheConcurrentConsumersFetchOnce(t *testing.T) {
	store := &blockingReadySnapshotStore{
		Store:   beads.NewMemStore(),
		rows:    []beads.Bead{{ID: "work-1", Status: "open"}},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	cache := newReadyDemandCache()
	const consumers = 32
	start := make(chan struct{})
	errs := make(chan error, consumers)
	var wg sync.WaitGroup
	for i := 0; i < consumers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rows, err := cache.liveReady(readyDemandCityStoreScope(), store, beads.ReadyQuery{Limit: 1})
			if err == nil && len(rows) != 1 {
				err = fmt.Errorf("rows = %d, want 1", len(rows))
			}
			errs <- err
		}()
	}
	close(start)
	<-store.entered
	close(store.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("concurrent backing Ready calls = %d, want 1", got)
	}
}

func TestReadyDemandReadIntentForcesLaterMutationGeneration(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	seedReadyWork(t, store, "before", "")
	cache := newReadyDemandCache()
	handle := cache.storeHandle(readyDemandRigStoreScope("scope"), store)

	// A prior mutation already established the post-boundary generation.
	handle.invalidateBeforeMutationAttempt()
	selected := handle.generationForRead()
	// The reader has selected the generation but has not entered the provider.
	// A mutation attempt must still swap rather than reuse that generation.
	handle.invalidateBeforeMutationAttempt()
	if current := handle.generationForRead(); current == selected {
		t.Fatal("mutation reused a generation after read intent was published")
	}
	seedReadyWork(t, store, "after", "")

	// Let the old in-flight reader publish late. Its result must remain confined
	// to the old entry and cannot overwrite the current generation.
	if _, err := handle.liveSnapshot(selected); err != nil {
		t.Fatalf("old generation read: %v", err)
	}
	post, err := handle.controllerDemandReady()
	if err != nil {
		t.Fatalf("current generation read: %v", err)
	}
	if len(post) != 2 {
		t.Fatalf("current generation rows = %v, want authoritative post-write rows", readyIDs(post))
	}
	if got := len(store.readyQueries); got != 2 {
		t.Fatalf("Ready calls = %d, want isolated old+new generation reads", got)
	}
}

func TestCanonicalizationAttemptsAdvanceOnlyTheAttemptedStoreGeneration(t *testing.T) {
	wantErr := errors.New("update acknowledgement lost")
	tests := []struct {
		name      string
		commit    bool
		updateErr error
		wantRoute string
	}{
		{name: "success", commit: true, wantRoute: "rig-A/worker"},
		{name: "definite rejection", updateErr: wantErr, wantRoute: "rig-A/core.worker"},
		{name: "commit then error", commit: true, updateErr: wantErr, wantRoute: "rig-A/worker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
			created, err := backing.Create(beads.Bead{
				Title:  "legacy route",
				Type:   "task",
				Status: "open",
				Metadata: map[string]string{
					"gc.routed_to": "rig-A/core.worker",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			store := &canonicalizationAttemptStore{readyQueryRecordingStore: backing, commit: tt.commit, updateErr: tt.updateErr}
			cache := newReadyDemandCache()
			if _, err := cache.liveReady(readyDemandRigStoreScope("rig-A"), store, beads.ReadyQuery{Limit: 10}); err != nil {
				t.Fatalf("pre Ready: %v", err)
			}
			var stderr bytes.Buffer
			cfg := &config.City{Agents: []config.Agent{{
				Name:              "worker",
				Dir:               "rig-A",
				MaxActiveSessions: intPtr(1),
			}}}
			canonicalizeLegacyBoundUnassignedRoutedWorkWithReadyCache(
				cfg,
				[]beads.Bead{created},
				[]beads.Store{store},
				[]string{"rig-A"},
				cache,
				&stderr,
			)
			rows, err := cache.controllerDemandReady(readyDemandRigStoreScope("rig-A"), store)
			if err != nil {
				t.Fatalf("post Ready: %v", err)
			}
			if len(rows) != 1 || rows[0].Metadata["gc.routed_to"] != tt.wantRoute {
				t.Fatalf("post Ready rows = %#v, want route %q", rows, tt.wantRoute)
			}
			if got := len(backing.readyQueries); got != 2 {
				t.Fatalf("Ready calls = %d, want pre+post = 2", got)
			}
			if tt.updateErr != nil && !bytes.Contains(stderr.Bytes(), []byte(tt.updateErr.Error())) {
				t.Fatalf("stderr = %q, want update error", stderr.String())
			}
		})
	}
}

func TestBuildDesiredStatePostRepairOpenListErrorDoesNotSetGlobalPartial(t *testing.T) {
	for _, tt := range []struct {
		name    string
		partial bool
	}{
		{name: "hard error"},
		{name: "partial rows", partial: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			rigPath := filepath.Join(cityPath, "rigs", "rig-A")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}
			minSessions, maxSessions := 0, 1
			cfg := &config.City{
				Agents: []config.Agent{{
					Name:              "worker",
					Dir:               "rig-A",
					Provider:          "mock",
					MinActiveSessions: &minSessions,
					MaxActiveSessions: &maxSessions,
				}},
				Rigs:      []config.Rig{{Name: "rig-A", Path: rigPath}},
				Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
			}
			backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
			created, err := backing.Create(beads.Bead{
				Title:    "legacy routed work",
				Type:     "task",
				Status:   "open",
				Metadata: map[string]string{"gc.routed_to": "rig-A/core.worker"},
			})
			if err != nil {
				t.Fatal(err)
			}
			writer := &canonicalizationAttemptStore{readyQueryRecordingStore: backing, commit: true}
			wantErr := errors.New("post-repair authoritative open list unavailable")
			live := &postRepairOpenListReader{Store: writer, err: wantErr, partial: tt.partial}
			store := controllerDemandHandlesStore{
				Store: writer,
				handles: beads.StoreHandles{
					Cached: backing,
					Live:   live,
					Writer: writer,
				},
			}
			var stderr bytes.Buffer

			result := buildDesiredStateWithSessionBeads(
				"test-city", cityPath, time.Now(), cfg, &localMockProvider{},
				beads.NewMemStore(), map[string]beads.Store{"rig-A": store},
				newSessionBeadSnapshot(nil), nil, &stderr,
			)
			if result.StoreQueryPartial {
				t.Fatalf("StoreQueryPartial = true, want migration-only List error scoped away from global drain fence; stderr=%s", stderr.String())
			}
			if got := result.ScaleCheckCounts["rig-A/worker"]; got != 1 {
				t.Fatalf("post-repair scale demand = %d, want 1 from healthy Ready; stderr=%s", got, stderr.String())
			}
			persisted, err := backing.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := persisted.Metadata["gc.routed_to"]; got != "rig-A/worker" {
				t.Fatalf("persisted route = %q, want canonical route", got)
			}
			if got := writer.updates.Load(); got != 1 {
				t.Fatalf("canonicalization writes = %d, want 1", got)
			}
			if got := live.calls.Load(); got != 1 {
				t.Fatalf("post-repair Live.List calls = %d, want 1", got)
			}
			if tt.partial && strings.Contains(stderr.String(), "assignedWorkBeads: PARTIAL") {
				t.Fatalf("partial migration List error leaked into global assigned-work diagnostic: %s", stderr.String())
			}
			if !tt.partial && strings.Count(stderr.String(), wantErr.Error()) != 1 {
				t.Fatalf("hard post-repair error diagnostic count = %d, want 1; stderr=%s", strings.Count(stderr.String(), wantErr.Error()), stderr.String())
			}
		})
	}
}

func TestBuildDesiredStatePostRepairRereadKeepsUntouchedPartialOpenRows(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "rig-B")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	minSessions, maxSessions := 0, 1
	controlTemplate := config.ControlDispatcherAgentName
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name:              controlTemplate,
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MinActiveSessions: &minSessions,
				MaxActiveSessions: &maxSessions,
			},
			{
				Name:              "worker",
				Dir:               "rig-B",
				Provider:          "mock",
				MinActiveSessions: &minSessions,
				MaxActiveSessions: &maxSessions,
			},
		},
		Rigs:      []config.Rig{{Name: "rig-B", Path: rigPath}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}

	cityBacking := beads.NewMemStore()
	blocker, err := cityBacking.Create(beads.Bead{Title: "unfinished worker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	control, err := cityBacking.Create(beads.Bead{
		Title:  "blocked retry",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":      "retry",
			"gc.routed_to": controlTemplate,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cityBacking.DepAdd(control.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	partialReader := &partialOpenAfterFirstListReader{
		Store: cityBacking,
		cause: errors.New("one corrupt untouched open row"),
	}
	cityStore := &controllerDemandHandlesStore{
		Store: cityBacking,
		handles: beads.StoreHandles{
			Cached: partialReader,
			Live:   cityBacking,
			Writer: cityBacking,
		},
	}

	rigBacking := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	if _, err := rigBacking.Create(beads.Bead{
		Title:    "legacy rig route",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "rig-B/core.worker"},
	}); err != nil {
		t.Fatal(err)
	}
	rigStore := &canonicalizationAttemptStore{readyQueryRecordingStore: rigBacking, commit: true}
	var stderr bytes.Buffer

	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now(), cfg, &localMockProvider{},
		cityStore, map[string]beads.Store{"rig-B": rigStore},
		newSessionBeadSnapshot(nil), nil, &stderr,
	)
	if got := result.ScaleCheckCounts[controlTemplate]; got != 1 {
		t.Fatalf("control demand = %d, want untouched partial open row retained after other-store repair; stderr=%s", got, stderr.String())
	}
	if result.StoreQueryPartial {
		t.Fatalf("StoreQueryPartial = true, want migration-only partial List scoped away from global drain fence; stderr=%s", stderr.String())
	}
	if got := partialReader.openCalls.Load(); got != 2 {
		t.Fatalf("city Cached.List(open) provider calls = %d, want assigned scan + one memoized migration scan", got)
	}
}

func TestManyCanonicalizationAttemptsShareOnePostGeneration(t *testing.T) {
	backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	var work []beads.Bead
	for _, title := range []string{"legacy one", "legacy two", "legacy three"} {
		created, err := backing.Create(beads.Bead{
			Title:  title,
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": "rig-A/core.worker",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		work = append(work, created)
	}
	store := &canonicalizationAttemptStore{readyQueryRecordingStore: backing, commit: true}
	cache := newReadyDemandCache()
	if _, err := cache.liveReady(readyDemandRigStoreScope("rig-A"), store, beads.ReadyQuery{Limit: 10}); err != nil {
		t.Fatalf("pre Ready: %v", err)
	}
	stores := make([]beads.Store, len(work))
	refs := make([]string, len(work))
	for i := range work {
		stores[i] = store
		refs[i] = "rig-A"
	}
	cfg := &config.City{Agents: []config.Agent{{
		Name:              "worker",
		Dir:               "rig-A",
		MaxActiveSessions: intPtr(1),
	}}}
	canonicalizeLegacyBoundUnassignedRoutedWorkWithReadyCache(cfg, work, stores, refs, cache, io.Discard)
	rows, err := cache.controllerDemandReady(readyDemandRigStoreScope("rig-A"), store)
	if err != nil {
		t.Fatalf("post Ready: %v", err)
	}
	if len(rows) != len(work) {
		t.Fatalf("post Ready rows = %d, want %d", len(rows), len(work))
	}
	if got := len(backing.readyQueries); got != 2 {
		t.Fatalf("Ready calls after %d write attempts = %d, want pre+one post = 2", len(work), got)
	}
}

func TestReadyDemandCacheMatchesDirectCachedLiveMergeExactly(t *testing.T) {
	cachedFatal := &readyIdentityError{code: "cached-fatal"}
	liveFatal := &readyIdentityError{code: "live-fatal"}
	cachedPartialCause := &readyIdentityError{code: "cached-partial"}
	livePartialCause := &readyIdentityError{code: "live-partial"}
	cachedPartial := &beads.PartialResultError{Op: "cached ready", Err: cachedPartialCause}
	livePartial := &beads.PartialResultError{Op: "live ready", Err: livePartialCause}
	cacheUnavailable := fmt.Errorf("cache projection: %w", beads.ErrCacheUnavailable)

	tests := []struct {
		name       string
		cachedRows []beads.Bead
		cachedErr  error
		liveRows   []beads.Bead
		liveErr    error
		sentinels  []error
	}{
		{
			name:       "complete live authoritative",
			cachedRows: []beads.Bead{{ID: "cached", Status: "open", Metadata: map[string]string{"tier": "cached"}}},
			liveRows:   []beads.Bead{{ID: "live", Status: "open", Metadata: map[string]string{"tier": "live"}}},
		},
		{
			name:       "partial live backfilled live first",
			cachedRows: []beads.Bead{{ID: "cached", Status: "open"}, {ID: "shared", Status: "open", Metadata: map[string]string{"tier": "cached"}}},
			liveRows:   []beads.Bead{{ID: "live", Status: "open"}, {ID: "shared", Status: "open", Metadata: map[string]string{"tier": "live"}}},
			liveErr:    livePartial,
			sentinels:  []error{livePartialCause},
		},
		{
			name:       "both tiers partial preserve both causes",
			cachedRows: []beads.Bead{{ID: "cached", Status: "open"}, {ID: "shared", Status: "open", Metadata: map[string]string{"tier": "cached"}}},
			cachedErr:  cachedPartial,
			liveRows:   []beads.Bead{{ID: "live", Status: "open"}, {ID: "shared", Status: "open", Metadata: map[string]string{"tier": "live"}}},
			liveErr:    livePartial,
			sentinels:  []error{cachedPartialCause, livePartialCause},
		},
		{
			name:       "both tiers hard discard both row sets",
			cachedRows: []beads.Bead{{ID: "cached-must-drop", Status: "open"}},
			cachedErr:  cachedFatal,
			liveRows:   []beads.Bead{{ID: "live-must-drop", Status: "open"}},
			liveErr:    liveFatal,
			sentinels:  []error{cachedFatal, liveFatal},
		},
		{
			name:       "fatal live rows discarded and cached retained",
			cachedRows: []beads.Bead{{ID: "cached", Status: "open"}},
			liveRows:   []beads.Bead{{ID: "must-drop", Status: "open"}},
			liveErr:    liveFatal,
			sentinels:  []error{liveFatal},
		},
		{
			name:       "fatal cached rows discarded and partial live retained",
			cachedRows: []beads.Bead{{ID: "must-drop", Status: "open"}},
			cachedErr:  cachedFatal,
			liveRows:   []beads.Bead{{ID: "live", Status: "open"}},
			liveErr:    livePartial,
			sentinels:  []error{cachedFatal, livePartialCause},
		},
		{
			name:      "cache unavailable preserves live result and error",
			cachedErr: cacheUnavailable,
			liveRows:  []beads.Bead{{ID: "live", Status: "open"}},
			liveErr:   livePartial,
			sentinels: []error{livePartialCause},
		},
	}

	build := func(cachedRows []beads.Bead, cachedErr error, liveRows []beads.Bead, liveErr error, cachedCalls, liveCalls *atomic.Int64) beads.Store {
		base := beads.NewMemStore()
		return controllerDemandHandlesStore{
			Store: base,
			handles: beads.StoreHandles{
				Cached: &readyResultStore{Store: base, rows: cachedRows, err: cachedErr, calls: cachedCalls},
				Live:   &readyResultStore{Store: base, rows: liveRows, err: liveErr, calls: liveCalls},
				Writer: base,
			},
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var directCachedCalls, directLiveCalls atomic.Int64
			directStore := build(tt.cachedRows, tt.cachedErr, tt.liveRows, tt.liveErr, &directCachedCalls, &directLiveCalls)
			wantRows, wantErr := readyForControllerDemand(directStore)

			var cachedCalls, liveCalls atomic.Int64
			cachedStore := build(tt.cachedRows, tt.cachedErr, tt.liveRows, tt.liveErr, &cachedCalls, &liveCalls)
			cache := newReadyDemandCache()
			gotRows, gotErr := cache.controllerDemandReady(readyDemandCityStoreScope(), cachedStore)
			if !reflect.DeepEqual(gotRows, wantRows) {
				t.Fatalf("rows = %#v, want exact direct result %#v", gotRows, wantRows)
			}
			if beads.IsPartialResult(gotErr) != beads.IsPartialResult(wantErr) {
				t.Fatalf("partial classification = %v, want %v; got=%v wantErr=%v", beads.IsPartialResult(gotErr), beads.IsPartialResult(wantErr), gotErr, wantErr)
			}
			for _, sentinel := range tt.sentinels {
				if !errors.Is(gotErr, sentinel) || !errors.Is(wantErr, sentinel) {
					t.Fatalf("error identity for %v: got=%v direct=%v", sentinel, gotErr, wantErr)
				}
			}
			gotRowsAgain, gotErrAgain := cache.controllerDemandReady(readyDemandCityStoreScope(), cachedStore)
			if !reflect.DeepEqual(gotRowsAgain, wantRows) || !errors.Is(gotErrAgain, gotErr) {
				t.Fatalf("memoized result changed: rows=%#v err=%v (first err %v)", gotRowsAgain, gotErrAgain, gotErr)
			}
			if got := liveCalls.Load(); got != 1 {
				t.Fatalf("cached path live calls = %d, want 1", got)
			}
			if got := cachedCalls.Load(); got > 1 {
				t.Fatalf("cached path cached-tier calls = %d, want <=1", got)
			}
			if tt.liveErr == nil && cachedCalls.Load() != 0 {
				t.Fatalf("complete live path consulted cached tier %d times, want 0", cachedCalls.Load())
			}
		})
	}
}

// Coverage boundary for the snapshot-equivalence tests below: they exercise
// MemStore and CachingStore-over-MemStore, which filter the assignee entirely
// client-side. The wisp-bearing production stores (NativeDoltStore, BdStore)
// apply the assignee predicate server-side on BOTH the issue and wisp legs — the
// pinned beads@v1.1.0 readyWorkWispIssueFilter carries filter.Assignee into the
// wisp filter, emitting `assignee = ?` for the wisp table — so filtering an
// unfiltered snapshot by assignee is exact for them too (see the readyDemandCache
// doc in build_desired_state.go). That server-side path is not exercised here
// because beads.NewNativeDoltStoreForConformance is an internal test-only export
// of internal/beads and is not importable from cmd/gc; a full NativeDoltStore is
// likewise too heavy for this package's unit tests.

// TestReadyDemandCacheLiveReadyEquivalentToDirect proves the snapshot-filtered
// live read returns exactly what a direct assignee/limit-scoped Ready would, so
// the demand probes see the same beads they see today.
func TestReadyDemandCacheLiveReadyEquivalentToDirect(t *testing.T) {
	seed := func(store beads.Store) {
		seedReadyWork(t, store, "a1", "worker-a")
		seedReadyWork(t, store, "a2", "worker-a")
		seedReadyWork(t, store, "b1", "worker-b")
		seedReadyWork(t, store, "unassigned", "")
	}
	cached := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	oracle := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	seed(cached)
	seed(oracle)

	cache := newReadyDemandCache()
	queries := []beads.ReadyQuery{
		{},
		{Limit: 1},
		{Assignee: "worker-a"},
		{Assignee: "worker-a", Limit: 1},
		{Assignee: "worker-b", Limit: 5},
		{Assignee: "missing"},
	}
	for _, q := range queries {
		want, err := liveReadyForControllerDemandQuery(oracle, q)
		if err != nil {
			t.Fatalf("oracle liveReady %+v: %v", q, err)
		}
		got, err := cache.liveReady(readyDemandCityStoreScope(), cached, q)
		if err != nil {
			t.Fatalf("cache liveReady %+v: %v", q, err)
		}
		wantIDs := readyIDs(want)
		gotIDs := readyIDs(got)
		if len(wantIDs) != len(gotIDs) {
			t.Fatalf("liveReady %+v returned %v, want %v", q, gotIDs, wantIDs)
		}
		for i := range wantIDs {
			if wantIDs[i] != gotIDs[i] {
				t.Fatalf("liveReady %+v returned %v, want %v", q, gotIDs, wantIDs)
			}
		}
	}
}

// TestReadyDemandCacheControllerDemandEquivalentToDirect proves the cached
// controller-demand read matches the free function on both a plain store and a
// CachingStore-backed store (explicit cached/live handles + merge path).
func TestReadyDemandCacheControllerDemandEquivalentToDirect(t *testing.T) {
	t.Run("plain store", func(t *testing.T) {
		cached := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
		oracle := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
		for _, s := range []beads.Store{cached, oracle} {
			seedReadyWork(t, s, "w1", "worker-a")
			seedReadyWork(t, s, "w2", "")
		}
		want, err := readyForControllerDemand(oracle)
		if err != nil {
			t.Fatalf("oracle readyForControllerDemand: %v", err)
		}
		got, err := newReadyDemandCache().controllerDemandReady(readyDemandCityStoreScope(), cached)
		if err != nil {
			t.Fatalf("cache controllerDemandReady: %v", err)
		}
		if a, b := readyIDs(want), readyIDs(got); len(a) != len(b) {
			t.Fatalf("controllerDemandReady returned %v, want %v", b, a)
		}
	})

	t.Run("caching store", func(t *testing.T) {
		build := func() *beads.CachingStore {
			backing := beads.NewMemStore()
			if _, err := backing.Create(beads.Bead{Title: "routed", Type: "task", Status: "open"}); err != nil {
				t.Fatalf("seed backing: %v", err)
			}
			c := beads.NewCachingStoreForTest(backing, nil)
			if err := c.PrimeActive(); err != nil {
				t.Fatalf("PrimeActive: %v", err)
			}
			return c
		}
		oracle := build()
		cached := build()
		want, err := readyForControllerDemand(oracle)
		if err != nil {
			t.Fatalf("oracle readyForControllerDemand: %v", err)
		}
		got, err := newReadyDemandCache().controllerDemandReady(readyDemandCityStoreScope(), cached)
		if err != nil {
			t.Fatalf("cache controllerDemandReady: %v", err)
		}
		if a, b := readyIDs(want), readyIDs(got); len(a) != len(b) {
			t.Fatalf("controllerDemandReady returned %v, want %v", b, a)
		}
	})

	t.Run("explicit handles partial live merge", func(t *testing.T) {
		cachedRows := []beads.Bead{{ID: "bd-cached", Status: "open"}}
		liveRows := []beads.Bead{{ID: "bd-live", Status: "open"}}
		build := func() beads.Store {
			return controllerDemandHandlesStore{
				Store: beads.NewMemStore(),
				handles: beads.StoreHandles{
					Cached: &readyStaticStore{ready: cachedRows},
					Live:   &readyPartialLiveStore{rows: liveRows},
				},
			}
		}
		want, wantErr := readyForControllerDemandQuery(build(), beads.ReadyQuery{})
		got, gotErr := newReadyDemandCache().controllerDemandReady(readyDemandCityStoreScope(), build())
		if (wantErr == nil) != (gotErr == nil) || beads.IsPartialResult(wantErr) != beads.IsPartialResult(gotErr) {
			t.Fatalf("controllerDemandReady err = %v, want %v", gotErr, wantErr)
		}
		a, b := readyIDs(want), readyIDs(got)
		if len(a) != len(b) {
			t.Fatalf("controllerDemandReady merged rows = %v, want %v", b, a)
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("controllerDemandReady merged rows = %v, want %v", b, a)
			}
		}
	})
}

// TestCollectAssignedWorkBeadsCachedMatchesUncached proves that threading a
// shared cache through the assigned-work collection returns the same beads and
// readiness verdicts as the legacy per-assignee fan-out, while collapsing the
// N-assignee live reads to a single backing read.
func TestCollectAssignedWorkBeadsCachedMatchesUncached(t *testing.T) {
	seed := func(store *readyQueryRecordingStore) *sessionBeadSnapshot {
		var sessions []beads.Bead
		for _, name := range []string{"worker-a", "worker-b", "worker-c"} {
			s, err := store.Create(beads.Bead{
				Title:  name + " session",
				Type:   sessionBeadType,
				Status: "open",
				Metadata: map[string]string{
					"session_name": name,
					"template":     "worker",
					"state":        "asleep",
				},
			})
			if err != nil {
				t.Fatalf("create session %q: %v", name, err)
			}
			sessions = append(sessions, s)
			seedReadyWork(t, store, "ready for "+name, name)
		}
		return newSessionBeadSnapshot(sessions)
	}

	uncachedStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	uncachedSnap := seed(uncachedStore)
	wantBeads, _, _, wantReady, wantPartial := collectAssignedWorkBeadsWithStores(&config.City{}, uncachedStore, nil, nil, uncachedSnap)

	cachedStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	cachedSnap := seed(cachedStore)
	cache := newReadyDemandCache()
	gotBeads, _, _, gotReady, gotPartial := collectAssignedWorkBeadsWithStores(&config.City{}, cachedStore, nil, nil, cachedSnap, cache)

	if wantPartial != gotPartial {
		t.Fatalf("partial mismatch: uncached=%v cached=%v", wantPartial, gotPartial)
	}
	if a, b := readyIDs(wantBeads), readyIDs(gotBeads); len(a) != len(b) {
		t.Fatalf("assigned work mismatch: cached=%v uncached=%v", b, a)
	} else {
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("assigned work mismatch: cached=%v uncached=%v", b, a)
			}
		}
	}
	if len(wantReady) != len(gotReady) {
		t.Fatalf("readyAssigned mismatch: cached=%v uncached=%v", gotReady, wantReady)
	}
	for k := range wantReady {
		if !gotReady[k] {
			t.Fatalf("readyAssigned missing %+v in cached path: %v", k, gotReady)
		}
	}

	// Legacy path probes one live read per assignee; the cached path collapses
	// them to one backing read for the single store.
	uncachedReadyReads := len(uncachedStore.readyQueries)
	cachedReadyReads := len(cachedStore.readyQueries)
	if uncachedReadyReads < 3 {
		t.Fatalf("expected legacy per-assignee fan-out (>=3 reads), got %d", uncachedReadyReads)
	}
	if cachedReadyReads > 1 {
		t.Fatalf("cached assigned-work path issued %d backing Ready reads, want <= 1", cachedReadyReads)
	}
}

func BenchmarkReadyDemandCacheAssigneeFanout(b *testing.B) {
	store := &atomicReadyCountingStore{MemStore: beads.NewMemStore()}
	const assigneeCount = 32
	for i := 0; i < 1024; i++ {
		if _, err := store.Create(beads.Bead{
			Title:    fmt.Sprintf("work-%04d", i),
			Type:     "task",
			Assignee: fmt.Sprintf("worker-%02d", i%assigneeCount),
		}); err != nil {
			b.Fatal(err)
		}
	}
	queries := make([]beads.ReadyQuery, assigneeCount)
	for i := range queries {
		queries[i] = beads.ReadyQuery{Assignee: fmt.Sprintf("worker-%02d", i), Limit: 5}
	}

	b.Run("direct_per_assignee", func(b *testing.B) {
		start := store.calls.Load()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, query := range queries {
				if _, err := liveReadyForControllerDemandQuery(store, query); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(store.calls.Load()-start)/float64(b.N), "ready_reads/op")
	})
	b.Run("shared_snapshot", func(b *testing.B) {
		start := store.calls.Load()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cache := newReadyDemandCache()
			for _, query := range queries {
				if _, err := cache.liveReady(readyDemandCityStoreScope(), store, query); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(store.calls.Load()-start)/float64(b.N), "ready_reads/op")
	})
}
