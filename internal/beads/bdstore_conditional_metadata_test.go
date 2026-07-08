package beads_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBdStoreSetMetadataIfUsesGuardedJSONUpdate pins the exact conditional
// JSON UPDATE the BdStore emits for a compare-and-set: JSON_SET writes the next
// value, and the WHERE folds an absent key to "" via COALESCE(JSON_UNQUOTE(...))
// so it honors the empty-string clear contract. The dotted key must be one
// double-quoted JSON path member, not a nested path. rows_affected>0 → swapped.
func TestBdStoreSetMetadataIfUsesGuardedJSONUpdate(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte(`{"rows_affected":1,"schema_version":1}`), nil
	}
	s := beads.NewBdStore("/city", runner)

	swapped, err := s.SetMetadataIf(context.Background(), "bd-42", "gc.control_epoch", "1", "2")
	if err != nil {
		t.Fatalf("SetMetadataIf: %v", err)
	}
	if !swapped {
		t.Fatal("swapped = false, want true when rows_affected > 0")
	}
	if gotName != "bd" {
		t.Fatalf("runner name = %q, want bd", gotName)
	}
	if len(gotArgs) != 3 || gotArgs[0] != "sql" || gotArgs[1] != "--json" {
		t.Fatalf("args = %q, want bd sql --json <query>", gotArgs)
	}
	wantQuery := `UPDATE issues SET metadata = JSON_SET(COALESCE(metadata, JSON_OBJECT()), '$."gc.control_epoch"', '2'), updated_at = CURRENT_TIMESTAMP WHERE id = 'bd-42' AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$."gc.control_epoch"')), '') = '1'`
	if gotArgs[2] != wantQuery {
		t.Fatalf("SQL query =\n  %q\nwant\n  %q", gotArgs[2], wantQuery)
	}
}

// TestBdStoreSetMetadataIfMismatchReportsFalse pins that a 0-rows conditional
// UPDATE (the value moved out from under the caller) is the typed non-error
// conflict signal, not an error.
func TestBdStoreSetMetadataIfMismatchReportsFalse(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`{"rows_affected":0,"schema_version":1}`), nil
	}
	s := beads.NewBdStore("/city", runner)

	swapped, err := s.SetMetadataIf(context.Background(), "bd-42", "gc.control_epoch", "1", "2")
	if err != nil {
		t.Fatalf("SetMetadataIf: %v", err)
	}
	if swapped {
		t.Fatal("swapped = true, want false on a 0-rows conditional update")
	}
}

// TestBdStoreSetMetadataIfEscapesLiterals pins that the id, next value, expected
// value, and the JSON path all pass through bdSQLStringLiteral so a value with a
// single quote or backslash cannot break out of the SQL literal.
func TestBdStoreSetMetadataIfEscapesLiterals(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(`{"rows_affected":1}`), nil
	}
	s := beads.NewBdStore("/city", runner)

	if _, err := s.SetMetadataIf(context.Background(), "bd-'1", "k'x", "e'1", "n\\2"); err != nil {
		t.Fatalf("SetMetadataIf: %v", err)
	}
	wantQuery := `UPDATE issues SET metadata = JSON_SET(COALESCE(metadata, JSON_OBJECT()), '$."k''x"', 'n\\2'), updated_at = CURRENT_TIMESTAMP WHERE id = 'bd-''1' AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$."k''x"')), '') = 'e''1'`
	if gotArgs[2] != wantQuery {
		t.Fatalf("SQL query =\n  %q\nwant\n  %q", gotArgs[2], wantQuery)
	}
}

// TestBdStoreSetMetadataIfSetToSameUsesRead pins the set-to-same handling: when
// next == expected there is no value change, so a conditional UPDATE (which
// reports *changed* rows) would masquerade as a conflict. The store instead
// decides the precondition with a read and never issues an UPDATE.
func TestBdStoreSetMetadataIfSetToSameUsesRead(t *testing.T) {
	var calls []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte(`[{"id":"bd-42","title":"t","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z","metadata":{"gc.control_epoch":"5"}}]`), nil
	}
	s := beads.NewBdStore("/city", runner)

	swapped, err := s.SetMetadataIf(context.Background(), "bd-42", "gc.control_epoch", "5", "5")
	if err != nil {
		t.Fatalf("SetMetadataIf: %v", err)
	}
	if !swapped {
		t.Fatal("swapped = false, want true: precondition holds and next == expected")
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "bd sql ") {
			t.Fatalf("set-to-same issued a conditional UPDATE (%q); want a read-only precondition check", c)
		}
	}
}

// TestBdStoreSetMetadataIfSetToSameMissingBead pins that the set-to-same read
// path treats a missing bead as a non-match (false, nil), not an error.
func TestBdStoreSetMetadataIfSetToSameMissingBead(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1: issue not found: bd-99")
	}
	s := beads.NewBdStore("/city", runner)

	swapped, err := s.SetMetadataIf(context.Background(), "bd-99", "gc.control_epoch", "5", "5")
	if err != nil {
		t.Fatalf("SetMetadataIf on missing bead returned error, want (false, nil): %v", err)
	}
	if swapped {
		t.Fatal("swapped = true, want false on a missing bead")
	}
}

// TestBdStoreSetMetadataIfFallsBackToEmbeddedDolt pins that when `bd sql` is
// unsupported in embedded mode the store falls back to the same conditional
// UPDATE run through `dolt sql`, parsing ROW_COUNT() for the swap decision.
func TestBdStoreSetMetadataIfFallsBackToEmbeddedDolt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"demo"}`), 0o644); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}
	var calls []string
	runner := func(callDir, name string, args ...string) ([]byte, error) {
		calls = append(calls, callDir+": "+name+" "+strings.Join(args, " "))
		switch {
		case name == "bd" && len(args) >= 1 && args[0] == "sql":
			return nil, fmt.Errorf("exit status 1: Error: 'bd sql' is not yet supported in embedded mode")
		case name == "dolt" && len(args) == 5 && args[0] == "sql" && args[1] == "-r" && args[2] == "json" && args[3] == "-q":
			return []byte(`{"rows":[{"rows_affected":1}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected call %s %q", name, args)
		}
	}
	s := beads.NewBdStore(dir, runner)

	swapped, err := s.SetMetadataIf(context.Background(), "bd-42", "gc.control_epoch", "1", "2")
	if err != nil {
		t.Fatalf("SetMetadataIf: %v", err)
	}
	if !swapped {
		t.Fatal("swapped = false, want true via embedded-dolt fallback")
	}
	wantDoltQuery := filepath.Join(dir, ".beads", "embeddeddolt", "demo") + `: dolt sql -r json -q UPDATE issues SET metadata = JSON_SET(COALESCE(metadata, JSON_OBJECT()), '$."gc.control_epoch"', '2'), updated_at = CURRENT_TIMESTAMP WHERE id = 'bd-42' AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$."gc.control_epoch"')), '') = '1'; SELECT ROW_COUNT() AS rows_affected`
	if len(calls) != 2 || calls[1] != wantDoltQuery {
		t.Fatalf("calls =\n  %#v\nwant embedded dolt fallback:\n  %q", calls, wantDoltQuery)
	}
}

// TestBdStoreSetMetadataIfUnsupportedWithoutEmbeddedDolt pins the loud
// ErrConditionalMetadataUnsupported when neither `bd sql` nor an embedded-dolt
// backend can service the conditional update — never a silent no-op.
func TestBdStoreSetMetadataIfUnsupportedWithoutEmbeddedDolt(t *testing.T) {
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name == "bd" && len(args) >= 1 && args[0] == "sql" {
			return nil, fmt.Errorf("exit status 1: Error: 'bd sql' is not yet supported in embedded mode")
		}
		return nil, fmt.Errorf("unexpected call %s %q", name, args)
	}
	s := beads.NewBdStore(t.TempDir(), runner) // no .beads/metadata.json → not embedded dolt

	swapped, err := s.SetMetadataIf(context.Background(), "bd-42", "gc.control_epoch", "1", "2")
	if !errors.Is(err, beads.ErrConditionalMetadataUnsupported) {
		t.Fatalf("err = %v, want ErrConditionalMetadataUnsupported", err)
	}
	if swapped {
		t.Fatal("swapped = true, want false on unsupported")
	}
}
