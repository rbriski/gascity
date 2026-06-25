package config

import (
	"strings"
	"testing"
)

// TestReservedClassPrefix_MatchesOldValues pins the registry to the exact id
// prefixes that were previously hardcoded in cmd/gc (classSQLitePrefix +
// graphStoreIDPrefix), so the consolidation is byte-identical.
func TestReservedClassPrefix_MatchesOldValues(t *testing.T) {
	want := map[string]string{
		BeadClassGraph:     "gcg",
		BeadClassMessaging: "gcm",
		BeadClassSessions:  "gcs",
		BeadClassOrders:    "gco",
		BeadClassNudges:    "gcn",
	}
	for class, wantPrefix := range want {
		got, ok := ReservedClassPrefix(class)
		if !ok {
			t.Errorf("ReservedClassPrefix(%q) = (_, false), want a reserved prefix", class)
			continue
		}
		if got != wantPrefix {
			t.Errorf("ReservedClassPrefix(%q) = %q, want %q", class, got, wantPrefix)
		}
	}
	if len(ReservedClassPrefixes()) != len(want) {
		t.Errorf("ReservedClassPrefixes() has %d entries, want %d", len(ReservedClassPrefixes()), len(want))
	}
	// Work has no reserved class prefix.
	if p, ok := ReservedClassPrefix(BeadClassWork); ok {
		t.Errorf("ReservedClassPrefix(BeadClassWork) = (%q, true), want (\"\", false)", p)
	}
}

func TestReservedClassPrefixes_ReturnsCopy(t *testing.T) {
	m := ReservedClassPrefixes()
	m[BeadClassOrders] = "mutated"
	if got, _ := ReservedClassPrefix(BeadClassOrders); got != "gco" {
		t.Fatalf("mutating the returned map leaked into the registry: orders = %q, want gco", got)
	}
}

func TestIsReservedClassPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		want   bool
	}{
		{"gcg", true},
		{"gcm", true},
		{"gco", true},
		{"gcn", true},
		{"gcs", true},
		{"GCO", true}, // case-insensitive
		{" gco ", true},
		{"gc", false},   // the work prefix
		{"ga", false},   // the alternate work prefix
		{"fe", false},   // an ordinary rig prefix
		{"gco-", false}, // includes the namespace separator
		{"", false},
	}
	for _, tt := range tests {
		if got := IsReservedClassPrefix(tt.prefix); got != tt.want {
			t.Errorf("IsReservedClassPrefix(%q) = %v, want %v", tt.prefix, got, tt.want)
		}
	}
}

func TestValidateReservedPrefixesIn_Collision(t *testing.T) {
	tests := []struct {
		name      string
		rigs      []Rig
		hqPrefix  string
		wantClass string // a substring the error must mention
	}{
		{"rig prefix equals orders", []Rig{{Name: "r", Prefix: "gco"}}, "gc", "orders"},
		{"rig prefix equals messaging", []Rig{{Name: "r", Prefix: "gcm"}}, "gc", "messaging"},
		{"rig prefix equals graph", []Rig{{Name: "r", Prefix: "gcg"}}, "gc", "graph"},
		{"rig prefix equals nudges (case-insensitive)", []Rig{{Name: "r", Prefix: "GCN"}}, "gc", "nudges"},
		{"HQ prefix equals sessions", nil, "gcs", "sessions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateReservedPrefixesIn(tt.rigs, tt.hqPrefix)
			if err == nil {
				t.Fatalf("ValidateReservedPrefixesIn(%+v, %q) = nil, want a collision error", tt.rigs, tt.hqPrefix)
			}
			if !strings.Contains(err.Error(), tt.wantClass) {
				t.Fatalf("error %q does not mention class %q", err.Error(), tt.wantClass)
			}
		})
	}
}

func TestValidateReservedPrefixesIn_NoCollision(t *testing.T) {
	tests := []struct {
		name     string
		rigs     []Rig
		hqPrefix string
	}{
		{"ordinary rig prefixes", []Rig{{Name: "frontend", Prefix: "fe"}, {Name: "my-rig", Prefix: "mf"}}, "gc"},
		{"work prefixes are not reserved", []Rig{{Name: "a", Prefix: "gc"}, {Name: "b", Prefix: "ga"}}, "gc"},
		{"longer prefix disjoint from reserved", []Rig{{Name: "r", Prefix: "gcol"}}, "gc"},
		{"empty prefixes derive non-colliding", []Rig{{Name: "alpha"}, {Name: "beta"}}, "gc"},
		{"no rigs", nil, "gc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateReservedPrefixesIn(tt.rigs, tt.hqPrefix); err != nil {
				t.Fatalf("ValidateReservedPrefixesIn(%+v, %q) = %v, want nil", tt.rigs, tt.hqPrefix, err)
			}
		})
	}
}
