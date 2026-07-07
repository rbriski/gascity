package main

import (
	"os"
	"strings"
)

// graphFrontierMode is the GC_GRAPH_FRONTIER kill switch for the control
// dispatcher's frontier read (P2). It selects how the journal ControlFrontier
// SELECT relates to the legacy `bd | jq` serve-tick frontier. It is a small
// typed vocabulary rather than a raw os.Getenv scattered through the serve tick,
// so every call site reasons about the same three modes and the default is
// unmistakably legacy.
type graphFrontierMode int

const (
	// frontierModeLegacy is the DEFAULT (unset or unrecognized value): only the
	// legacy `bd | jq` frontier runs. The journal leg is never opened or probed,
	// so an opted-in city and a non-opted city are both byte-identical to the
	// pre-P2 serve tick. This is the INERT guarantee.
	frontierModeLegacy graphFrontierMode = iota

	// frontierModeShadow runs BOTH the legacy frontier and the journal
	// ControlFrontier for the same tick, compares their ready-bead id sets, emits
	// a frontier.shadow.divergence event when they differ, and SERVES the legacy
	// result unchanged. It is the safety gate: zero divergence events over a soak
	// is the exit criterion for promoting a city to serve.
	frontierModeShadow

	// frontierModeServe serves the journal ControlFrontier for journal-resident
	// roots and the legacy `bd | jq` frontier for legacy roots, unioned/deduped by
	// id (residence makes the two sets disjoint).
	frontierModeServe
)

// graphFrontierModeEnvVar names the kill-switch environment variable.
const graphFrontierModeEnvVar = "GC_GRAPH_FRONTIER"

// parseGraphFrontierMode maps a raw env value to a mode. Unknown, empty, and
// unset all collapse to legacy — the safe default — so a typo can never silently
// activate the journal leg.
func parseGraphFrontierMode(raw string) graphFrontierMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "shadow":
		return frontierModeShadow
	case "serve":
		return frontierModeServe
	default:
		return frontierModeLegacy
	}
}

// graphFrontierMode reads GC_GRAPH_FRONTIER once and returns the parsed mode.
// It is read per serve tick (cheap: one os.Getenv + a switch) so an operator can
// flip the mode without restarting the dispatcher, matching the reversibility the
// P2 design requires.
func currentGraphFrontierMode() graphFrontierMode {
	return parseGraphFrontierMode(os.Getenv(graphFrontierModeEnvVar))
}

// String renders the mode for trace lines.
func (m graphFrontierMode) String() string {
	switch m {
	case frontierModeShadow:
		return "shadow"
	case frontierModeServe:
		return "serve"
	default:
		return "legacy"
	}
}
