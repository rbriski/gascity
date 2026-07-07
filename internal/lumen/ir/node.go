package ir

import "encoding/json"

// Node is one emitted IR node. Kind/ID/Name/After/Origin are typed conveniences;
// Raw preserves every field verbatim — including kind-specific payload and nested
// child nodes (block members, dispatch arms, scatter bodies, …) — so decode then
// encode is lossless. Typed per-kind payload structs are Phase 1 work, derived
// from docs/spec/ir.lumen; the emitted node payload is open (schema
// additionalProperties:true) for every kind except run.
type Node struct {
	Kind   NodeKind
	ID     string
	Name   string
	After  []string
	Origin Origin
	Raw    map[string]json.RawMessage
}

// UnmarshalJSON preserves the full object in Raw and lifts the common envelope
// fields into typed accessors.
func (n *Node) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	n.Raw = raw
	decodeField(raw, "kind", &n.Kind)
	decodeField(raw, "id", &n.ID)
	decodeField(raw, "name", &n.Name)
	decodeField(raw, "after", &n.After)
	decodeField(raw, "origin", &n.Origin)
	return nil
}

// MarshalJSON re-emits the preserved object, guaranteeing lossless round-trip.
func (n Node) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.Raw)
}

// IR is a decoded lumen.ir document. As with Node, Raw preserves the whole
// document so encode is lossless; the typed fields are conveniences.
type IR struct {
	Contract Contract
	Name     string
	Input    InputDecl
	Nodes    []Node
	Origin   Origin
	Raw      map[string]json.RawMessage
}

// UnmarshalJSON decodes the envelope, typing the top-level nodes.
func (ir *IR) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	ir.Raw = raw
	if err := decodeFieldStrict(raw, "contract", &ir.Contract); err != nil {
		return err
	}
	if err := decodeFieldStrict(raw, "nodes", &ir.Nodes); err != nil {
		return err
	}
	decodeField(raw, "name", &ir.Name)
	decodeField(raw, "input", &ir.Input)
	decodeField(raw, "origin", &ir.Origin)
	return nil
}

// MarshalJSON re-emits the preserved document.
func (ir IR) MarshalJSON() ([]byte, error) {
	return json.Marshal(ir.Raw)
}

func decodeField(raw map[string]json.RawMessage, key string, dst any) {
	if v, ok := raw[key]; ok {
		_ = json.Unmarshal(v, dst)
	}
}

func decodeFieldStrict(raw map[string]json.RawMessage, key string, dst any) error {
	if v, ok := raw[key]; ok {
		return json.Unmarshal(v, dst)
	}
	return nil
}

// WalkNodes visits every IR node in the document — the top-level nodes and all
// nested nodes, regardless of the field they nest under — calling fn with each
// node's raw object. It identifies a node structurally: a JSON object carrying
// kind, id, and after (which distinguishes true IR nodes from expression values
// like {"kind":"literal", ...} that carry a kind but no id/after).
func (ir *IR) WalkNodes(fn func(node map[string]json.RawMessage)) {
	for i := range ir.Nodes {
		walkRawObject(ir.Nodes[i].Raw, fn)
	}
}

func walkRawObject(obj map[string]json.RawMessage, fn func(map[string]json.RawMessage)) {
	_, hasKind := obj["kind"]
	_, hasID := obj["id"]
	_, hasAfter := obj["after"]
	if hasKind && hasID && hasAfter {
		fn(obj)
	}
	for _, v := range obj {
		walkRawValue(v, fn)
	}
}

func walkRawValue(v json.RawMessage, fn func(map[string]json.RawMessage)) {
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			var m map[string]json.RawMessage
			if json.Unmarshal(v, &m) == nil {
				walkRawObject(m, fn)
			}
			return
		case '[':
			var arr []json.RawMessage
			if json.Unmarshal(v, &arr) == nil {
				for _, e := range arr {
					walkRawValue(e, fn)
				}
			}
			return
		default:
			return
		}
	}
}
