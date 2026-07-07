package ir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// S0.1 spike: prove the emitted lumen.ir 0.2.5 contract is tractable to model in
// Go and pinnable, by decoding + validating + losslessly round-tripping the full
// vendored golden corpus and rejecting the schema-negative case.

func goldenFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("testdata/goldens/*.ir.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden IR files under testdata/goldens (err=%v)", err)
	}
	return files
}

func TestGoldensDecodeAndValidate(t *testing.T) {
	for _, f := range goldenFiles(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		doc, err := Decode(data)
		if err != nil {
			t.Errorf("%s: decode+validate failed: %v", filepath.Base(f), err)
			continue
		}
		if doc.Contract.Name != ContractName {
			t.Errorf("%s: contract.name = %q", filepath.Base(f), doc.Contract.Name)
		}
		if doc.Contract.Version != "0.2.5" {
			t.Errorf("%s: contract.version = %q", filepath.Base(f), doc.Contract.Version)
		}
		if len(doc.Nodes) == 0 {
			t.Errorf("%s: no top-level nodes decoded", filepath.Base(f))
		}
	}
}

// TestGoldensRoundTripLossless proves the typed decode preserves everything: a
// document decoded into the typed IR and re-marshaled is semantically identical
// to the original (compared as generic JSON so key order / whitespace don't
// matter).
func TestGoldensRoundTripLossless(t *testing.T) {
	for _, f := range goldenFiles(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var doc IR
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Errorf("%s: unmarshal: %v", filepath.Base(f), err)
			continue
		}
		reencoded, err := json.Marshal(doc)
		if err != nil {
			t.Errorf("%s: marshal: %v", filepath.Base(f), err)
			continue
		}
		var before, after any
		if err := json.Unmarshal(data, &before); err != nil {
			t.Fatalf("%s: unmarshal original to any: %v", f, err)
		}
		if err := json.Unmarshal(reencoded, &after); err != nil {
			t.Fatalf("%s: unmarshal re-encoded to any: %v", f, err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("%s: lossy round-trip through typed IR", filepath.Base(f))
		}
	}
}

// TestNodeKindCensus confirms every node kind used across the corpus is in the
// closed set, and reports which of the 26 kinds the golden corpus exercises (a
// coverage signal for the conformance strategy).
func TestNodeKindCensus(t *testing.T) {
	total := map[NodeKind]int{}
	for _, f := range goldenFiles(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		doc, err := Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for k, n := range doc.Kinds() {
			total[k] += n
		}
	}
	for k := range total {
		if !KnownNodeKinds[k] {
			t.Errorf("golden corpus uses unknown node kind %q", k)
		}
	}
	used := make([]string, 0, len(total))
	for k, n := range total {
		used = append(used, string(k)+"×"+itoa(n))
	}
	sort.Strings(used)
	var unused []string
	for k := range KnownNodeKinds {
		if total[k] == 0 {
			unused = append(unused, string(k))
		}
	}
	sort.Strings(unused)
	t.Logf("golden corpus exercises %d/%d node kinds: %v", len(total), len(KnownNodeKinds), used)
	t.Logf("node kinds NOT covered by goldens (%d): %v", len(unused), unused)
}

func TestNegativeCaseRejected(t *testing.T) {
	files, _ := filepath.Glob("testdata/negative/*.ir.json")
	if len(files) == 0 {
		t.Skip("no negative fixtures vendored")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := Decode(data); err == nil {
			t.Errorf("%s: expected decode to reject the negative case, got nil error", filepath.Base(f))
		} else {
			t.Logf("%s correctly rejected: %v", filepath.Base(f), err)
		}
	}
}

func TestKindEnumSizes(t *testing.T) {
	if len(KnownNodeKinds) != 26 {
		t.Errorf("KnownNodeKinds has %d entries, want 26 (the 0.2.5 emitted node enum)", len(KnownNodeKinds))
	}
	if len(KnownTypeKinds) != 8 {
		t.Errorf("KnownTypeKinds has %d entries, want 8", len(KnownTypeKinds))
	}
}

// TestRunNodeClosedShapeAt025 exercises the run-node closed-payload check
// directly at 0.2.5 — the vendored negative fixture is a 0.2.4 doc, so it is
// rejected on the version pin before run-node validation ever runs. This proves
// the phantom-field rejection mechanism independently.
func TestRunNodeClosedShapeAt025(t *testing.T) {
	const withPhantom = `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "main",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "run", "id": "run_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "target": {"kind": "by-name", "name": "echo"}, "outcome": "transparent",
         "phantomField": "should-be-rejected"}
      ]
    }`
	if _, err := Decode([]byte(withPhantom)); err == nil {
		t.Error("expected a run node with a phantom field to be rejected at 0.2.5")
	} else {
		t.Logf("phantom run field correctly rejected: %v", err)
	}

	// Positive control: the same node without the phantom field must pass.
	const clean = `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "main",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "run", "id": "run_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "target": {"kind": "by-name", "name": "echo"}, "outcome": "transparent"}
      ]
    }`
	if _, err := Decode([]byte(clean)); err != nil {
		t.Errorf("clean run node should validate, got: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
