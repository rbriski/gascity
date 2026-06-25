package config

import (
	"fmt"
	"strings"
)

// reservedClassPrefixes maps each SQLite-relocated coordination class to the
// non-configurable bead-ID prefix its embedded store mints. This is the single
// source of truth, consolidating cmd/gc's classSQLitePrefix map and the
// graphStoreIDPrefix constant. Distinct prefixes keep cross-store ids
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

// ReservedClassPrefix returns the reserved id-prefix for a SQLite-relocated
// coordination class (e.g. BeadClassOrders -> "gco"), and whether the class has
// one. Classes without a reserved prefix (e.g. BeadClassWork) return ("", false).
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

// ValidateReservedPrefixesIn reports the first rig or HQ whose effective bead
// prefix collides with a reserved class prefix (gcg/gcm/gco/gcn/gcs). A collision
// would route work beads into a relocated class's SQLite store (or vice versa),
// since cross-store resolution keys purely on the id prefix. Collision is
// namespace overlap under the "prefix+'-'" id scheme: equal prefixes, or one
// namespace being a string-prefix of the other — the same rule the class-prefix
// disjointness guard enforces. Returns nil when clear.
func ValidateReservedPrefixesIn(rigs []Rig, hqPrefix string) error {
	check := func(owner, prefix string) error {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix == "" {
			return nil
		}
		pn := prefix + "-"
		for class, reserved := range reservedClassPrefixes {
			rn := strings.ToLower(reserved) + "-"
			if strings.HasPrefix(pn, rn) || strings.HasPrefix(rn, pn) {
				return fmt.Errorf("%s prefix %q collides with the reserved %q-class id prefix %q", owner, prefix, class, reserved)
			}
		}
		return nil
	}
	if err := check("HQ", hqPrefix); err != nil {
		return err
	}
	for _, r := range rigs {
		if err := check(fmt.Sprintf("rig %q", r.Name), r.EffectivePrefix()); err != nil {
			return err
		}
	}
	return nil
}
