package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// graphCutoverMarkerName is the file whose mere presence under <city>/.gc/graph
// arms the generational cutover (P3.3). Presence means "new graph roots are born
// journal-resident"; absence is the P1.5/P2 inert behavior (new roots mint on the
// legacy leg, byte-identical). Like the graph-journal scope marker, the file
// content is a human-legible note and is NEVER parsed for routing — only its
// presence is load-bearing.
const graphCutoverMarkerName = "cutover"

// graphCutoverMarkerPath is <city>/.gc/graph/cutover, alongside the graph-journal
// scope marker (.gc/graph/.beads/config.yaml).
func graphCutoverMarkerPath(cityPath string) string {
	return filepath.Join(graphScopeRoot(cityPath), graphCutoverMarkerName)
}

// cityGraphCutoverArmed reports whether the generational cutover marker is present.
// Only a clean os.Stat success arms it; every error (IsNotExist or any transient
// stat fault) reads as NOT armed, so the conservative default is always the inert
// legacy path — a marker can never be *implied* by an unreadable directory. It is
// stat'd on the new-root create hot path and per serve-tick frontier resolution;
// both are rare/cheap enough for a single os.Stat, and re-reading it live is what
// makes marker removal restore legacy behavior immediately (no restart).
func cityGraphCutoverArmed(cityPath string) bool {
	if strings.TrimSpace(cityPath) == "" {
		return false
	}
	_, err := os.Stat(graphCutoverMarkerPath(cityPath))
	return err == nil
}

// graphFrontierModeForCity resolves the control-dispatcher frontier mode for a
// city, folding the cutover marker into the GC_GRAPH_FRONTIER kill switch (P3.3
// item 2, "marker-implies-serve"):
//
//   - An explicit, non-blank GC_GRAPH_FRONTIER always wins — it is the kill
//     switch. Explicit "legacy" forces legacy even on an armed city; "shadow" and
//     "serve" force their modes; an unrecognized value collapses to legacy
//     (typo-safe, so a mistyped env can never silently activate the journal leg).
//   - With the env unset or blank, the cutover marker promotes the default from
//     legacy to serve. So an armed city serves the journal ControlFrontier for its
//     journal-resident roots without the operator also having to export an env var,
//     while an un-armed city stays byte-identical to the pre-P3.3 legacy default.
//
// This replaces the env-only resolution the two serve-tick call sites used before
// P3.3; it is read per tick (cheap: one LookupEnv + at most one os.Stat) so a
// mode/marker flip takes effect without restarting the dispatcher.
func graphFrontierModeForCity(cityPath string) graphFrontierMode {
	if raw, ok := os.LookupEnv(graphFrontierModeEnvVar); ok && strings.TrimSpace(raw) != "" {
		return parseGraphFrontierMode(raw)
	}
	if cityGraphCutoverArmed(cityPath) {
		return frontierModeServe
	}
	return frontierModeLegacy
}

// graphCutoverMarkerContent is the human-legible note written into the marker. It
// is never parsed; only the file's presence matters.
func graphCutoverMarkerContent() string {
	return "# gc migrate graph-journal cutover marker\n" +
		"# Presence arms the generational cutover: new graph roots are born\n" +
		"# journal-resident and the control frontier defaults to serve for\n" +
		"# journal-resident roots (GC_GRAPH_FRONTIER still overrides as the kill\n" +
		"# switch). Remove with: gc migrate graph-journal cutover --disarm\n" +
		"# Already-born-journal roots stay journal (generational; no un-migration).\n" +
		fmt.Sprintf("armed_at: %s\n", time.Now().UTC().Format(time.RFC3339)) +
		"parity_verified: true\n"
}

// cutoverParityRefusalMessage is printed when an operator tries to arm the cutover
// without attesting the P3.1 parity gate. The gate is an integration test that
// cannot run at CLI time, so arming requires an explicit --parity-verified
// acknowledgment; this message names the exact command to run first.
const cutoverParityRefusalMessage = `gc migrate graph-journal cutover: refusing to arm without parity verification.

Arming makes NEW graph roots born journal-resident and defaults the control
frontier to serve for journal roots. It is control-plane-only: a born-journal
root's CONTROL steps drain, but its WORKER steps are invisible to workers until
P4 — do NOT arm a city running worker formulas yet. That is only safe once the
P3.1 parity gate is green for THIS build. The gate is an integration test that
cannot run at CLI time, so you must run it yourself and then attest with
--parity-verified:

    go test -tags integration -run TestControlFrontierParityAgainstRealBd ./cmd/gc/

Once it passes, re-run:

    gc migrate graph-journal cutover --parity-verified`

// migrateGraphJournalArmCutover writes the cutover marker for cityPath. It REFUSES
// on a city that has not opted into the graph-journal scope (no journal leg exists
// to mint roots onto), directing the operator to run "init" first. Idempotent: an
// already-armed city is re-stamped. The parity gate is enforced by the command
// layer (a required --parity-verified flag), not here.
func migrateGraphJournalArmCutover(cityPath string) error {
	present, err := cityGraphScopePresence(cityPath)
	if err != nil {
		return fmt.Errorf("probing graph scope: %w", err)
	}
	if !present {
		return fmt.Errorf("city is not opted into the graph-journal scope; run \"gc migrate graph-journal init\" first")
	}
	if err := os.MkdirAll(graphScopeRoot(cityPath), 0o755); err != nil {
		return fmt.Errorf("creating graph scope dir %q: %w", graphScopeRoot(cityPath), err)
	}
	marker := graphCutoverMarkerPath(cityPath)
	if err := atomicWriteFile(marker, []byte(graphCutoverMarkerContent())); err != nil {
		return fmt.Errorf("writing cutover marker %q: %w", marker, err)
	}
	return nil
}

// migrateGraphJournalDisarmCutover removes the cutover marker, restoring legacy
// minting immediately. It is generational: already-born-journal roots keep their
// residence (there is no un-migration), but their in-flight dispatch quiesces
// because the frontier defaults back to legacy (the caller warns; resume via
// GC_GRAPH_FRONTIER=serve or re-arm). A disarm mid-drain that flips a follow-loop
// pass from serve to legacy (LOW-1) is the pre-existing env-switch class and
// self-heals on the next pass, so it needs no extra guard here. Idempotent: a city
// with no marker is a no-op.
func migrateGraphJournalDisarmCutover(cityPath string) error {
	marker := graphCutoverMarkerPath(cityPath)
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing cutover marker %q: %w", marker, err)
	}
	return nil
}
