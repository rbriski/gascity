package beads

import (
	"errors"
	"testing"
)

// Gas City keys real runtime behavior off substrings of bd's free-text error
// output — the most fragile part of the bd contract, where a reworded message
// silently changes what gc does (e.g. a lost claim becomes a hard error, or a
// write-loss fallback goes undetected). These tests pin each classifier against
// the ACTUAL phrasings bd emits, sourced from the incidents cited below, plus
// negative cases that guard against over-matching. If a classifier is
// refactored and drops a phrasing, this fails offline immediately; if bd
// rewords a message, the live cross-version corpus/acceptance cells catch it.
//
// This is the "exit-codes-and-errors" contract domain — flagged by the contract
// design as the highest-risk untested surface. See
// engdocs/design/beads-gascity-contract-test-system.md.

func TestIsBdNotFoundContract(t *testing.T) {
	// bd emits the singular phrasing on stderr and the plural in its stdout
	// --json error envelope; classifyBDExecResult uses whichever stream has
	// content, so both must classify as not-found (the plural form was the
	// latent bug fixed alongside the corpus decoder).
	matches := []string{
		`Error fetching gcy-x: no issue found matching "gcy-x"`,
		`no issues found matching the provided IDs`,
		`issue not found`,
		`exit status 1: not found`,
	}
	rejects := []string{
		`exit status 1: bd create failed: validation error`,
		`dial tcp 127.0.0.1:3306: connect: connection refused`,
		`bead updated`,
		``,
	}
	for _, m := range matches {
		if !isBdNotFound(errors.New(m)) {
			t.Errorf("isBdNotFound should match bd not-found phrasing: %q", m)
		}
	}
	for _, m := range rejects {
		if isBdNotFound(errors.New(m)) {
			t.Errorf("isBdNotFound should NOT match unrelated error: %q", m)
		}
	}
	if isBdNotFound(nil) {
		t.Error("isBdNotFound(nil) must be false")
	}
}

func TestIsBdSQLUnsupportedContract(t *testing.T) {
	// gc enriches the ready projection via `bd sql`, which bd rejects in embedded
	// mode; gc must recognize that and degrade rather than surface a hard error.
	matches := []string{
		`exit status 1: Error: 'bd sql' is not yet supported in embedded mode`,
	}
	rejects := []string{
		`Error: 'bd sql' failed: syntax error near "SELEC"`, // sql is supported here, the query is bad
		`no issue found`,
		`not yet supported: some other feature`,
		``,
	}
	for _, m := range matches {
		if !isBdSQLUnsupportedInEmbeddedMode(errors.New(m)) {
			t.Errorf("isBdSQLUnsupportedInEmbeddedMode should match: %q", m)
		}
	}
	for _, m := range rejects {
		if isBdSQLUnsupportedInEmbeddedMode(errors.New(m)) {
			t.Errorf("isBdSQLUnsupportedInEmbeddedMode should NOT match: %q", m)
		}
	}
	if isBdSQLUnsupportedInEmbeddedMode(nil) {
		t.Error("isBdSQLUnsupportedInEmbeddedMode(nil) must be false")
	}
}

func TestIsBdClaimConflictContract(t *testing.T) {
	// When two actors race to claim a bead, bd reports a conflict; gc must treat
	// it as a lost race (not a hard failure) so convergence still works.
	// isBdClaimConflictMessage takes the raw message, not an error.
	matches := []string{
		`bead gcy-x is already assigned to alice`,
		`bead already claimed by bob`,
		`claimed by carol@example.test`,
		`claim conflict: another actor won the race`,
	}
	rejects := []string{
		`bead gcy-x updated`,
		`no issue found matching "gcy-x"`,
		`assignee set`,
		``,
	}
	for _, m := range matches {
		if !isBdClaimConflictMessage(m) {
			t.Errorf("isBdClaimConflictMessage should match claim-conflict phrasing: %q", m)
		}
	}
	for _, m := range rejects {
		if isBdClaimConflictMessage(m) {
			t.Errorf("isBdClaimConflictMessage should NOT match: %q", m)
		}
	}
}

func TestBdSilentFallbackContract(t *testing.T) {
	// bd losing its managed Dolt server and silently importing a stale
	// issues.jsonl into an empty database is a write-loss footgun
	// (gascity#1930 / sa-41j3kp). gc detects it by BOTH markers and converts the
	// exit-0 into a hard error — so one marker alone must NOT trip the detector.
	matches := []string{
		`auto-importing 12345 bytes into empty database`,
		`WARN: auto-importing .beads/issues.jsonl into empty database`,
		`AUTO-IMPORTING data INTO EMPTY DATABASE`, // case-insensitive
	}
	rejects := []string{
		`auto-importing 12345 bytes`,           // only the first marker
		`writing into empty database snapshot`, // only the second marker
		`normal bd output, nothing to see here`,
		``,
	}
	for _, m := range matches {
		if !bdOutputIndicatesSilentFallback(m) {
			t.Errorf("bdOutputIndicatesSilentFallback should match: %q", m)
		}
	}
	for _, m := range rejects {
		if bdOutputIndicatesSilentFallback(m) {
			t.Errorf("bdOutputIndicatesSilentFallback should NOT match (needs both markers): %q", m)
		}
	}
}
