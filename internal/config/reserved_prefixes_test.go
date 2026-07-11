package config

import "testing"

// The by-id ownership classifier must be namespace-shaped (first dash
// segment): bd's wisp tier mints <issue_prefix>-wisp-<suffix> ids, so a
// suffix-shape-sensitive heuristic (sling.BeadPrefix) classified
// "gcg-wisp-a7gc" (digit suffix → parsed prefix "gcg-wisp") and
// "gcg-wisp-znyf" (letter suffix → first-dash fallback "gcg") into DIFFERENT
// stores. These tests pin the first-segment rule across every shape that
// heuristic straddles.
func TestReservedClassBeadIDPrefix(t *testing.T) {
	tests := []struct {
		name       string
		beadID     string
		wantPrefix string
		wantOK     bool
	}{
		{"plain infra id", "gcg-1a2b3c4d", "gcg", true},
		{"bd-native numeric infra id", "gcg-42", "gcg", true},
		{"wisp id, digit-bearing hash suffix", "gcg-wisp-a7gc", "gcg", true},
		{"wisp id, letter-only hash suffix", "gcg-wisp-znyf", "gcg", true},
		{"wisp id, numeric suffix", "gcg-wisp-0042", "gcg", true},
		{"wisp id, long suffix", "gcg-wisp-3nvj3yx", "gcg", true},
		{"hierarchical child id", "gcg-1a2b.3", "gcg", true},
		{"case-insensitive", "GCG-WISP-A7GC", "gcg", true},
		{"other reserved classes", "gcs-77f1", "gcs", true},
		{"messaging class", "gcm-1", "gcm", true},
		{"orders class", "gco-1", "gco", true},
		{"nudges class", "gcn-1", "gcn", true},
		{"surrounding whitespace", "  gcg-wisp-a7gc  ", "gcg", true},
		{"work bead id", "ga-jaudf8", "", false},
		{"hyphenated rig prefix bead", "pieces-annotator-x8o", "", false},
		{"reserved-looking but longer first segment", "gcgx-1", "", false},
		{"bare reserved prefix, no dash", "gcg", "", false},
		{"empty", "", "", false},
		{"dash-only", "-", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPrefix, gotOK := ReservedClassBeadIDPrefix(tt.beadID)
			if gotPrefix != tt.wantPrefix || gotOK != tt.wantOK {
				t.Errorf("ReservedClassBeadIDPrefix(%q) = (%q, %v), want (%q, %v)",
					tt.beadID, gotPrefix, gotOK, tt.wantPrefix, tt.wantOK)
			}
			if got := IsReservedClassBeadID(tt.beadID); got != tt.wantOK {
				t.Errorf("IsReservedClassBeadID(%q) = %v, want %v", tt.beadID, got, tt.wantOK)
			}
		})
	}
}
