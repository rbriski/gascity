package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
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

// resolveMailMessagesStore returns beadmail's message-persistence seam: the
// embedded SQLite messaging store (emitting bead.* events via rec) when
// [beads.classes.messaging].backend="sqlite" and it opens, otherwise the work
// store. Session reads always stay on the work store until sessions relocate, so
// the two seams diverge exactly here.
func resolveMailMessagesStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beadmail.MailStore {
	if cfg != nil && cfg.Beads.ClassUsesSQLite(config.BeadClassMessaging) {
		if sqliteStore, ok := openClassSQLiteStore(cityPath, config.BeadClassMessaging, rec); ok {
			return sqliteStore
		}
	}
	return workStore
}

// resolveOrderStore returns the order-tracking store: the embedded SQLite order
// store (emitting bead.* events via rec) when [beads.classes.orders].backend="sqlite"
// and it opens, otherwise the work store. Returned as a beads.Store so the dispatch
// path uses it both as the orders.OrderStore tracking seam and, when distinct from
// the work store, as an extra gate-read store (so the single-flight gate finds the
// relocated tracking bead). Byte-identical to the work store at the default backend.
func resolveOrderStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	if cfg != nil && cfg.Beads.ClassUsesSQLite(config.BeadClassOrders) {
		if sqliteStore, ok := openClassSQLiteStore(cityPath, config.BeadClassOrders, rec); ok {
			return sqliteStore
		}
	}
	return workStore
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
