package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// doMetaNodeIR renders a minimal do node whose optional top-level "metadata" object
// is metaJSON (empty string ⇒ no metadata field), for the decode-time wall pins.
func doMetaNodeIR(t *testing.T, metaJSON string) ir.Node {
	t.Helper()
	metaField := ""
	if metaJSON != "" {
		metaField = `"metadata":` + metaJSON + `,`
	}
	src := `{"kind":"do","id":"hello","name":"hello","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},"source":{"kind":"prompt"},` + metaField +
		`"interpreter":{"kind":"agent","mode":{"kind":"do"},"origin":{"uri":"t","line":1,"col":0}},` +
		`"body":{"raw":"hi","source":{"kind":"inline"},"language":"markdown","origin":{"uri":"t","line":1,"col":0}}}`
	var n ir.Node
	if err := json.Unmarshal([]byte(src), &n); err != nil {
		t.Fatalf("unmarshal do node: %v", err)
	}
	return n
}

// TestDecodeDoMetadataStaticLiterals proves decodeDo lifts a static string map off
// the open do payload onto the leaf verbatim (the passthrough seam's source of truth).
func TestDecodeDoMetadataStaticLiterals(t *testing.T) {
	s, err := decodeDo(doMetaNodeIR(t, `{"gc.continuation_group":"main","gc.scope_ref":"release-7"}`))
	if err != nil {
		t.Fatalf("decodeDo: %v", err)
	}
	want := map[string]string{"gc.continuation_group": "main", "gc.scope_ref": "release-7"}
	if len(s.metadata) != len(want) {
		t.Fatalf("metadata = %v, want %v", s.metadata, want)
	}
	for k, v := range want {
		if s.metadata[k] != v {
			t.Errorf("metadata[%q] = %q, want %q", k, s.metadata[k], v)
		}
	}
}

// TestDecodeDoMetadataAbsent proves a do with no metadata object decodes to a nil map
// (no field on the wire), so a metadata-free stream folds and dispatches as pre-seam.
func TestDecodeDoMetadataAbsent(t *testing.T) {
	s, err := decodeDo(doMetaNodeIR(t, ""))
	if err != nil {
		t.Fatalf("decodeDo: %v", err)
	}
	if s.metadata != nil {
		t.Fatalf("metadata = %v, want nil (absent object)", s.metadata)
	}
	// An empty object is likewise nil (no keys ⇒ nothing to carry).
	s2, err := decodeDo(doMetaNodeIR(t, `{}`))
	if err != nil {
		t.Fatalf("decodeDo empty: %v", err)
	}
	if s2.metadata != nil {
		t.Fatalf("empty-object metadata = %v, want nil", s2.metadata)
	}
}

// TestDecodeDoMetadataRefusesInterpolatedValue pins the load-bearing determinism
// wall: a value carrying a {{…}} template is refused at decode (wrapping
// ErrUnsupportedNode), because dynamic metadata would re-render against scope and
// break byte-identical re-adoption.
func TestDecodeDoMetadataRefusesInterpolatedValue(t *testing.T) {
	for _, val := range []string{"{{who}}", "team-{{who}}", "{{ base[i] }}", "{{ pick(x) }}"} {
		v, _ := json.Marshal(val)
		_, err := decodeDo(doMetaNodeIR(t, `{"gc.continuation_group":`+string(v)+`}`))
		if !errors.Is(err, ErrUnsupportedNode) {
			t.Fatalf("decodeDo(metadata value %q) err = %v, want wrapped ErrUnsupportedNode", val, err)
		}
	}
}

// TestDecodeDoMetadataRefusesNonStringValue pins the unmarshal-refusal arm: a metadata
// object whose value is not a string (a number, a nested object, an array, or a bool)
// cannot ride onto the map[string]string work bead, so it is refused LOUD at decode
// (wrapping ErrUnsupportedNode, naming the do and "metadata") — never a silently
// dropped/zeroed key. Mutation — swallowing the unmarshal error — turns this RED.
func TestDecodeDoMetadataRefusesNonStringValue(t *testing.T) {
	for _, meta := range []string{
		`{"gc.continuation_group":1}`,         // number
		`{"gc.continuation_group":{"n":"m"}}`, // nested object
		`{"gc.continuation_group":["a","b"]}`, // array
		`{"gc.continuation_group":true}`,      // bool
	} {
		_, err := decodeDo(doMetaNodeIR(t, meta))
		if !errors.Is(err, ErrUnsupportedNode) {
			t.Fatalf("decodeDo(metadata %s) err = %v, want wrapped ErrUnsupportedNode", meta, err)
		}
		if !strings.Contains(err.Error(), "hello") || !strings.Contains(err.Error(), "metadata") {
			t.Errorf("decodeDo(metadata %s) err = %q, want it to name the do and \"metadata\"", meta, err.Error())
		}
	}
}

// TestDecodeDoMetadataRefusesReservedKey pins the clobber guard: each engine-owned
// routing key is refused as static do metadata (wrapping ErrUnsupportedNode) so a
// pack can never override the authoritative keys the dispatch seam stamps.
func TestDecodeDoMetadataRefusesReservedKey(t *testing.T) {
	for _, key := range []string{
		beadmeta.RoutedToMetadataKey,
		beadmeta.LumenRunMetadataKey,
		beadmeta.LumenActivationMetadataKey,
		beadmeta.LumenAttemptMetadataKey,
	} {
		k, _ := json.Marshal(key)
		_, err := decodeDo(doMetaNodeIR(t, `{`+string(k)+`:"x"}`))
		if !errors.Is(err, ErrUnsupportedNode) {
			t.Fatalf("decodeDo(reserved key %q) err = %v, want wrapped ErrUnsupportedNode", key, err)
		}
	}
}
