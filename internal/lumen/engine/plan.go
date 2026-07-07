package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/lumen/exechost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// ErrUnsupportedNode is returned when the formula body contains a node kind the
// linear walking-skeleton executor does not implement (do/agent, channels,
// scatter, gather, dispatch, retry, run, async, …). It is a load-time-style
// refusal surfaced before any effect runs — the executor never silently skips a
// node it cannot honor.
var ErrUnsupportedNode = errors.New("lumen: unsupported node kind")

// step is one executable leaf of a flattened linear formula. block nodes are
// transparent and never become steps; their members flatten in place.
type step struct {
	kind  ir.NodeKind
	id    string
	after []string

	// exec fields.
	program   string
	script    string // body.raw template, before {{var}} interpolation
	passCodes []int  // exitMap.pass; nil means "0 is the only pass code"
	cwd       string
	env       []string

	// settle fields.
	outcome string

	// settle/lit/interp value evaluation; the raw node object, evaluated against
	// the live scope at run time.
	raw map[string]json.RawMessage
}

// flatten lowers a linear formula body to an ordered slice of executable leaf
// steps. block nodes are transparent containers (their members flatten in
// place, recursively); exec/settle/lit/interp become steps; any other kind is
// an ErrUnsupportedNode. The result is topologically ordered over each step's
// `after` edges, with source order breaking ties, so a linear formula executes
// in dependency order.
func flatten(nodes []ir.Node) ([]step, error) {
	var steps []step
	for i := range nodes {
		if err := flattenNode(nodes[i], &steps); err != nil {
			return nil, err
		}
	}
	return topoSort(steps)
}

func flattenNode(n ir.Node, out *[]step) error {
	switch n.Kind {
	case ir.NodeBlock:
		members, err := childNodes(n.Raw["members"])
		if err != nil {
			return fmt.Errorf("lumen: block %q: %w", n.ID, err)
		}
		for i := range members {
			if err := flattenNode(members[i], out); err != nil {
				return err
			}
		}
		return nil
	case ir.NodeExec:
		s, err := decodeExec(n)
		if err != nil {
			return err
		}
		*out = append(*out, s)
		return nil
	case ir.NodeSettle:
		*out = append(*out, decodeSettle(n))
		return nil
	case ir.NodeLit, ir.NodeInterp:
		*out = append(*out, step{kind: n.Kind, id: n.ID, after: n.After, raw: n.Raw})
		return nil
	default:
		return fmt.Errorf("%w: %q (node %q)", ErrUnsupportedNode, n.Kind, n.ID)
	}
}

func childNodes(raw json.RawMessage) ([]ir.Node, error) {
	if raw == nil {
		return nil, nil
	}
	var nodes []ir.Node
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("decoding members: %w", err)
	}
	return nodes, nil
}

// decodeExec lifts an exec node's interpreter/body/exitMap into a step. The
// interpreter program kind selects the shell; body.raw is the script template;
// exitMap.pass is the set of exit codes that settle pass.
func decodeExec(n ir.Node) (step, error) {
	s := step{kind: ir.NodeExec, id: n.ID, after: n.After, program: exechost.ProgramExec}

	if raw, ok := n.Raw["interpreter"]; ok {
		var interp struct {
			Program struct {
				Kind string `json:"kind"`
			} `json:"program"`
		}
		if err := json.Unmarshal(raw, &interp); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q interpreter: %w", n.ID, err)
		}
		if interp.Program.Kind != "" {
			s.program = interp.Program.Kind
		}
	}

	if raw, ok := n.Raw["body"]; ok {
		var body struct {
			Raw string `json:"raw"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q body: %w", n.ID, err)
		}
		s.script = body.Raw
	}

	if raw, ok := n.Raw["exitMap"]; ok {
		var em struct {
			Pass []int `json:"pass"`
		}
		if err := json.Unmarshal(raw, &em); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q exitMap: %w", n.ID, err)
		}
		s.passCodes = em.Pass
	}

	if raw, ok := n.Raw["cwd"]; ok {
		if err := json.Unmarshal(raw, &s.cwd); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q cwd: %w", n.ID, err)
		}
	}

	if raw, ok := n.Raw["env"]; ok {
		var envMap map[string]string
		if err := json.Unmarshal(raw, &envMap); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q env: %w", n.ID, err)
		}
		keys := make([]string, 0, len(envMap))
		for k := range envMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.env = append(s.env, k+"="+envMap[k])
		}
	}

	return s, nil
}

// decodeSettle lifts a settle node's outcome into a step; its value expression
// is evaluated later against the live scope.
func decodeSettle(n ir.Node) step {
	s := step{kind: ir.NodeSettle, id: n.ID, after: n.After, raw: n.Raw}
	if raw, ok := n.Raw["outcome"]; ok {
		_ = json.Unmarshal(raw, &s.outcome)
	}
	return s
}

// topoSort returns steps in a stable topological order over their `after` edges.
// Edges that reference an id outside the leaf set (e.g. a transparent block id)
// are ignored — for a linear formula those dependencies are already satisfied by
// source order. Ties are broken by source order for determinism; a cycle is a
// loud error.
func topoSort(steps []step) ([]step, error) {
	n := len(steps)
	idx := make(map[string]int, n)
	for i, s := range steps {
		if _, dup := idx[s.id]; dup {
			return nil, fmt.Errorf("lumen: duplicate step id %q", s.id)
		}
		idx[s.id] = i
	}

	indeg := make([]int, n)
	adj := make([][]int, n)
	for i, s := range steps {
		for _, dep := range s.after {
			j, ok := idx[dep]
			if !ok {
				continue // dependency outside the leaf set
			}
			adj[j] = append(adj[j], i)
			indeg[i]++
		}
	}

	done := make([]bool, n)
	order := make([]step, 0, n)
	for len(order) < n {
		picked := -1
		for i := 0; i < n; i++ {
			if !done[i] && indeg[i] == 0 {
				picked = i
				break
			}
		}
		if picked == -1 {
			return nil, fmt.Errorf("lumen: dependency cycle among steps")
		}
		done[picked] = true
		order = append(order, steps[picked])
		for _, m := range adj[picked] {
			indeg[m]--
		}
	}
	return order, nil
}

// evalValue renders an IR value expression to a string against scope. It
// handles the expression shapes the linear scope needs: a literal
// ({"kind":"literal","value":X}), a ref ({"kind":"ref","name":N}), and the
// emitted interp wrapper ({"kind":"interp","expr":{...}}, seen in the
// linear-do-after / path-interpolation goldens), which unwraps to its inner
// expression; a bare scalar is rendered directly. Any other expression kind is a
// typed error.
func evalValue(raw json.RawMessage, scope map[string]string) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var probe struct {
		Kind  string          `json:"kind"`
		Value json.RawMessage `json:"value"`
		Name  string          `json:"name"`
		Expr  json.RawMessage `json:"expr"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Not an object — treat as a bare scalar (string/number/bool).
		return scalarToString(raw), nil
	}
	switch probe.Kind {
	case "":
		return scalarToString(raw), nil
	case "literal":
		return scalarToString(probe.Value), nil
	case "ref":
		return scope[probe.Name], nil
	case "interp":
		return evalValue(probe.Expr, scope)
	default:
		return "", fmt.Errorf("lumen: unsupported value expression kind %q", probe.Kind)
	}
}

// evalInterp renders an interp node to a string. It supports a direct value
// expression, a template-parts form (text parts plus embedded expressions), and
// a raw-body form with {{var}} interpolation. An unrecognized shape is a typed
// error rather than a silent empty string.
func evalInterp(raw map[string]json.RawMessage, scope map[string]string) (string, error) {
	if v, ok := raw["value"]; ok {
		return evalValue(v, scope)
	}

	partsRaw := raw["parts"]
	if partsRaw == nil {
		if tmpl, ok := raw["template"]; ok {
			var t struct {
				Parts json.RawMessage `json:"parts"`
			}
			if err := json.Unmarshal(tmpl, &t); err != nil {
				return "", fmt.Errorf("lumen: interp template: %w", err)
			}
			partsRaw = t.Parts
		}
	}
	if partsRaw != nil {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(partsRaw, &parts); err != nil {
			return "", fmt.Errorf("lumen: interp parts: %w", err)
		}
		var sb strings.Builder
		for _, p := range parts {
			var kind string
			_ = json.Unmarshal(p["kind"], &kind)
			if kind == "text" {
				sb.WriteString(scalarToString(p["value"]))
				continue
			}
			expr, err := json.Marshal(p)
			if err != nil {
				return "", err
			}
			v, err := evalValue(expr, scope)
			if err != nil {
				return "", err
			}
			sb.WriteString(v)
		}
		return sb.String(), nil
	}

	if b, ok := raw["body"]; ok {
		var body struct {
			Raw string `json:"raw"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			return "", fmt.Errorf("lumen: interp body: %w", err)
		}
		return interpolate(body.Raw, scope), nil
	}

	return "", fmt.Errorf("lumen: interp node: unrecognized shape")
}

// scalarToString renders a JSON scalar as a plain Go string: a JSON string is
// unquoted; anything else (number, bool, object) is rendered from its raw form.
func scalarToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}
