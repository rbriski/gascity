package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// TestNonOptedCityGraphSeamByteIdentity is Invariant A end-to-end: a city with
// no .gc/graph scope behaves exactly as before P1.5 — the graph accessor returns
// the city store pointer, the scope probe and opener report absence, no journal
// files appear, and the by-id event router treats a gcg- id exactly as today
// (falls through to the all-stores scan).
func TestNonOptedCityGraphSeamByteIdentity(t *testing.T) {
	cityPath := t.TempDir()
	city := beads.NewMemStore()
	cs := &controllerState{
		cfg:           &config.City{},
		cityName:      "test-city",
		cityPath:      cityPath,
		cityBeadStore: city,
	}

	// Scope probe: absent.
	if cityHasGraphScope(cityPath) {
		t.Fatal("cityHasGraphScope() = true for a city with no .gc/graph scope")
	}

	// Opener: reports absence and opens nothing.
	result, present, err := openCityGraphJournalResultAt(cityPath)
	if err != nil {
		t.Fatalf("openCityGraphJournalResultAt() error = %v", err)
	}
	if present {
		t.Fatal("openCityGraphJournalResultAt() present = true for a non-opted city")
	}
	if result.Store != nil {
		t.Fatalf("openCityGraphJournalResultAt() store = %v, want nil", result.Store)
	}
	if _, statErr := os.Stat(filepath.Join(cityPath, ".gc", "graph")); !os.IsNotExist(statErr) {
		t.Fatalf(".gc/graph should not exist for a non-opted city, stat err = %v", statErr)
	}

	// Graph accessor: the exact city-store pointer, no router wrapper.
	if got := cs.GraphBeadStore().Store; !sameStorePtr(got, city) {
		t.Fatalf("GraphBeadStore().Store = %p, want CityBeadStore() %p", got, cs.CityBeadStore())
	}

	// Event router: a gcg- id is owned by nothing here (arm inert), so the caller
	// takes the all-stores fallback exactly as it did before P1.5.
	if store, known := cs.beadEventConfiguredStoreLocked("gcg-x123"); known || store != nil {
		t.Fatalf("beadEventConfiguredStoreLocked(gcg-…) = (%v, %v), want (nil, false) on a non-opted city", store, known)
	}
}

// TestCityGraphScopeTransientStatErrorNotMemoizedAsAbsence pins MEDIUM-2: a stat
// error that is NOT os.IsNotExist (here ENOTDIR — a file where a scope directory
// is expected) is unknowable, not authoritative absence. It must surface through
// the opener's error path and must NOT be memoized as a real miss, so a later
// probe (after the transient condition clears) can still open the store. A city
// that genuinely opted in must never be pinned to bare-legacy routing by a
// transient first probe.
func TestCityGraphScopeTransientStatErrorNotMemoizedAsAbsence(t *testing.T) {
	cityPath := t.TempDir()
	// Place a regular FILE where the scope's .beads directory is expected, so
	// stat of <scope>/.beads/config.yaml fails with ENOTDIR, not ENOENT.
	scopeBeads := filepath.Join(cityPath, ".gc", "graph", ".beads")
	if err := os.MkdirAll(filepath.Dir(scopeBeads), 0o755); err != nil {
		t.Fatalf("mkdir graph scope root: %v", err)
	}
	if err := os.WriteFile(scopeBeads, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	present, err := cityGraphScopePresence(cityPath)
	if err == nil {
		t.Fatal("cityGraphScopePresence returned nil error for a non-IsNotExist stat failure")
	}
	if os.IsNotExist(err) {
		t.Fatalf("cityGraphScopePresence surfaced an IsNotExist error, want a transient one: %v", err)
	}
	if present {
		t.Fatal("cityGraphScopePresence present = true on a transient stat error")
	}

	// The opener must surface the error (present=true tags "not real absence").
	if _, _, openErr := openCityGraphJournalResultAt(cityPath); openErr == nil {
		t.Fatal("openCityGraphJournalResultAt swallowed the transient stat error as absence")
	}

	// The cache must NOT memoize the transient miss.
	if got := cachedCityGraphJournal(cityPath); got != nil {
		t.Fatalf("cachedCityGraphJournal returned a store on a transient error: %v", got)
	}
	if _, ok := cityGraphJournalCache.Load(filepath.Clean(cityPath)); ok {
		t.Fatal("cachedCityGraphJournal memoized a transient stat error as absence — a later probe can never opt in")
	}
}

// TestGraphJournalCacheStaleNegativeTransientRevalidationSurfaces pins L3fix: a
// stale MEMOIZED negative that is re-validated with a transient stat error
// (EACCES/EMFILE — here ENOTDIR) must NOT be reported as authoritative "not
// opted" (nil, false, nil). It must surface as opted-unknown (nil, true, err),
// matching the fresh openCityGraphJournalResultAt path, so the caller retries
// instead of pinning bare-legacy routing for a city that may in fact be opted.
func TestGraphJournalCacheStaleNegativeTransientRevalidationSurfaces(t *testing.T) {
	cityPath := t.TempDir()
	key := filepath.Clean(cityPath)
	cityGraphJournalCache.Store(key, &graphJournalCacheEntry{store: nil})
	t.Cleanup(func() { cityGraphJournalCache.Delete(key) })

	// Make the scope re-stat fail transiently: a regular file where the scope's
	// .beads directory is expected, so the stat fails ENOTDIR (not ENOENT).
	scopeBeads := filepath.Join(graphScopeRoot(cityPath), ".beads")
	if err := os.MkdirAll(filepath.Dir(scopeBeads), 0o755); err != nil {
		t.Fatalf("mkdir graph scope root: %v", err)
	}
	if err := os.WriteFile(scopeBeads, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	store, opted, err := cachedCityGraphJournalResult(cityPath)
	if store != nil {
		t.Fatalf("store = %v, want nil on a transient revalidation error", store)
	}
	if err == nil {
		t.Fatal("cachedCityGraphJournalResult swallowed a transient revalidation stat error as authoritative absence")
	}
	if !opted {
		t.Fatal("opted = false on a transient revalidation error; want opted-unknown (true) so the caller retries, matching the fresh path")
	}
	// The transient miss must NOT overwrite/evict the memoized entry.
	if _, ok := cityGraphJournalCache.Load(key); !ok {
		t.Fatal("transient revalidation error evicted the memoized entry; a later probe must still re-validate")
	}
}

// TestGraphJournalCacheStaleNegativeConcurrentReopenDedups pins L2fix: after a
// city opts in mid-flight, concurrent callers that all observe a stale memoized
// negative must converge on exactly ONE opened store (LoadOrStore dedup, losers
// scheduled-closed) — never evict a freshly-stored positive. The re-validation
// now evicts ONLY the observed stale entry via CompareAndDelete, so a racing
// opener's live handle is never dropped from the map (which would leak it and
// force the next caller to open a second).
func TestGraphJournalCacheStaleNegativeConcurrentReopenDedups(t *testing.T) {
	// Close the losing handles synchronously so the test leaves nothing pending.
	prevDelay := controllerStateStoreCloseDelay
	controllerStateStoreCloseDelay = 0
	t.Cleanup(func() { controllerStateStoreCloseDelay = prevDelay })

	cityPath := t.TempDir()
	if err := migrateGraphJournalInit(cityPath); err != nil {
		t.Fatalf("migrate graph-journal init: %v", err)
	}
	key := filepath.Clean(cityPath)
	// Seed the exact race precondition: a stale negative from before the opt-in.
	cityGraphJournalCache.Store(key, &graphJournalCacheEntry{store: nil})
	t.Cleanup(func() {
		if v, ok := cityGraphJournalCache.Load(key); ok {
			if s := v.(*graphJournalCacheEntry).store; s != nil {
				_ = closeBeadStoreHandle(s)
			}
			cityGraphJournalCache.Delete(key)
		}
	})

	const goroutines = 32
	var wg sync.WaitGroup
	stores := make([]beads.Store, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			stores[idx], _, errs[idx] = cachedCityGraphJournalResult(cityPath)
		}(i)
	}
	close(start)
	wg.Wait()

	var winner beads.Store
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: cachedCityGraphJournalResult error = %v", i, errs[i])
		}
		if stores[i] == nil {
			t.Fatalf("goroutine %d: got a nil store for an opted city", i)
		}
		if winner == nil {
			winner = stores[i]
			continue
		}
		if !sameStorePtr(stores[i], winner) {
			t.Fatalf("concurrent callers got two different memoized stores (a live handle was evicted, not deduped): %p vs %p", stores[i], winner)
		}
	}
	// The cache must hold exactly that single positive.
	v, ok := cityGraphJournalCache.Load(key)
	if !ok {
		t.Fatal("cache holds no entry after the reopen storm")
	}
	if got := v.(*graphJournalCacheEntry).store; !sameStorePtr(got, winner) {
		t.Fatalf("memoized store %p != the store returned to callers %p", got, winner)
	}
}

// TestJournalStoreHandleActuallyCloses pins the LOW fix: closeBeadStoreHandle —
// the function scheduleCloseBeadStoreHandle uses to release the graph-journal
// handle that loses the LoadOrStore open race — must reach a real close on the
// policy-wrapped journal store, closing the underlying graphstore's SQLite
// pools. Without JournalStore.CloseStore the unwrap bottoms out on a store with
// no close method and silently leaks the sqlite handle.
func TestJournalStoreHandleActuallyCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "close-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	// A live handle answers Ping before close.
	if err := gs.DB().Ping(); err != nil {
		t.Fatalf("graphstore write pool not live before close: %v", err)
	}

	wrapped := wrapStoreWithBeadPolicies(beads.NewJournalStore(gs), &config.City{})
	if err := closeBeadStoreHandle(wrapped); err != nil {
		t.Fatalf("closeBeadStoreHandle(journal store): %v", err)
	}

	// After close the underlying SQLite handles are gone — a leaked handle would
	// still Ping successfully.
	if err := gs.DB().Ping(); err == nil {
		t.Fatal("graphstore write pool still live after close — handle leaked")
	}
	if err := gs.ReadDB().Ping(); err == nil {
		t.Fatal("graphstore read pool still live after close — handle leaked")
	}
}
