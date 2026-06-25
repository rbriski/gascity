package config

import "testing"

func TestNormalizedClassBackend(t *testing.T) {
	cases := []struct {
		name  string
		cfg   BeadsConfig
		class string
		want  string
	}{
		{"default empty config -> bd", BeadsConfig{}, BeadClassMessaging, BeadsBackendBD},
		{"explicit bd -> bd", BeadsConfig{Classes: map[string]BeadClassConfig{"messaging": {Backend: "bd"}}}, BeadClassMessaging, BeadsBackendBD},
		{"explicit sqlite -> sqlite", BeadsConfig{Classes: map[string]BeadClassConfig{"messaging": {Backend: "sqlite"}}}, BeadClassMessaging, BeadsBackendSQLite},
		{"case-insensitive + whitespace", BeadsConfig{Classes: map[string]BeadClassConfig{"orders": {Backend: "  SQLite "}}}, BeadClassOrders, BeadsBackendSQLite},
		{"explicit postgres -> postgres", BeadsConfig{Classes: map[string]BeadClassConfig{"nudges": {Backend: "postgres"}}}, BeadClassNudges, BeadsBackendPostgres},
		{"case-insensitive postgres", BeadsConfig{Classes: map[string]BeadClassConfig{"sessions": {Backend: " Postgres "}}}, BeadClassSessions, BeadsBackendPostgres},
		{"unknown backend value falls back to bd", BeadsConfig{Classes: map[string]BeadClassConfig{"nudges": {Backend: "mariadb"}}}, BeadClassNudges, BeadsBackendBD},
		{"empty backend value -> bd", BeadsConfig{Classes: map[string]BeadClassConfig{"sessions": {Backend: ""}}}, BeadClassSessions, BeadsBackendBD},
		{"unconfigured class -> bd", BeadsConfig{Classes: map[string]BeadClassConfig{"messaging": {Backend: "sqlite"}}}, BeadClassSessions, BeadsBackendBD},
		// Backward compatibility: the legacy top-level graph_store knob selects the
		// graph class when there is no explicit [beads.classes.graph] entry.
		{"legacy graph_store=sqlite -> graph sqlite", BeadsConfig{GraphStore: "sqlite"}, BeadClassGraph, BeadsBackendSQLite},
		{"legacy graph_store empty -> graph bd", BeadsConfig{}, BeadClassGraph, BeadsBackendBD},
		{"legacy graph_store does not leak to other classes", BeadsConfig{GraphStore: "sqlite"}, BeadClassMessaging, BeadsBackendBD},
		{"explicit graph class overrides absence of legacy knob", BeadsConfig{Classes: map[string]BeadClassConfig{"graph": {Backend: "sqlite"}}}, BeadClassGraph, BeadsBackendSQLite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.NormalizedClassBackend(tc.class); got != tc.want {
				t.Fatalf("NormalizedClassBackend(%q) = %q, want %q", tc.class, got, tc.want)
			}
		})
	}
}

// TestClassBackendUsesSQLite is the convenience predicate cutover wiring uses.
func TestClassBackendUsesSQLite(t *testing.T) {
	cfg := BeadsConfig{Classes: map[string]BeadClassConfig{"messaging": {Backend: "sqlite"}}}
	if !cfg.ClassUsesSQLite(BeadClassMessaging) {
		t.Error("ClassUsesSQLite(messaging) = false, want true")
	}
	if cfg.ClassUsesSQLite(BeadClassOrders) {
		t.Error("ClassUsesSQLite(orders) = true, want false")
	}
}
