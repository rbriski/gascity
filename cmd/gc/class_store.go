package main

import (
	"log"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// classStoreHandleCache shares one SQLite handle per class store directory across
// all in-process consumers (mirroring graphStoreHandleCache), so one consumer's
// close cannot pull the handle out from under the others. Opened lazily by
// openClassSQLiteStore.
var classStoreHandleCache sync.Map // dir string -> beads.Store (noClose-wrapped)

// openClassSQLiteStore opens (or returns the cached) embedded SQLite store for a
// coordination class at <cityPath>/.gc/<class>/, with the class's id prefix and
// per-process retention disabled (a single controller-owned sweep handles GC, as
// for the graph store). Returns (nil,false) on failure so the caller falls back
// to the work backend.
func openClassSQLiteStore(cityPath, class string) (beads.Store, bool) {
	dir := classSQLiteDir(cityPath, class)
	if cached, ok := classStoreHandleCache.Load(dir); ok {
		return cached.(beads.Store), true
	}
	store, err := beads.OpenSQLiteStore(
		dir,
		beads.WithSQLiteStoreIDPrefix(classSQLitePrefix[class]),
		beads.WithSQLiteStoreRetention(0, 0),
	)
	if err != nil {
		log.Printf("beads: class %q backend=sqlite requested but opening %s failed: %v; class stays on the work backend", class, dir, err)
		return nil, false
	}
	// Cache a never-closed wrapper so a consumer's close cannot close the shared
	// handle out from under the others (same discipline as the graph store).
	shared := store
	if sq, ok := store.(*beads.SQLiteStore); ok {
		shared = noCloseGraphStore{sq}
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
// embedded SQLite messaging store when [beads.classes.messaging].backend="sqlite"
// (and it opens), otherwise the work store. Session reads always stay on the work
// store until sessions relocate, so the two seams diverge exactly here.
func resolveMailMessagesStore(workStore beads.Store, cfg *config.City, cityPath string) beadmail.MailStore {
	if cfg != nil && cfg.Beads.ClassUsesSQLite(config.BeadClassMessaging) {
		if sqliteStore, ok := openClassSQLiteStore(cityPath, config.BeadClassMessaging); ok {
			return sqliteStore
		}
	}
	return workStore
}

// newCityMailProvider builds the controller's mail provider. Message persistence
// routes to SQLite when configured (controller-mediated: the long-lived
// controller owns the single writer) while session reads stay on the work store.
// At the default backend it is byte-identical to the previous newMailProvider
// (both seams are the work store).
func newCityMailProvider(workStore beads.Store, cfg *config.City, cityPath string) mail.Provider {
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		return newMailProviderNamed(v, workStore, true)
	}
	return beadmail.NewCachedWithStores(resolveMailMessagesStore(workStore, cfg, cityPath), workStore)
}
