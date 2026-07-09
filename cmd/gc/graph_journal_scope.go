package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// graphScopeRoot returns the on-disk scope of the city's journal graph store:
// a single journal.db plus its provider marker under <city>/.gc/graph.
func graphScopeRoot(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "graph")
}

// cityGraphScopePresence is the activation probe: it reports whether the graph
// scope's provider marker is present. Only os.IsNotExist is authoritative "not
// opted" (false, nil). A genuine absence takes exactly one os.Stat and is
// byte-identical to today. Any OTHER stat error (EACCES, EMFILE, ENOTDIR, …) is
// unknowable, not absence: it returns (false, err) so callers surface and retry
// it rather than seeding a permanent bare-legacy routing for a city that is in
// fact opted (MEDIUM-2 — a transient error must never be cached as absence).
func cityGraphScopePresence(cityPath string) (bool, error) {
	if strings.TrimSpace(cityPath) == "" {
		return false, nil
	}
	_, err := os.Stat(filepath.Join(graphScopeRoot(cityPath), ".beads", "config.yaml"))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// cityHasGraphScope is the boolean activation boundary used on read/event hot
// paths where there is no error channel to surface a transient probe failure.
// A non-IsNotExist stat error yields false for this call only (the graph arm
// goes inert and the event falls through to the legacy all-stores scan, which
// is the safe pre-P1.5 behavior); it is never memoized, so the next call
// retries. The authoritative opener path uses cityGraphScopePresence so a
// transient error is surfaced rather than cached (MEDIUM-2).
func cityHasGraphScope(cityPath string) bool {
	present, _ := cityGraphScopePresence(cityPath)
	return present
}

// graphScopeCityID derives the chain-genesis city id for the journal graph
// store from the city's canonical project identity when present, else "" (the
// store then adopts whatever city_id it already holds, or genesis-derives from
// its stream id). Best-effort: any read error yields "".
func graphScopeCityID(cityPath string) string {
	if projectID, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, cityPath); err == nil && ok {
		return strings.TrimSpace(projectID)
	}
	return ""
}

// openCityGraphJournalResultAt opens the journal graph store for a city that
// has opted in. It returns (zero, false, nil) when the city has no graph scope
// — callers treat absence as "route legacy" — and (result, true, nil) when the
// store opened. The store is policy-wrapped to match the city-store open path;
// it is deliberately not cache-wrapped at P1.5 (the graph class is event-silent
// and the adapter is already an in-process SQLite read).
func openCityGraphJournalResultAt(cityPath string) (beads.StoreOpenResult, bool, error) {
	present, err := cityGraphScopePresence(cityPath)
	if err != nil {
		// Unknowable scope (transient stat error): surface it so the caller warns
		// and retries later. present=true tags this as "not authoritative absence"
		// so cachedCityGraphJournal declines to memoize it as a real miss (MEDIUM-2).
		return beads.StoreOpenResult{}, true, fmt.Errorf("probing city graph scope %q: %w", cityPath, err)
	}
	if !present {
		return beads.StoreOpenResult{}, false, nil
	}
	// Resolve the OPTIONAL backend selector from the scope marker. Absent/sqlite
	// keeps the byte-identical embedded-SQLite path; backend=postgres opens the
	// hosted Postgres engine. A malformed marker is opted-but-unopenable (present
	// stays true so the caller surfaces and retries rather than memoizing a miss).
	// The error never carries the DSN — the marker holds only the env-var name or
	// the credential command, never the credential itself.
	backend, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		return beads.StoreOpenResult{}, true, fmt.Errorf("resolving city graph backend %q: %w", cityPath, err)
	}
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot: graphScopeRoot(cityPath),
		CityPath:  cityPath,
		Provider:  "journal",
		Logger:    slog.Default(),
		OpenJournalStore: func() (beads.Store, error) {
			gs, err := backend.openGraphStore(context.Background(), cityPath)
			if err != nil {
				return nil, err
			}
			return beads.NewJournalStore(gs), nil
		},
	})
	if err != nil {
		return beads.StoreOpenResult{}, true, err
	}
	result.Store = wrapStoreWithBeadPolicies(result.Store, cfg)
	return result, true, nil
}

// graphJournalCacheEntry memoizes an authoritative graph-journal lookup: a
// successfully opened store, or a real absence (nil store). Transient open
// errors are never cached.
type graphJournalCacheEntry struct {
	store beads.Store
}

// cityGraphJournalCache is the one-shot memo keyed by clean city path, mirroring
// the city-store open memo: authoritative results only, LoadOrStore on the
// concurrent-first-open race.
var cityGraphJournalCache sync.Map // string(clean cityPath) -> *graphJournalCacheEntry

// cachedCityGraphJournalResult returns the city's journal graph store together
// with whether the city is OPTED into a graph scope and any open error,
// memoizing only authoritative results. The three outcomes are distinct — a
// caller that must not silently strand journal-resident work (the serve-mode
// control frontier, MEDIUM-1) depends on telling them apart:
//
//   - (nil, false, nil): genuinely not opted (no .gc/graph scope). Byte-identical
//     legacy routing.
//   - (store, true, nil): opted and opened.
//   - (nil, true, err):   opted but the leg could not be opened/probed (a
//     transient stat/open failure). NEVER memoized, so a later call retries.
//
// A memoized entry is always authoritative (a real open or a real absence, never
// a transient error), so "opted" is recovered exactly as store != nil.
func cachedCityGraphJournalResult(cityPath string) (beads.Store, bool, error) {
	key := filepath.Clean(cityPath)
	if v, ok := cityGraphJournalCache.Load(key); ok {
		store := v.(*graphJournalCacheEntry).store
		if store != nil {
			return store, true, nil
		}
		// A memoized ABSENCE is not permanent: `gc migrate graph-journal init`
		// opts a running city in mid-flight, and post-start opt-in must be safe
		// for every consumer, not just the tick (which already re-stats the scope
		// each pass, cityHasGraphScope). Re-stat the scope with the same cheap
		// probe: a still-absent scope stays the cached negative, while a now-present
		// scope invalidates the stale entry and falls through to a fresh open. Only
		// the nil-store negative is re-validated; a real opened handle stays memoized,
		// so the opted-city hot path is unchanged.
		present, statErr := cityGraphScopePresence(cityPath)
		switch {
		case statErr != nil:
			// A transient re-validation stat error (EACCES/EMFILE/…) is unknowable,
			// not authoritative absence. Surface it as opted-unknown (present=true
			// tags "not real absence"), matching the fresh openCityGraphJournalResultAt
			// path, so the caller retries rather than pinning bare-legacy routing. The
			// memoized negative is left intact for the next pass (L3fix).
			return nil, true, fmt.Errorf("re-validating city graph scope %q: %w", cityPath, statErr)
		case !present:
			// Still absent: the cached negative stands (byte-identical legacy routing).
			return nil, false, nil
		}
		// Now present. Evict ONLY the observed stale negative — never a positive a
		// racing opener stored in the gap. An unconditional Delete could drop a
		// freshly-memoized live handle out of the map (leaking it; callers hold it
		// but nothing closes it, and the next caller opens a second). CompareAndDelete
		// spares any value other than the stale v we loaded (L2fix).
		cityGraphJournalCache.CompareAndDelete(key, v)
	}
	opened, present, err := openCityGraphJournalResultAt(cityPath)
	if err != nil {
		// Opted-but-unopenable (present=true tags "not authoritative absence"):
		// surface it and do not memoize, so a caller with a hard-fail discipline
		// can react and a later call retries once the transient condition clears.
		return nil, present, err
	}
	var store beads.Store
	if present {
		store = opened.Store
	}
	entry := &graphJournalCacheEntry{store: store}
	if actual, loaded := cityGraphJournalCache.LoadOrStore(key, entry); loaded {
		// Lost the concurrent-first-open race: close our just-opened handle and
		// use the winner's memoized store.
		if store != nil {
			scheduleCloseBeadStoreHandle("city graph journal store", store)
		}
		winner := actual.(*graphJournalCacheEntry).store
		return winner, winner != nil, nil
	}
	return store, present, nil
}

// cachedCityGraphJournal returns the city's journal graph store (nil when the
// city has no graph scope OR a transient open error routed it to legacy),
// memoizing the authoritative result. This is the error-channel-free accessor for
// read/event hot paths: a transient open error logs and degrades to the legacy
// store rather than surfacing. Callers that must hard-fail on an opted-but-
// unopenable leg (the serve-mode control frontier) use
// cachedCityGraphJournalResult instead.
func cachedCityGraphJournal(cityPath string) beads.Store {
	store, _, err := cachedCityGraphJournalResult(cityPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc: city graph journal store: %v (graph class routes to the work store)\n", err)
		return nil
	}
	return store
}

// newControllerStateOpenCityGraphJournal opens the city-level journal graph
// store for newControllerState. Tests swap this seam to inject an in-memory
// journal leg (or to assert it is never called on a non-opted city).
var newControllerStateOpenCityGraphJournal = openCityGraphJournalResultAt

// CityGraphJournalStore returns the controller's journal graph store, or nil
// when the city has no .gc/graph scope. Mirrors CityBeadStore()'s locking.
func (cs *controllerState) CityGraphJournalStore() beads.Store {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cityGraphJournal
}

// cityGraphJournalStore returns the runtime's journal graph store: the
// controller's handle when controller-managed, else the one-shot cache. Nil
// unless the city has a .gc/graph scope, in which case resolveGraphStore returns
// the legacy store unchanged.
func (cr *CityRuntime) cityGraphJournalStore() beads.Store {
	if cr.cs != nil {
		return cr.cs.CityGraphJournalStore()
	}
	return cachedCityGraphJournal(cr.cityPath)
}
