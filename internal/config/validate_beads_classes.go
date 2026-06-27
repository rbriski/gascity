package config

import (
	"fmt"
	"strings"
)

// ValidateBeadsClasses rejects an unrecognized [beads.classes.<class>] backend so
// a typo fails fast at load instead of silently normalizing to "bd" (the work
// store) and diverting infra beads to the wrong place. Every relocatable class
// (graph, sessions, messaging, orders, nudges) honors bd | sqlite | postgres:
// graph routes through resolveGraphStore (the dedicated SQLite/Postgres graph
// store at the legacy .gc/beads.sqlite or the gcg schema), the rest route through
// resolveClassStore. An empty/unset backend means "stay on the work store" and is
// allowed.
//
// NOTE: a postgres backend requires its per-class schema to be provisioned
// (`gc beads postgres init`) BEFORE the city loads; an unprovisioned backend logs
// loudly and falls back to the work store, so provision first.
func ValidateBeadsClasses(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	for class, c := range cfg.Beads.Classes {
		if strings.TrimSpace(c.Backend) == "" {
			continue // unset → bd default
		}
		if normalizeBackend(c.Backend) == "" {
			return fmt.Errorf(
				"%s: [beads.classes.%s] backend=%q is not a known beads backend (use %q, %q, or %q)",
				source, class, c.Backend, BeadsBackendBD, BeadsBackendSQLite, BeadsBackendPostgres)
		}
	}
	return nil
}
