package main

import (
	"testing"
)

// TestRigList_CharacterizationGolden freezes the current per-lane behavior of
// `gc rig list` across the three routing lanes. It is the second command on the
// generalized harness and the first Phase-1 migration candidate.
//
// EXPECTED DIVERGENCE (grounded, see PROGRESS.md): rig list is a hand-reconciled
// API-vs-local pair. renderRigListFromAPI (API path, remote+alive lanes)
// hardcodes the HQ entry's Running=true (rationale: the controller answered, so
// it is up) and takes per-rig running/suspended from the API RigView; doRigList
// (serverless lane) derives HQ Running from controllerAlive(cityPath) and per-rig
// running from tmux probes. With no controller in the harness these DIVERGE on HQ
// Running (API true vs serverless false), and that divergence is exactly what the
// golden freezes — it makes the Move-1 reconciliation this command needs explicit
// and provable. remote and alive MUST still match (A==B); only serverless differs.
//
// The city has no rigs, isolating the HQ-entry divergence and avoiding the
// tmux/session probe path (rigListSessionProvider is only built when rigs exist).
// The harness redacts the temp cityPath and resets the per-process builtin-import
// warning cache so this config-reading command is deterministic and lane-fair.
func TestRigList_CharacterizationGolden(t *testing.T) {
	h := newCharCity(t, charCityBasic, nil)
	h.runCharGolden(t, charCommand{
		name:  "rig-list",
		route: routeRigList,
		// rig list derives its data from config, not the bead store — no
		// store read-back applies.
	})
}
