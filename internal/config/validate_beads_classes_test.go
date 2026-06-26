package config

import (
	"strings"
	"testing"
)

func graphClass(backend string) *City {
	return &City{Beads: BeadsConfig{Classes: map[string]BeadClassConfig{BeadClassGraph: {Backend: backend}}}}
}

func classCfg(class, backend string) *City {
	return &City{Beads: BeadsConfig{Classes: map[string]BeadClassConfig{class: {Backend: backend}}}}
}

func TestValidateBeadsClasses(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *City
		wantErr bool
	}{
		{"nil cfg", nil, false},
		{"no classes", &City{}, false},
		{"graph unset", graphClass(""), false},
		{"graph bd", graphClass("bd"), false},
		{"graph sqlite", graphClass("sqlite"), false},
		{"graph sqlite case+space", graphClass("  SQLite "), false},
		// A typo'd backend is now REJECTED (fail-fast) rather than silently
		// normalizing to bd and diverting infra beads to the work store.
		{"graph typo rejected", graphClass("garbage"), true},
		{"graph postgres allowed", graphClass("postgres"), false},
		{"graph postgres case+space allowed", graphClass(" Postgres "), false},
		// Every relocatable class honors bd|sqlite|postgres; typos fail fast.
		{"orders postgres allowed", classCfg(BeadClassOrders, "postgres"), false},
		{"nudges postgres allowed", classCfg(BeadClassNudges, "postgres"), false},
		{"sessions postgres allowed", classCfg(BeadClassSessions, "postgres"), false},
		{"sessions sqlite allowed", classCfg(BeadClassSessions, "sqlite"), false},
		{"sessions unset allowed", classCfg(BeadClassSessions, ""), false},
		{"sessions typo rejected", classCfg(BeadClassSessions, "postgre"), true},
		{"messaging typo rejected", classCfg(BeadClassMessaging, "sqlit"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBeadsClasses(tc.cfg, "city.toml")
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("ValidateBeadsClasses(%s) = nil, want error", tc.name)
			case !tc.wantErr && err != nil:
				t.Fatalf("ValidateBeadsClasses(%s) = %v, want nil", tc.name, err)
			case tc.wantErr && !strings.Contains(err.Error(), "city.toml"):
				t.Errorf("error should name the source: %v", err)
			}
		})
	}
}

// TestNormalizeBackend pins the canonicalizer the guard and the dispatcher share.
func TestNormalizeBackend(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"bd":        BeadsBackendBD,
		"  BD ":     BeadsBackendBD,
		"sqlite":    BeadsBackendSQLite,
		" SQLite ":  BeadsBackendSQLite,
		"postgres":  BeadsBackendPostgres,
		"Postgres ": BeadsBackendPostgres,
		"garbage":   "",
	}
	for in, want := range cases {
		if got := normalizeBackend(in); got != want {
			t.Errorf("normalizeBackend(%q) = %q, want %q", in, got, want)
		}
	}
}
