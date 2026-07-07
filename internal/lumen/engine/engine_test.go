package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// newStore opens a fresh journal store in a temp dir.
func newStore(t *testing.T) *graphstore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "test-city"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// decodeIR builds an *ir.IR from a JSON literal, exercising the real decode path.
func decodeIR(t *testing.T, jsonDoc string) *ir.IR {
	t.Helper()
	doc, err := ir.Decode([]byte(jsonDoc))
	if err != nil {
		t.Fatalf("decode IR: %v", err)
	}
	return doc
}

// execNode renders one exec node (echo-style body) at line ln, after the given deps.
func execNode(id, script string, after []string) string {
	afterJSON, _ := json.Marshal(after)
	scriptJSON, _ := json.Marshal(script)
	return `{
      "kind": "exec", "id": "` + id + `", "name": "` + id + `", "after": ` + string(afterJSON) + `,
      "origin": {"uri": "t", "line": 1, "col": 0},
      "interpreter": {"kind": "shell", "program": {"kind": "exec"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "body": {"raw": ` + string(scriptJSON) + `, "language": "bash", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "exitMap": {"pass": [0], "retryable": []}
    }`
}

// blockDoc wraps members in a top-level named block, as a bare statement body compiles to.
func blockDoc(name string, members ...string) string {
	return `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "` + name + `",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "block", "id": "block_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "members": [` + strings.Join(members, ",") + `]}
      ]
    }`
}

// eventTypes returns the journal event types in seq order.
func eventTypes(events []graphstore.StoredEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// settledIDs returns, in seq order, the (bare node id, outcome) of every
// outcome.settled event, deriving the node id from the activation key.
func settledIDs(t *testing.T, events []graphstore.StoredEvent) [][2]string {
	t.Helper()
	var out [][2]string
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Outcome    string `json:"outcome"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled payload: %v", err)
		}
		out = append(out, [2]string{engine.ActivationNodeID(p.Activation), p.Outcome})
	}
	return out
}

// nodeStatus reads a node's projected status from the Tier-A projection.
func nodeStatus(t *testing.T, store *graphstore.Store, id string) string {
	t.Helper()
	var status string
	err := store.DB().QueryRowContext(context.Background(),
		`SELECT status FROM nodes WHERE id = ?`, id).Scan(&status)
	if err != nil {
		t.Fatalf("read node %q status: %v", id, err)
	}
	return status
}

func closedOutcome(t *testing.T, events []graphstore.StoredEvent) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventRunClosed {
			continue
		}
		var p struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode run.closed payload: %v", err)
		}
		return p.Outcome
	}
	t.Fatalf("no run.closed event found")
	return ""
}

// TestRunHelloExecEndToEnd is the walking-skeleton proof: a one-exec linear
// formula runs end-to-end on the journal substrate. It asserts the run passes,
// the exec output is captured, the journal records run.started -> node.settled
// -> run.closed in order, the projection shows the node done, and the hash chain
// verifies.
func TestRunHelloExecEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("hello",
		execNode("greet", `echo "hi from lumen on gas city"`, nil),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Outcome != engine.OutcomePass {
		t.Errorf("outcome = %q, want %q", res.Outcome, engine.OutcomePass)
	}
	if got := res.NodeOutputs["greet"]; !strings.Contains(got, "hi from lumen on gas city") {
		t.Errorf("NodeOutputs[greet] = %q, want it to contain the greeting", got)
	}

	if got, want := eventTypes(res.Events), []string{
		engine.EventRunStarted, engine.EventNodeActivated, engine.EventOutcomeSettled, engine.EventRunClosed,
	}; !equalStrings(got, want) {
		t.Errorf("journal event order = %v, want %v", got, want)
	}

	if st := nodeStatus(t, store, "greet"); st != "done" {
		t.Errorf("projected node status = %q, want done", st)
	}

	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify(%q) = %v, want nil", res.StreamID, err)
	}
}

// TestRunTwoExecLinearOrdering proves ordering and output capture across two
// sequential exec steps (b after a).
func TestRunTwoExecLinearOrdering(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("twostep",
		execNode("a", `echo one`, nil),
		execNode("b", `echo two`, []string{"a"}),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Outcome != engine.OutcomePass {
		t.Errorf("outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["a"]; !strings.Contains(got, "one") {
		t.Errorf("NodeOutputs[a] = %q, want to contain 'one'", got)
	}
	if got := res.NodeOutputs["b"]; !strings.Contains(got, "two") {
		t.Errorf("NodeOutputs[b] = %q, want to contain 'two'", got)
	}

	settled := settledIDs(t, res.Events)
	want := [][2]string{{"a", "pass"}, {"b", "pass"}}
	if len(settled) != len(want) {
		t.Fatalf("settled = %v, want %v", settled, want)
	}
	for i := range want {
		if settled[i] != want[i] {
			t.Errorf("settled[%d] = %v, want %v (ordering not honored)", i, settled[i], want[i])
		}
	}

	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestRunExecFailsSettlesFailed proves a non-zero exit settles the step and the
// run as failed.
func TestRunExecFailsSettlesFailed(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("failing",
		execNode("boom", `exit 1`, nil),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
	if got := closedOutcome(t, res.Events); got != engine.OutcomeFailed {
		t.Errorf("run.closed outcome = %q, want failed", got)
	}
	settled := settledIDs(t, res.Events)
	if len(settled) != 1 || settled[0] != [2]string{"boom", "failed"} {
		t.Errorf("settled = %v, want [{boom failed}]", settled)
	}
	if st := nodeStatus(t, store, "boom"); st != "failed" {
		t.Errorf("projected node status = %q, want failed", st)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestRunUnsupportedNodeRefused proves a node kind outside the linear set is
// refused with ErrUnsupportedNode before any effect runs.
func TestRunUnsupportedNodeRefused(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	// A guard node (lowers to do/agent) is out of scope for the linear skeleton.
	doc := decodeIR(t, `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "guarded",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "channel", "id": "ch", "after": [], "origin": {"uri": "t", "line": 1, "col": 0}}
      ]
    }`)

	_, err := engine.Run(ctx, store, doc, nil)
	if err == nil {
		t.Fatal("expected ErrUnsupportedNode, got nil")
	}
	if !errors.Is(err, engine.ErrUnsupportedNode) {
		t.Errorf("err = %v, want ErrUnsupportedNode", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
