package chartest

import "regexp"

// DefaultRules returns the canonicalization rules for gascity CLI output under
// the harness's file-store configuration (GC_BEADS=file → MemStore mints ids
// "gc-<n>"). It deliberately matches ONLY volatile minted tokens:
//
//   - bead ids: gc-<decimal>. Anchored with \b and \d+ (not [a-z0-9]+) so it
//     never clips the "gc" binary name, "gc-hosted"/"gc-runtime", a bare rig
//     prefix, or molecule refs like "mol-adopt-pr-v2".
//   - timestamps: RFC3339 / RFC3339Nano as emitted by stdlib time.Time JSON
//     marshaling — variable-length fractional seconds and either a trailing Z
//     or a numeric offset (local time serializes as ±hh:mm, not Z).
//
// It does NOT match stable identifiers (formula names, stable graph anchors
// like gcg-run-root, schema versions, small integers) or the real Dolt
// "ga-<base36>" ids, which are never minted under GC_DOLT=skip. Callers running
// against a real Dolt store add an anchored ga- rule themselves.
func DefaultRules() []Rule {
	return []Rule{
		{Category: "BEAD", Pattern: regexp.MustCompile(`\bgc-\d+\b`)},
		{Category: "T", Pattern: regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)},
	}
}
