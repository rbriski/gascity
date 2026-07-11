package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// This file is the CLI one-shot seam for the graph, orders, and nudges
// coordination classes, mirroring cliSessionStore (cli_session_store.go) for
// the session class. Each helper routes a generic CLI work store to its
// coordination-class store so a [beads.classes.<class>] relocation reaches
// one-shot commands (gc sling, gc order run, ...) the same way it reaches the
// running controller (which routes through the CityRuntime.*BeadStore
// accessors). The infra store is sourced lazily from cachedCityInfraStore, so a
// split city's graph/order/nudge writes reach the infra store from a one-shot
// command; it is nil (⇒ identity to the input store) on every single-store
// city, so wrapping stays byte-identical until the split activates.
//
// The recorder is nil: a one-shot CLI command has no live event bus, matching
// today's behavior where these paths emit no bead events. Threading a recorder
// so relocated CLI writes emit bead.* is a separate follow-up (see the
// cli_session_store.go comment).

// cliGraphStore routes a generic CLI one-shot work store to the graph
// (workflow/v2) coordination-class store: the infra store on a split city, else
// the input store verbatim (identity). This is the CLI analog of
// CityRuntime.graphBeadStore. It returns the SAME wrapped instance the input
// store or the infra store already is, never a re-wrap, so the graph-create
// optional-capability assertions (GraphApplyFor / HandlesFor /
// StorageCreateStore) that molecule.Instantiate relies on stay intact.
func cliGraphStore(store beads.Store, cfg *config.City, cityPath string) beads.Store {
	return resolveGraphStore(store, cachedCityInfraStore(cityPath, cfg), cfg, cityPath, nil)
}

// cliOrderStore routes a generic CLI one-shot work store to the order-tracking
// coordination-class store: the infra store on a split city, else the input
// store verbatim (identity). It is the CLI analog of
// CityRuntime.ordersBeadStore, returning the same wrapped instance so the
// order-tracking bead operations keep their store capabilities.
func cliOrderStore(store beads.Store, cfg *config.City, cityPath string) beads.Store {
	return resolveOrderStore(store, cachedCityInfraStore(cityPath, cfg), cfg, cityPath, nil)
}

// cliNudgesStore routes a generic CLI one-shot work store to the nudge
// coordination-class store: the infra store on a split city, else the input
// store verbatim (identity). It is the CLI analog of
// CityRuntime.nudgesBeadStore, returning the same wrapped instance.
func cliNudgesStore(store beads.Store, cfg *config.City, cityPath string) beads.Store {
	return resolveNudgesStore(store, cachedCityInfraStore(cityPath, cfg), cfg, cityPath, nil)
}
