package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

// bundleDoc is a minimal valid main IR that runs a sub-formula, with the
// sub-formula body carried in the optional top-level `formulas` bundle.
const bundleDoc = `{
  "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "donbox/formula-language"},
  "name": "main",
  "input": {"name": "main.input", "fields": []},
  "nodes": [
    {"kind": "run", "id": "greeting", "name": "greeting", "after": [],
     "target": {"kind": "by-name", "name": "greeter"},
     "environment": {"fields": []},
     "outcome": "transparent"}
  ],
  "formulas": {
    "greeter": {
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "donbox/formula-language"},
      "name": "greeter",
      "input": {"name": "greeter.input", "fields": []},
      "nodes": [
        {"kind": "exec", "id": "hello", "name": "hello", "after": [],
         "interpreter": {"program": {"kind": "shell"}}, "body": {"raw": "echo hi"}}
      ]
    }
  }
}`

// TestBundleDecodeAndLosslessRoundTrip proves the optional top-level `formulas`
// bundle decodes into IR.Formulas and re-marshals byte-identically (Raw
// passthrough), so irHash and every content-addressed blob are unaffected.
func TestBundleDecodeAndLosslessRoundTrip(t *testing.T) {
	doc, err := Decode([]byte(bundleDoc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if doc.Formulas == nil {
		t.Fatal("Formulas map is nil, want the greeter sub-formula")
	}
	sub, ok := doc.Formulas["greeter"]
	if !ok {
		t.Fatalf("Formulas missing greeter; have %v", keys(doc.Formulas))
	}
	if sub.Name != "greeter" {
		t.Errorf("sub.Name = %q, want greeter", sub.Name)
	}
	if len(sub.Nodes) != 1 || sub.Nodes[0].ID != "hello" {
		t.Errorf("sub.Nodes = %+v, want one node id=hello", sub.Nodes)
	}

	// Re-marshal must byte-match a raw-passthrough re-encoding of the same input:
	// IR.MarshalJSON marshals ir.Raw (a map[string]json.RawMessage), so Go sorts
	// only the top-level keys and emits every nested value verbatim. The canonical
	// comparison is therefore a generic map[string]json.RawMessage decode/encode.
	assertLosslessMarshal(t, doc, bundleDoc)
}

// assertLosslessMarshal proves Marshal(Decode(src)) == Marshal(rawDecode(src)),
// the Raw-passthrough losslessness guarantee that keeps irHash stable.
func assertLosslessMarshal(t *testing.T, doc *IR, src string) {
	t.Helper()
	got, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(src), &raw); err != nil {
		t.Fatalf("raw unmarshal: %v", err)
	}
	want, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("raw marshal: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("round-trip not lossless:\n got=%s\nwant=%s", got, want)
	}
}

// TestNoBundleByteIdentity proves a run-free document with no `formulas` key
// decodes with a nil Formulas map and re-marshals byte-identically — the
// backward-compatibility guarantee (absent bundle ⇒ unchanged irHash).
func TestNoBundleByteIdentity(t *testing.T) {
	const plain = `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"m","input":{"name":"m.input","fields":[]},` +
		`"nodes":[{"kind":"exec","id":"a","name":"a","after":[],"body":{"raw":"echo"}}]}`
	doc, err := Decode([]byte(plain))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if doc.Formulas != nil {
		t.Errorf("Formulas = %v, want nil for a run-free doc", doc.Formulas)
	}
	assertLosslessMarshal(t, doc, plain)
}

// TestBundleValidateRecursesIntoSubDocs proves that an unknown node kind hidden
// inside a sub-formula body is a load error, not a runtime surprise — Validate
// recurses into every bundled sub-doc.
func TestBundleValidateRecursesIntoSubDocs(t *testing.T) {
	bad := strings.Replace(bundleDoc, `"kind": "exec", "id": "hello"`, `"kind": "bogus", "id": "hello"`, 1)
	_, err := Decode([]byte(bad))
	if err == nil {
		t.Fatal("Decode accepted an unknown sub-doc node kind")
	}
	if !strings.Contains(err.Error(), "formulas[\"greeter\"]") || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not name formulas[\"greeter\"] + the bad kind", err)
	}
}

// TestBundlePhantomRunFieldInSubDocRefused proves the run-node closed field-set
// is enforced INSIDE bundled sub-docs too.
func TestBundlePhantomRunFieldInSubDocRefused(t *testing.T) {
	sub := `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"s","input":{"name":"s.input","fields":[]},` +
		`"nodes":[{"kind":"run","id":"r","name":"r","after":[],"target":{"kind":"by-name","name":"z"},` +
		`"outcome":"transparent","phantom":1}]}`
	doc := `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"main","input":{"name":"main.input","fields":[]},"nodes":[],` +
		`"formulas":{"s":` + sub + `}}`
	_, err := Decode([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "phantom") {
		t.Fatalf("want a phantom-field refusal for the sub-doc run node, got %v", err)
	}
}

// TestBundleNameKeyMismatchRefused proves a sub-doc whose declared name differs
// from its bundle key is refused (a malformed bundle).
func TestBundleNameKeyMismatchRefused(t *testing.T) {
	bad := strings.Replace(bundleDoc, `"name": "greeter",`, `"name": "wrongname",`, 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "greeter") || !strings.Contains(err.Error(), "wrongname") {
		t.Fatalf("want a name/key mismatch refusal, got %v", err)
	}
}

// TestBundleNestedBundleRefused proves the bundle must be FLAT: a sub-doc may
// not itself carry a `formulas` bundle (one closure, no lookup ambiguity).
func TestBundleNestedBundleRefused(t *testing.T) {
	nested := strings.Replace(bundleDoc,
		`"nodes": [
        {"kind": "exec", "id": "hello", "name": "hello", "after": [],
         "interpreter": {"program": {"kind": "shell"}}, "body": {"raw": "echo hi"}}
      ]`,
		`"nodes": [], "formulas": {"deeper": {"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},"name":"deeper","input":{"name":"d.input","fields":[]},"nodes":[]}}`,
		1)
	_, err := Decode([]byte(nested))
	if err == nil || !strings.Contains(err.Error(), "flat") {
		t.Fatalf("want a nested-bundle refusal, got %v", err)
	}
}

// TestBundleNullEntryRefused proves a null bundle entry is refused loudly.
func TestBundleNullEntryRefused(t *testing.T) {
	doc := `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"main","input":{"name":"main.input","fields":[]},"nodes":[],` +
		`"formulas":{"greeter":null}}`
	_, err := Decode([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "greeter") {
		t.Fatalf("want a null-entry refusal naming greeter, got %v", err)
	}
}

// TestBundleUnsupportedSubContractRefused proves a sub-doc pinned to an
// unsupported contract version is a load error.
func TestBundleUnsupportedSubContractRefused(t *testing.T) {
	bad := strings.Replace(bundleDoc, `"version": "0.2.5", "producer": "donbox/formula-language"},
      "name": "greeter"`, `"version": "9.9.9", "producer": "x"},
      "name": "greeter"`, 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "greeter") {
		t.Fatalf("want an unsupported sub-contract refusal naming greeter, got %v", err)
	}
}

// TestBundleMalformedSubInputRefused proves a type-mismatched input decl in a
// bundled sub-doc is a LOAD error, not a silently-zeroed struct. R1's decodeRunEnv
// reads sub.Input.Fields to enforce "required unbound field is a loud lowering
// error"; a lenient decode (Required silently false) would defeat that guard and
// run the sub-graph with an unbound {{ref}} spliced verbatim.
func TestBundleMalformedSubInputRefused(t *testing.T) {
	sub := `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"greeter","input":{"name":"greeter.input","fields":[` +
		`{"name":"who","type":{"kind":"atomic","name":"string"},"required":"true"}]},` + // required is a STRING, not bool
		`"nodes":[{"kind":"exec","id":"hello","name":"hello","after":[],"body":{"raw":"echo hi"}}]}`
	doc := `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"main","input":{"name":"main.input","fields":[]},"nodes":[],` +
		`"formulas":{"greeter":` + sub + `}}`
	if _, err := Decode([]byte(doc)); err == nil {
		t.Fatal("Decode accepted a sub-doc with a type-mismatched `required` field (lenient input decode defeats the run-env guard)")
	}
}

func keys(m map[string]*IR) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
