package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// recoverNode renders a recover(try/catch) node with id "rec": the `guarded` leaf runs;
// on FAILURE the `body` (catch) leaf runs with the error bound as `errorBinding`.
func recoverNode(after []string, guarded, body, errorBinding string) string {
	afterJSON := "[]"
	if len(after) > 0 {
		afterJSON = `["` + strings.Join(after, `","`) + `"]`
	}
	return `{"kind":"recover","id":"rec","name":"rec","after":` + afterJSON + `,` +
		`"origin":{"uri":"t","line":1,"col":0},` +
		`"guarded":` + guarded + `,"body":` + body + `,"errorBinding":"` + errorBinding + `"}`
}

// settleNodeReason renders a settle leaf carrying an authored outcome + reason (the
// guarded whose reason the catch's {{ error.reason }} binds).
func settleNodeReason(id, outcome, reason string) string {
	return `{"kind":"settle","id":"` + id + `","name":"` + id + `","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},"outcome":"` + outcome + `","reason":"` + reason + `"}`
}

// doNodeErrorTemplate renders a do whose prompt is `<pre>{{ base.field }}<post>` as a
// member-interp template (the compiled shape of an error-binding reference).
func doNodeErrorTemplate(id, pre, base, field, post string) string {
	return `{"kind":"do","id":"` + id + `","name":"` + id + `","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},"source":{"kind":"prompt"},` +
		`"interpreter":{"kind":"agent","mode":{"kind":"do"},"origin":{"uri":"t","line":1,"col":0}},` +
		`"body":{"raw":"` + pre + `{{ ` + base + `.` + field + ` }}` + post + `",` +
		`"template":{"parts":[` +
		`{"kind":"text","value":"` + pre + `"},` +
		`{"kind":"interp","expr":{"kind":"member","base":{"kind":"ref","name":"` + base + `"},"name":"` + field + `"}},` +
		`{"kind":"text","value":"` + post + `"}]},` +
		`"source":{"kind":"inline"},"templated":true,"language":"markdown","syntax":"bare","origin":{"uri":"t","line":1,"col":0}}}`
}

// TestRecoverGuardedPassTransparent proves a recover whose guarded passes settles
// transparently from it and does NOT run the catch body.
func TestRecoverGuardedPassTransparent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rc1",
		recoverNode(nil, execNode("g", `echo G`, nil), execNode("c", `echo C`, nil), "error"),
		execNode("after", `echo "r={{ rec }}"`, []string{"rec"}),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["c"]; got != "" {
		t.Errorf("catch output = %q, want empty (guarded passed, catch must not run)", got)
	}
	if got := res.NodeOutputs["rec"]; got != "G" {
		t.Errorf("recover output = %q, want G (transparent from guarded)", got)
	}
	if got := res.NodeOutputs["after"]; got != "r=G" {
		t.Errorf("downstream = %q, want r=G (guarded output plumbed)", got)
	}
}

// TestRecoverGuardedFailRunsCatch proves a failed guarded runs the catch and the
// recover settles from the catch (the error is handled).
func TestRecoverGuardedFailRunsCatch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rc2",
		recoverNode(nil, execNode("g", `exit 1`, nil), execNode("c", `echo recovered`, nil), "error"),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "g", engine.OutcomeFailed)
	assertSettled(t, settled, "c", engine.OutcomePass) // catch ran on the failure
	assertSettled(t, settled, "rec", engine.OutcomePass)
	if got := res.NodeOutputs["rec"]; got != "recovered" {
		t.Errorf("recover output = %q, want recovered (from the catch)", got)
	}
}

// TestRecoverCatchFailsReFails proves a catch that itself fails re-fails the recover.
func TestRecoverCatchFailsReFails(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rc3",
		recoverNode(nil, execNode("g", `exit 1`, nil), settleNode("c", "failed"), "error"),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "rec", engine.OutcomeFailed)
}

// TestRecoverGuardedCanceledTransparent pins the arch-review fix: a CANCELED guarded is
// NOT caught (canceling is not a recoverable failure) — the recover settles transparent
// canceled and the catch does NOT run.
func TestRecoverGuardedCanceledTransparent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rc4",
		recoverNode(nil, settleNode("g", "canceled"), execNode("c", `echo C`, nil), "error"),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "rec", engine.OutcomeCanceled)
	if got := res.NodeOutputs["c"]; got != "" {
		t.Errorf("catch output = %q, want empty (canceled is not caught)", got)
	}
}

// TestRecoverSkipCascade proves a recover gated on a failed dep settles skipped and runs
// NEITHER sub.
func TestRecoverSkipCascade(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rc5",
		execNode("gate", `exit 1`, nil),
		recoverNode([]string{"gate"}, execNode("g", `echo G`, nil), execNode("c", `echo C`, nil), "error"),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "rec", engine.OutcomeSkipped)
	if res.NodeOutputs["g"] != "" || res.NodeOutputs["c"] != "" {
		t.Errorf("subs ran under a skip-cascaded recover: g=%q c=%q", res.NodeOutputs["g"], res.NodeOutputs["c"])
	}
}

// TestRecoverDropRefoldByteIdentity pins DET for a recover (guarded fail + catch pass).
func TestRecoverDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rc6",
		recoverNode(nil, settleNodeReason("g", "failed", "boom"), execNode("c", `echo ok`, nil), "error"),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestRecoverLoweringRefusals pins the refused shapes.
func TestRecoverLoweringRefusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		nodes []string
	}{
		{"non-leaf guarded", []string{recoverNode(nil, scatterNode("s", nil, "continue", execNode("x", `echo 1`, nil)), execNode("c", `echo 1`, nil), "error")}},
		{"missing guarded", []string{`{"kind":"recover","id":"rec","name":"rec","after":[],"origin":{"uri":"t","line":1,"col":0},"body":` + execNode("c", `echo 1`, nil) + `,"errorBinding":"error"}`}},
		{"missing body", []string{`{"kind":"recover","id":"rec","name":"rec","after":[],"origin":{"uri":"t","line":1,"col":0},"guarded":` + execNode("g", `echo 1`, nil) + `,"errorBinding":"error"}`}},
		{"bad error binding", []string{recoverNode(nil, execNode("g", `echo 1`, nil), execNode("c", `echo 1`, nil), "err.x")}},
		{"nested in aggregate", []string{scatterNode("outer", nil, "continue", recoverNode(nil, execNode("g", `echo 1`, nil), execNode("c", `echo 1`, nil), "error"))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeIR(t, blockDoc("rcrf", tc.nodes...))
			if _, err := engine.Run(ctx, newStore(t), doc, nil); !errors.Is(err, engine.ErrUnsupportedNode) {
				t.Fatalf("run err = %v, want ErrUnsupportedNode", err)
			}
		})
	}
}

// --- Advance (pool) + the error binding ---

// TestAdvanceRecoverBindsErrorReason is the headline: a failed guarded settle carrying a
// reason binds {{ error.reason }} into the catch do's prompt, proving the error object +
// the evalValue member arm + the folded nodeState.Detail.
func TestAdvanceRecoverBindsErrorReason(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("arc",
		recoverNode(nil,
			settleNodeReason("charge", "failed", "the card was declined"),
			doNodeErrorTemplate("refund", "Reverse anything from ", "error", "reason", "."),
			"error"),
	))
	res, err := engine.Advance(ctx, store, doc, "gcg-run-rec", nil, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || len(res.InFlight) != 1 || res.InFlight[0].NodeID != "refund" {
		t.Fatalf("advance = %+v, want the catch do dispatched", res)
	}
	if fake.dispatches[0].Prompt != "Reverse anything from the card was declined." {
		t.Fatalf("catch prompt = %q, want the error.reason bound", fake.dispatches[0].Prompt)
	}
}

// TestAdvanceRecoverGuardedPassSealsNoCatch proves a passing guarded seals the recover
// WITHOUT dispatching the catch.
func TestAdvanceRecoverGuardedPassSealsNoCatch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("arcp",
		recoverNode(nil, settleNode("g", "pass"), doNode("c", "recover", nil), "error"),
	))
	res, err := engine.Advance(ctx, store, doc, "gcg-run-recp", nil, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Sealed {
		t.Fatalf("advance = %+v, want Sealed (guarded passed, no catch)", res)
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("dispatched %d beads, want 0 (catch must not run)", fake.dispatchCount())
	}
}
