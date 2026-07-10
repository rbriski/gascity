package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// dispatchExec renders a dispatch "d" over subject ref `policy`, with exec arms
// (arm i's body id is d_arm<i>). Each arm is [matchValue, thenScript].
func dispatchExec(arms ...[2]string) string {
	var armJSON []string
	for i, arm := range arms {
		bodyID := "d_arm" + string(rune('0'+i))
		armJSON = append(armJSON,
			`{"match":{"kind":"literal","value":"`+arm[0]+`"},"body":`+execNode(bodyID, arm[1], nil)+`}`)
	}
	return `{"kind":"dispatch","id":"d","name":"d","after":[],` +
		`"subject":{"kind":"ref","name":"policy"},"arms":[` + strings.Join(armJSON, ",") + `]}`
}

// TestDispatchPicksMatchingArm proves a dispatch runs the arm whose match equals the
// subject and settles transparently from it (value plumbs to a downstream {{d}}).
func TestDispatchPicksMatchingArm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("dp",
		dispatchExec([2]string{"separate", `echo "sep"`}, [2]string{"shared", `echo "shr"`}),
		execNode("done", `echo "picked: {{ d }}"`, []string{"d"}),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "shared"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	// The matching arm (index 1, "shared") ran; the other did not.
	if got := res.NodeOutputs["d_arm1"]; got != "shr" {
		t.Errorf("arm1 output = %q, want %q (the shared arm ran)", got, "shr")
	}
	if got := res.NodeOutputs["d_arm0"]; got != "" {
		t.Errorf("arm0 output = %q, want empty (the separate arm must not run)", got)
	}
	if got := res.NodeOutputs["d"]; got != "shr" {
		t.Errorf("dispatch output = %q, want %q (transparent from the chosen arm)", got, "shr")
	}
	if got := res.NodeOutputs["done"]; got != "picked: shr" {
		t.Errorf("downstream = %q, want %q (dispatch output plumbed)", got, "picked: shr")
	}
}

// TestDispatchNoMatchIsPassNoOp proves a dispatch with no matching arm settles PASS
// with an empty result and does NOT skip-cascade its dependents.
func TestDispatchNoMatchIsPassNoOp(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("dn",
		dispatchExec([2]string{"separate", `echo "sep"`}, [2]string{"shared", `echo "shr"`}),
		execNode("done", `echo "after: {{ d }}"`, []string{"d"}),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "neither"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass (no match is a no-op)", res.Outcome)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "d", engine.OutcomePass)
	assertSettled(t, settled, "done", engine.OutcomePass) // downstream ran (not skip-cascaded)
	if got := res.NodeOutputs["done"]; got != "after: " {
		t.Errorf("downstream = %q, want %q (no arm ran, dispatch output empty)", got, "after: ")
	}
}

// TestDispatchDropRefoldByteIdentity pins DET for a dispatch (matched + unmatched).
func TestDispatchDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []string{"shared", "neither"} {
		store := newStore(t)
		doc := decodeIR(t, blockDoc("dd",
			dispatchExec([2]string{"separate", `echo a`}, [2]string{"shared", `echo b`}),
			execNode("done", `echo "d {{ d }}"`, []string{"d"}),
		))
		res, err := engine.Run(ctx, store, doc, map[string]any{"policy": policy})
		if err != nil {
			t.Fatalf("run(policy=%s): %v", policy, err)
		}
		assertProjectionEqualsRefold(t, store, res.StreamID)
	}
}

// TestDispatchNumericSubjectMatches pins the red-team fix: a numeric subject and a
// numeric match literal that are the SAME scalar must match, even when spelled
// differently (5.0 vs 5) — both sides canonicalize to "5".
func TestDispatchNumericSubjectMatches(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	dispatch := `{"kind":"dispatch","id":"d","name":"d","after":[],` +
		`"subject":{"kind":"ref","name":"policy"},` +
		`"arms":[{"match":{"kind":"literal","value":5.0},"body":` + execNode("darm", `echo matched`, nil) + `}]}`
	doc := decodeIR(t, blockDoc("dnum", dispatch, execNode("done", `echo "d={{ d }}"`, []string{"d"})))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": 5.0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["d"]; got != "matched" {
		t.Errorf("dispatch output = %q, want %q (numeric subject 5.0 must match numeric arm 5.0)", got, "matched")
	}
}

// TestAdvanceDispatchDoArmParks proves a dispatch whose chosen arm is a do
// materializes that arm as pool work and parks.
func TestAdvanceDispatchDoArmParks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	dispatch := `{"kind":"dispatch","id":"d","name":"d","after":[],` +
		`"subject":{"kind":"ref","name":"policy"},"arms":[` +
		`{"match":{"kind":"literal","value":"go"},"body":` + doNode("darm", "do the gated work", nil) + `}]}`
	doc := decodeIR(t, blockDoc("add", dispatch))
	res, err := engine.Advance(ctx, store, doc, "gcg-run-dispatchdo", map[string]any{"policy": "go"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || !res.Parked {
		t.Fatalf("advance = %+v, want Parked (the do arm is dispatched, awaited)", res)
	}
	if len(res.InFlight) != 1 || res.InFlight[0].NodeID != "darm" {
		t.Fatalf("InFlight = %+v, want the darm do materialized", res.InFlight)
	}
}
