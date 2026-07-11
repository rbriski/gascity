package config

import (
	"sort"
	"strings"
)

// reservedClassPrefixes maps each relocated coordination class (any backend) to
// the non-configurable bead-ID prefix its dedicated store mints. This is the
// single source of truth, consolidating cmd/gc's per-class prefix map and the
// graph-store prefix constant. Distinct prefixes keep cross-store ids
// unambiguous so a stranded bd-era id never resolves into the wrong store.
//
// BeadClassWork is intentionally absent: work beads stay on bd/Dolt under the
// rig/HQ EffectivePrefix, not a reserved class prefix.
var reservedClassPrefixes = map[string]string{
	BeadClassGraph:     "gcg",
	BeadClassMessaging: "gcm",
	BeadClassSessions:  "gcs",
	BeadClassOrders:    "gco",
	BeadClassNudges:    "gcn",
}

// InfraScopePrefix is the issue_prefix of a city's INFRA scope — the second
// (coordination) store on a split city. It is the graph class's reserved prefix
// ("gcg") because graph beads dominate infra volume and, uniquely, are minted by
// `bd graph apply`, which carries no explicit-ID field: every plan node the
// orchestration explosion materializes natively mints <InfraScopePrefix>-<n>.
//
// The other four coordination classes (sessions/messaging/nudges/orders) do NOT
// get their own per-class prefix in the infra store today. The pinned bd version
// rejects `bd create --id gcs-…` against a scope whose issue_prefix is "gcg"
// (prefix mismatch, resolvable only with --force, which BdStore does not emit),
// so a per-class explicit-ID scheme is deferred (see the E2 design's
// openQuestion #2 / risk #8). Instead all infra beads mint under this one scope
// prefix, which is a reserved class prefix, so the ID-prefix boundary invariant
// still holds: every infra-store bead carries a reserved class prefix, no
// domain-store bead does.
const InfraScopePrefix = "gcg"

// MintInfraBeadID returns a bead ID under the infra scope prefix for a freshly
// created infra bead: <InfraScopePrefix>-<suffix>. suffix must be a short,
// collision-resistant token (the caller supplies it so ID minting stays free of
// a crypto dependency in this package). The returned prefix segment satisfies
// IsReservedClassPrefix, and — because it equals the infra scope's issue_prefix —
// bd accepts it as an explicit --id without --force.
func MintInfraBeadID(suffix string) string {
	return InfraScopePrefix + "-" + suffix
}

// ReservedClassPrefix returns the reserved id-prefix for a relocated
// coordination class (any backend; e.g. BeadClassOrders -> "gco"), and whether
// the class has one. Classes without a reserved prefix (e.g. BeadClassWork)
// return ("", false).
func ReservedClassPrefix(class string) (string, bool) {
	p, ok := reservedClassPrefixes[class]
	return p, ok
}

// ReservedClassPrefixes returns a copy of the class -> reserved-prefix map.
func ReservedClassPrefixes() map[string]string {
	out := make(map[string]string, len(reservedClassPrefixes))
	for class, prefix := range reservedClassPrefixes {
		out[class] = prefix
	}
	return out
}

// IsReservedClassPrefix reports whether p (without a trailing "-") is a reserved
// class id-prefix. Case-insensitive, matching ValidateRigs' prefix handling.
func IsReservedClassPrefix(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return false
	}
	for _, reserved := range reservedClassPrefixes {
		if strings.ToLower(reserved) == p {
			return true
		}
	}
	return false
}

// reservedClassPrefixListText returns the reserved class id-prefixes as a
// sorted, comma-separated string for use in validation error messages.
func reservedClassPrefixListText() string {
	prefixes := make([]string, 0, len(reservedClassPrefixes))
	for _, p := range reservedClassPrefixes {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	return strings.Join(prefixes, ", ")
}
