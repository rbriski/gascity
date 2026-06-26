package config

import "fmt"

// ValidateBeadsClasses rejects per-class beads backends the runtime cannot honor,
// so a config that would be silently mis-routed fails fast at load instead.
//
// The graph class routes through coordrouter.Router (gated on graphRelocated),
// whose registerGraphStoreBackend now opens a SQLite OR a Postgres graph backend.
// So [beads.classes.graph] ∈ {"", "bd", "sqlite", "postgres"} are all honored:
// NormalizedClassBackend(graph) selects the backend and the Router registers it.
// An unknown value normalizes to "" → NormalizedClassBackend(graph)="bd", so it
// stays on the work store rather than silently diverting.
//
// NOTE: graph=postgres requires the gcg schema to be provisioned (`gc beads
// postgres init`) BEFORE the city loads; an unprovisioned backend logs loudly and
// falls back to the work store (openClassPostgresStore), so provision first.
func ValidateBeadsClasses(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	c, ok := cfg.Beads.Classes[BeadClassGraph]
	if !ok {
		return nil
	}
	switch normalizeBackend(c.Backend) {
	case BeadsBackendBD, BeadsBackendSQLite, BeadsBackendPostgres, "":
		return nil
	default:
		return fmt.Errorf(
			"%s: [beads.classes.%s] backend=%q is not a known beads backend "+
				"(graph supports %q, %q, or %q)",
			source, BeadClassGraph, c.Backend, BeadsBackendBD, BeadsBackendSQLite, BeadsBackendPostgres)
	}
}
