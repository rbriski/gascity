package main

import (
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// classStoreHandleCache shares one SQLite handle per class store directory across
// all in-process consumers (mirroring graphStoreHandleCache), so one consumer's
// close cannot pull the handle out from under the others. Opened lazily by
// openClassSQLiteStore.
var classStoreHandleCache sync.Map // dir string -> beads.Store (noClose-wrapped)

// beadEventRowRecorder translates a store-edge RowChange into the bead.* event the
// controller already publishes for bd-backed stores, so a relocated SQLite class
// keeps feeding the event bus (order triggers, the dashboard bead feed, cache
// observers) exactly as before. getBead reads the post-commit bead for the
// payload (created/updated/closed); a delete carries the type captured
// pre-removal. The store distinguishes RowClosed (a true open->closed
// transition) from RowUpdated, so this maps op-for-op to CachingStore's events.
func beadEventRowRecorder(getBead func(id string) (beads.Bead, error), rec events.Recorder) beads.RowChangeEmitter {
	if rec == nil {
		return nil
	}
	return func(rc beads.RowChange) {
		var (
			eventType string
			bead      beads.Bead
		)
		switch rc.Op {
		case beads.RowCreated:
			b, err := getBead(rc.ID)
			if err != nil {
				return
			}
			bead, eventType = b, events.BeadCreated
		case beads.RowUpdated:
			b, err := getBead(rc.ID)
			if err != nil {
				return
			}
			// RowUpdated -> bead.updated unconditionally: the store emits RowClosed
			// only on a true open->closed transition, matching CachingStore (which
			// emits bead.closed only on a transition, bead.updated for an update to
			// an already-closed bead). Inferring closed from status here would
			// diverge for orders/sessions that update already-closed beads.
			bead, eventType = b, events.BeadUpdated
		case beads.RowClosed:
			b, err := getBead(rc.ID)
			if err != nil {
				return
			}
			bead, eventType = b, events.BeadClosed
		case beads.RowDeleted:
			bead, eventType = beads.Bead{ID: rc.ID, Type: rc.Type}, events.BeadDeleted
		default:
			return
		}
		payload, err := json.Marshal(api.BeadEventPayload{Bead: bead})
		if err != nil {
			return
		}
		// Actor "cache-reconcile" matches the default mail path (api_state.go's
		// CachingStore onChange) so applyBeadEventToStores does not Poke the work
		// reconciler on every relocated-class write — the controller can no longer
		// observe these writes via its own reconcile, so a wake would be wasted.
		rec.Record(events.Event{Type: eventType, Actor: "cache-reconcile", Subject: rc.ID, Payload: payload})
	}
}

// openClassSQLiteStore opens (or returns the cached) embedded SQLite store for a
// coordination class at <cityPath>/.gc/<class>/, with the class's id prefix and
// per-process retention disabled. Controller-owned retention GC is a deferred
// follow-up (no such sweep exists yet, same as the graph store); messaging needs
// none because it self-GCs via Archive->Delete. Retention MUST stay disabled
// while a recorder is attached: purgeTerminal would Delete rows and emit a
// bead.deleted storm with no bd-path equivalent.
//
// When rec is non-nil the store emits bead.* events on every committed mutation
// (the store-edge replacement for bd hooks). INVARIANT: this opener is the
// controller-only path and is always called WITH the controller's recorder; the
// migration command opens its dest directly (no recorder, no cache). On a cache
// hit the already-open handle (and its recorder, if any) is returned as-is, so a
// recorder-less caller must never share a dir with a recorder-wanting one.
// Returns (nil,false) on failure so the caller falls back to the work backend.
func openClassSQLiteStore(cityPath, class string, rec events.Recorder) (beads.Store, bool) {
	dir := classSQLiteDir(cityPath, class)
	if cached, ok := classStoreHandleCache.Load(dir); ok {
		return cached.(beads.Store), true
	}
	var opened beads.Store // late-bound so the recorder can read post-commit beads
	opts := []beads.SQLiteStoreOption{
		beads.WithSQLiteStoreIDPrefix(classSQLitePrefix[class]),
		beads.WithSQLiteStoreRetention(0, 0),
	}
	if rec != nil {
		opts = append(opts, beads.WithSQLiteStoreRecorder(
			beadEventRowRecorder(func(id string) (beads.Bead, error) { return opened.Get(id) }, rec),
		))
	}
	store, err := beads.OpenSQLiteStore(dir, opts...)
	if err != nil {
		log.Printf("beads: class %q backend=sqlite requested but opening %s failed: %v; class stays on the work backend", class, dir, err)
		return nil, false
	}
	opened = store
	// Cache a never-closed wrapper so a consumer's close cannot close the shared
	// handle out from under the others (same discipline as the graph store).
	shared := store
	if sq, ok := store.(*beads.SQLiteStore); ok {
		shared = noCloseSQLiteStore{sq}
	}
	if actual, loaded := classStoreHandleCache.LoadOrStore(dir, shared); loaded {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore() //nolint:errcheck // best-effort close of the losing duplicate
		}
		shared = actual.(beads.Store)
	}
	return shared, true
}

// classBackendOpener opens the per-class internal-DB store for one configured
// backend, returning (store, true) on success or (nil, false) to fall back to the
// work store (logging its own diagnostic). It is the generic seam each in-process
// internal-DB backend registers into, so resolveClassStore dispatches by backend
// NAME instead of a hardcoded switch: a new in-process backend registers an opener
// here and ships its provisioning/ops + config as a downloadable pack.
//
// The hot read/write path stays in-process Go by design — the infra/beads split
// exists to escape per-op subprocess latency — so the pluggable surface is backend
// SELECTION + ops, not the store implementation (which is compiled in).
type classBackendOpener func(cfg *config.City, cityPath, class string, rec events.Recorder) (beads.Store, bool)

// classBackendOpeners maps a [beads.classes.<class>].backend value to its opener.
// "bd" is intentionally absent: it means "stay on the work store" and is handled
// before lookup. Backends register here (postgres is added with its opener).
var classBackendOpeners = map[string]classBackendOpener{
	config.BeadsBackendSQLite: func(_ *config.City, cityPath, class string, rec events.Recorder) (beads.Store, bool) {
		return openClassSQLiteStore(cityPath, class, rec)
	},
}

// resolveClassStore returns the beads.Store backing a coordination class, honoring
// [beads.classes.<class>].backend. It is the single dispatch point for per-class
// backend selection: "bd" (default) stays on the Provider/Dolt work store; any
// other backend is opened by its registered classBackendOpener, falling back to the
// work store — never a silent divert — when the backend has no registered opener or
// fails to open.
func resolveClassStore(workStore beads.Store, cfg *config.City, cityPath, class string, rec events.Recorder) beads.Store {
	if cfg == nil {
		return workStore
	}
	backend := cfg.Beads.NormalizedClassBackend(class)
	if backend == config.BeadsBackendBD {
		return workStore
	}
	opener, ok := classBackendOpeners[backend]
	if !ok {
		log.Printf("beads: class %q backend=%q has no registered opener; the class stays on the work store", class, backend)
		return workStore
	}
	if store, ok := opener(cfg, cityPath, class, rec); ok {
		return store
	}
	return workStore
}

// resolveMailMessagesStore returns beadmail's message-persistence seam: the
// configured class store (emitting bead.* events via rec) when messaging is
// relocated, otherwise the work store. Session reads always stay on the work store
// until sessions relocate, so the two seams diverge exactly here.
func resolveMailMessagesStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beadmail.MailStore {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassMessaging, rec)
}

// resolveOrderStore returns the order-tracking store: the embedded SQLite order
// store (emitting bead.* events via rec) when [beads.classes.orders].backend="sqlite"
// and it opens, otherwise the work store. Returned as a beads.Store so the dispatch
// path uses it both as the orders.OrderStore tracking seam and, when distinct from
// the work store, as an extra gate-read store (so the single-flight gate finds the
// relocated tracking bead). Byte-identical to the work store at the default backend.
func resolveOrderStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassOrders, rec)
}

// resolveNudgesStore returns the nudge-shadow store: the configured class store
// (emitting bead.* events via rec) when [beads.classes.nudges].backend relocates
// nudges, otherwise the work store. Returned as a beads.Store, which satisfies
// nudgequeue.NudgeStore for free, so only the LEAF nudge-bead operations route
// here; the session/wait/mail ops that share the nudge subsystem stay on the work
// store. Byte-identical to the work store at the default backend.
func resolveNudgesStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassNudges, rec)
}

// resolveSessionStore returns the session-lifecycle store: the configured class
// store (emitting bead.* events via rec) when [beads.classes.sessions].backend
// relocates sessions, otherwise the work store. Session-class beads are session
// lifecycle beads (type=session/gc:session) AND durable session waits
// (type=gate/gc:wait); both classify to ClassSessions. Only the SESSION/WAIT bead
// ops route here — the controller's cross-class WORK-bead assignment reads stay on
// the work store (see the two-store split in cmd/gc/session_beads.go). Byte-
// identical to the work store at the default backend.
func resolveSessionStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(workStore, cfg, cityPath, config.BeadClassSessions, rec)
}

// openGraphSQLiteStore opens (or returns the cached) embedded SQLite graph store at
// the LEGACY <cityPath>/.gc/beads.sqlite location — filepath.Join(cityPath,
// citylayout.RuntimeRoot), distinct from the .gc/<class>/ class-store convention
// (openClassSQLiteStore / classSQLiteDir) — so it stays byte-identical for cities
// already on graph_store="sqlite". It is a Router-free cut of registerGraphStoreSQLite's
// body MINUS the r.Register call, reusing graphStoreHandleCache / noCloseSQLiteStore /
// graphStoreIDPrefix verbatim. Returns (nil,false) on failure so resolveGraphStore
// falls back to the work backend (never a silent divert to the wrong location).
//
// INVARIANT (the data-orphan landmine): this opener MUST use citylayout.RuntimeRoot,
// NOT classSQLiteDir(cityPath, "graph"). Routing graph through .gc/graph/ would point
// a live graph_store="sqlite" city at an empty store and orphan its graph data.
func openGraphSQLiteStore(cityPath string) (beads.Store, bool) {
	dir := filepath.Join(cityPath, citylayout.RuntimeRoot)
	if cached, ok := graphStoreHandleCache.Load(dir); ok {
		return cached.(beads.Store), true
	}
	store, err := beads.OpenSQLiteStore(dir,
		beads.WithSQLiteStoreRetention(0, 0),
		beads.WithSQLiteStoreIDPrefix(graphStoreIDPrefix))
	if err != nil {
		log.Printf("beads: graph_store=sqlite requested but opening the SQLite graph store at %s failed: %v; graph beads stay on the work backend", dir, err)
		return nil, false
	}
	// Cache a never-closed wrapper so a consumer's closeBeadStoreHandle cannot close
	// the handle out from under the other consumers of the cached store.
	shared := store
	if sq, ok := store.(*beads.SQLiteStore); ok {
		shared = noCloseSQLiteStore{sq}
	}
	if actual, loaded := graphStoreHandleCache.LoadOrStore(dir, shared); loaded {
		// Lost the open race: close OUR real handle, use the cached shared one.
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore() //nolint:errcheck // best-effort close of the losing duplicate
		}
		shared = actual.(beads.Store)
	}
	return shared, true
}

// resolveGraphStore returns the beads.Store backing the GRAPH coordination class.
// It is the dedicated, class-aware successor to registerGraphStoreBackend +
// coordrouter.Router's ClassGraph leg, mirroring resolveSessionStore's shape while
// dispatching like registerGraphStoreBackend.
//
// It MUST NOT route through resolveClassStore / openClassSQLiteStore: the SQLite
// graph store lives at the LEGACY <cityPath>/.gc/ (citylayout.RuntimeRoot, file
// beads.sqlite), NOT the .gc/<class>/ class-store convention, so a live
// graph_store="sqlite" city is never pointed at an empty .gc/graph/ and its graph
// data is never orphaned. Postgres uses the canonical class path (gcg schema), which
// is safe to share with openClassSQLiteStore's Postgres opener.
//
//	graph=bd       -> the work store (graphRelocated false; byte-identical default)
//	graph=sqlite   -> the cached embedded SQLite store at .gc/beads.sqlite (gcg, retention 0,0)
//	graph=postgres -> openClassPostgresStore(cfg, cityPath, BeadClassGraph, nil) (gcg schema)
//
// On open-failure it falls back to the work store rather than a silent wrong store.
//
// rec is accepted for signature parity with the other resolve*Store helpers but is
// intentionally IGNORED: the graph store stays event-silent (matching the prior
// registerGraphStoreSQLite / registerGraphStoreBackend opens, which passed no
// recorder) because the formula-v2 topology is high-churn and is not mirrored to the
// bus.
func resolveGraphStore(workStore beads.Store, cfg *config.City, cityPath string, _ events.Recorder) beads.Store {
	if !graphRelocated(cfg) {
		return workStore
	}
	switch cfg.Beads.NormalizedClassBackend(config.BeadClassGraph) {
	case config.BeadsBackendSQLite:
		if s, ok := openGraphSQLiteStore(cityPath); ok {
			return s
		}
		return workStore
	case config.BeadsBackendPostgres:
		if s, ok := openClassPostgresStore(cfg, cityPath, config.BeadClassGraph, nil); ok {
			return s
		}
		return workStore
	default:
		return workStore
	}
}

// newCityMailProvider builds the controller's mail provider. Message persistence
// routes to SQLite when configured (controller-mediated: the long-lived
// controller owns the single writer) while session reads stay on the work store.
// At the default backend it is byte-identical to the previous newMailProvider
// (both seams are the work store, no SQLite store, no recorder).
func newCityMailProvider(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) mail.Provider {
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		return newMailProviderNamed(v, workStore, true)
	}
	return beadmail.NewCachedWithStores(resolveMailMessagesStore(workStore, cfg, cityPath, rec), workStore)
}
