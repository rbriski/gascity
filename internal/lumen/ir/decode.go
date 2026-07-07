package ir

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Decode parses a lumen.ir document and validates its contract identity and node
// taxonomy. It fails at load time — never at run time — on an unknown contract
// name/version or an unknown node kind, so a bad or drifted IR is a load error,
// not a runtime surprise.
func Decode(data []byte) (*IR, error) {
	var doc IR
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decoding lumen.ir: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return &doc, nil
}

// Validate checks the contract envelope and every node (including nested ones)
// against the closed emitted taxonomy. It also enforces the one closed node
// payload the schema pins: the run node.
func (ir *IR) Validate() error {
	if ir.Contract.Name != ContractName {
		return fmt.Errorf("lumen.ir: contract.name is %q, want %q", ir.Contract.Name, ContractName)
	}
	if !SupportedVersions[ir.Contract.Version] {
		return fmt.Errorf("lumen.ir: unsupported contract.version %q (supported: %s)",
			ir.Contract.Version, supportedVersionList())
	}

	var problems []string
	ir.WalkNodes(func(node map[string]json.RawMessage) {
		var kind string
		_ = json.Unmarshal(node["kind"], &kind)
		if !KnownNodeKinds[NodeKind(kind)] {
			var id string
			_ = json.Unmarshal(node["id"], &id)
			problems = append(problems, fmt.Sprintf("unknown node kind %q (node %q)", kind, id))
			return
		}
		if NodeKind(kind) == NodeRun {
			if err := validateRunNode(node); err != nil {
				problems = append(problems, err.Error())
			}
		}
	})
	if len(problems) > 0 {
		return fmt.Errorf("lumen.ir: %s", strings.Join(problems, "; "))
	}
	return nil
}

// runNodeFields is the closed key set for a run node (schema runNode,
// additionalProperties:false). The run payload is the one kind the 0.2.5 schema
// pins, so a phantom field on a run node is a contract violation we must reject.
var runNodeFields = map[string]bool{
	"kind": true, "id": true, "name": true, "after": true, "origin": true,
	"target": true, "with": true, "environment": true, "runInput": true, "outcome": true,
}

func validateRunNode(node map[string]json.RawMessage) error {
	var id string
	_ = json.Unmarshal(node["id"], &id)
	for key := range node {
		if !runNodeFields[key] {
			return fmt.Errorf("run node %q has unexpected field %q (closed payload)", id, key)
		}
	}
	if _, ok := node["target"]; !ok {
		return fmt.Errorf("run node %q missing required field \"target\"", id)
	}
	if _, ok := node["outcome"]; !ok {
		return fmt.Errorf("run node %q missing required field \"outcome\"", id)
	}
	return nil
}

// Kinds returns the census of node kinds used across the document (including
// nested nodes), with counts.
func (ir *IR) Kinds() map[NodeKind]int {
	census := map[NodeKind]int{}
	ir.WalkNodes(func(node map[string]json.RawMessage) {
		var kind string
		_ = json.Unmarshal(node["kind"], &kind)
		census[NodeKind(kind)]++
	})
	return census
}

func supportedVersionList() string {
	vs := make([]string, 0, len(SupportedVersions))
	for v := range SupportedVersions {
		vs = append(vs, v)
	}
	return strings.Join(vs, ", ")
}
