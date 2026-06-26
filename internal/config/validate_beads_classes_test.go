package config

import (
	"strings"
	"testing"
)

func graphClass(backend string) *City {
	return &City{Beads: BeadsConfig{Classes: map[string]BeadClassConfig{BeadClassGraph: {Backend: backend}}}}
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
		{"graph unknown normalizes to bd", graphClass("garbage"), false},
		// graph=postgres is now honored: the Router registers a Postgres graph
		// backend (registerGraphStoreBackend). The reject branch is forward-defense
		// for a future enum backend graph can't honor — unreachable while
		// normalizeBackend's recognized set == graph's honored set.
		{"graph postgres allowed", graphClass("postgres"), false},
		{"graph postgres case+space allowed", graphClass(" Postgres "), false},
		// Other classes route through resolveClassStore, so postgres is fine there.
		{"orders postgres allowed", &City{Beads: BeadsConfig{Classes: map[string]BeadClassConfig{BeadClassOrders: {Backend: "postgres"}}}}, false},
		{"nudges postgres allowed", &City{Beads: BeadsConfig{Classes: map[string]BeadClassConfig{BeadClassNudges: {Backend: "postgres"}}}}, false},
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
