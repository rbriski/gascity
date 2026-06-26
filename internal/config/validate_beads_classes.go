package config

import "fmt"

// ValidateBeadsClasses rejects per-class beads backends the runtime cannot honor
// yet, so a config that would be silently mis-routed fails fast at load instead.
//
// The graph class is the only one not wired through resolveClassStore: it routes
// through coordrouter.Router, which is gated on the legacy graph_store knob and
// registers only a SQLite backend. So [beads.classes.graph] backend="postgres"
// would make NormalizedClassBackend(graph) report "postgres" (and gc beads
// postgres init graph would provision a PG schema) while the Router still serves
// SQLite/Dolt — a silent wrong-store with no error and no log. Reject it until
// graph routes through resolveClassStore (P6 Router retirement), at which point
// this guard relaxes to allow postgres.
//
// graph ∈ {"", "bd", "sqlite"} is allowed: "sqlite" is the future P6 spelling and
// is a harmless no-op divert today (the Router still keys off graph_store), and an
// unknown value normalizes to "" → NormalizedClassBackend(graph)="bd", so it never
// diverts. Only an explicit "postgres" is dangerous.
func ValidateBeadsClasses(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	c, ok := cfg.Beads.Classes[BeadClassGraph]
	if !ok {
		return nil
	}
	switch normalizeBackend(c.Backend) {
	case BeadsBackendBD, BeadsBackendSQLite, "":
		return nil
	default:
		return fmt.Errorf(
			"%s: [beads.classes.%s] backend=%q is not honored by the graph router "+
				"(graph supports only %q or %q today); remove it or use graph_store=%q",
			source, BeadClassGraph, c.Backend, BeadsBackendBD, BeadsBackendSQLite, BeadsBackendSQLite)
	}
}
