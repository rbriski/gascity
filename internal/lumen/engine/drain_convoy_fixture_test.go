package engine_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// Convoy-drain input-set binding — the ENGINE-level pins for the P0 sling slice.
//
// The slice adds ZERO engine/reducer/IR code: it seeds a canonical member-id array as
// a run input on the sling seam and lets the already-landed `for-each over:
// input.<field>` member arm (INS) fan a `run <impl>` per id (FBR). These fixtures pin
// the two engine-observable contracts the sling relies on:
//   - input.members DRIVES the fan: one sub-graph per id, each id bound as `item`, with
//     per-member and aggregate settles (the sealed arms, exercised over a members array).
//   - the freeze-ids determinism property on engine.InputHash: the run identity pins
//     MEMBERSHIP (the id SET, in canonical order), not mutable member fields — so the
//     hash is stable across identical membership, sensitive to add/remove, and sensitive
//     to ORDER (which is why the sling MUST sort; that resolver-side pin lives in
//     cmd/gc/lumen_convoy_test.go).

// drainMembersInput builds the immutable run-input shape the sling seeds: a `members`
// field holding the []any id array the member arm fans.
func drainMembersInput(ids ...string) map[string]any {
	arr := make([]any, len(ids))
	for i, id := range ids {
		arr[i] = id
	}
	return map[string]any{"members": arr}
}

// drainConvoyExecDoc is the build-from-convoy shape with an INLINE (exec) impl body, so
// Run seals in one pass with no host: `for-each member input.members { run impl given
// {item: <binder>} }`, impl = one exec rendering {{item}}.
func drainConvoyExecDoc(t *testing.T) string {
	t.Helper()
	return bundleDoc(
		arrField("members"),
		forEachNode(nil, "item", "continue", memberOver("members"),
			runNodeRawEnv("do", nil, "impl", `[`+envField("item", "item")+`]`)),
		subDoc("impl", strField("item"),
			execNode("work", `echo "did {{ item }}"`, nil)),
	)
}

// TestForEachMemberInputConvoyDrainInlineFans pins the marquee: a for-each over
// input.members fans one `run impl` per id, binding each id as `item` INSIDE the minted
// sub-graph, with per-member aggregates and the fan aggregate all settling pass. This is
// exactly what the sling produces after seeding the resolved member-id array — the engine
// is unchanged; this proves input.members drives the drain.
func TestForEachMemberInputConvoyDrainInlineFans(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, drainConvoyExecDoc(t))

	// A canonical (already-sorted) 3-member id array, as the sling would seed it.
	res, err := engine.Run(ctx, store, doc, drainMembersInput("gc-a", "gc-b", "gc-c"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}

	// One sub-graph per id; each renders its OWN id bound as `item` (no cross-member leak).
	for i, want := range []string{"did gc-a", "did gc-b", "did gc-c"} {
		key := "fan/" + itoa(i) + "/work"
		if got := res.NodeOutputs[key]; got != want {
			t.Errorf("member %d output %s = %q, want %q (id bound as item in its sub-graph)", i, key, got, want)
		}
	}

	// Per-member aggregates + the fan aggregate all settle pass.
	settled := settledOutcomeByID(t, res.Events)
	for i := 0; i < 3; i++ {
		if got := settled["fan/"+itoa(i)]; got != engine.OutcomePass {
			t.Errorf("member aggregate fan/%d = %q, want pass", i, got)
		}
	}
	if settled["fan"] != engine.OutcomePass {
		t.Errorf("fan aggregate = %q, want pass", settled["fan"])
	}

	// node.activated fires per member sub-do plus the structural nodes — at minimum one
	// per fanned member is present (the fan is real, not vacuous).
	if n := countActivated(res.Events); n < 3 {
		t.Errorf("node.activated count = %d, want >= 3 (one fanned member sub-graph per id)", n)
	}

	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestForEachMemberInputConvoyDrainPoolFans is the pool/do twin driven through Advance:
// the impl body is a `do`, so each fanned member dispatches ONE real work bead through
// the seam — the shape the dolt e2e exercises. All three dispatch concurrently in one
// pass; on settle the member + fan aggregates seal pass.
func TestForEachMemberInputConvoyDrainPoolFans(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	streamID := "gcg-drain-convoy-pool"
	doc := decodeIR(t, bundleDoc(
		arrField("members"),
		forEachNode(nil, "item", "continue", memberOver("members"),
			runNodeRawEnv("do", nil, "impl", `[`+envField("item", "item")+`]`)),
		subDoc("impl", strField("item"),
			doNode("work", "do {{ item }}", nil)),
	))
	input := drainMembersInput("gc-a", "gc-b", "gc-c")

	r1, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if r1.Sealed || !r1.Parked {
		t.Fatalf("advance 1 = %+v, want Parked (three member sub-dos dispatched)", r1)
	}
	if fake.dispatchCount() != 3 || len(r1.InFlight) != 3 {
		t.Fatalf("dispatch=%d inFlight=%d, want 3 concurrent member sub-dos in one pass", fake.dispatchCount(), len(r1.InFlight))
	}

	// One work bead per member, each single-dispatched at its '/'-bearing activation.
	fake.settleAct(t, "fan/0/work:0", engine.OutcomePass, "ok0")
	fake.settleAct(t, "fan/1/work:0", engine.OutcomePass, "ok1")
	fake.settleAct(t, "fan/2/work:0", engine.OutcomePass, "ok2")
	r2, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r2.Sealed {
		t.Fatalf("advance 2 = %+v err %v, want Sealed", r2, err)
	}
	if r2.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r2.Run.Outcome)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["fan/0"] != engine.OutcomePass || settled["fan/1"] != engine.OutcomePass ||
		settled["fan/2"] != engine.OutcomePass || settled["fan"] != engine.OutcomePass {
		t.Errorf("settles = {m0:%q m1:%q m2:%q fan:%q}, want all pass",
			settled["fan/0"], settled["fan/1"], settled["fan/2"], settled["fan"])
	}
}

// TestForEachMemberInputConvoyEmptyVacuousPass pins R-EMPTY at the engine: an empty
// members array (what the sling seeds for a convoy with zero live members) fans NOTHING
// and settles a vacuous PASS — a legal empty drain, not a stall.
func TestForEachMemberInputConvoyEmptyVacuousPass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, drainConvoyExecDoc(t))

	res, err := engine.Run(ctx, store, doc, drainMembersInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("empty drain outcome = %q, want pass (vacuous)", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["fan"] != engine.OutcomePass {
		t.Errorf("fan = %q, want pass (vacuous)", settled["fan"])
	}
	if _, ok := settled["fan/0"]; ok {
		t.Errorf("fan/0 settled, want ZERO members (empty membership fans nothing)")
	}
}

// TestInputConvoyFreezeIDsInputHash is the load-bearing determinism pin (design §3.3):
// engine.InputHash over the seeded members array is
//   - STABLE across repeated identical membership (re-enqueue reproducibility),
//   - SENSITIVE to membership (adding/removing an id changes the hash), and
//   - SENSITIVE to ORDER (reordering changes the hash — which is exactly why the sling
//     freezes a SORTED array; the resolver-side sort pin lives in cmd/gc).
//
// Because the run input carries IDS ONLY (never member bead snapshots), a member's
// mutable fields cannot enter the hash by construction — the freeze-ids property. The
// end-to-end mutate-a-member-then-re-resolve proof lives in cmd/gc/lumen_convoy_test.go.
func TestInputConvoyFreezeIDsInputHash(t *testing.T) {
	base := engine.InputHash(drainMembersInput("gc-a", "gc-b", "gc-c"))
	if base == "" {
		t.Fatal("InputHash of a non-empty members array is empty")
	}

	// Stable across repeated identical membership.
	if again := engine.InputHash(drainMembersInput("gc-a", "gc-b", "gc-c")); again != base {
		t.Errorf("InputHash not stable across identical membership: %q vs %q", again, base)
	}

	// Sensitive to membership: add and remove both change the hash.
	if added := engine.InputHash(drainMembersInput("gc-a", "gc-b", "gc-c", "gc-d")); added == base {
		t.Errorf("InputHash unchanged after ADDING a member (membership must pin the hash)")
	}
	if removed := engine.InputHash(drainMembersInput("gc-a", "gc-b")); removed == base {
		t.Errorf("InputHash unchanged after REMOVING a member (membership must pin the hash)")
	}

	// Sensitive to order: an UNSORTED permutation hashes differently, so the sling's
	// canonical sort is what makes the hash stable across membership-equal re-enqueues.
	if reordered := engine.InputHash(drainMembersInput("gc-c", "gc-b", "gc-a")); reordered == base {
		t.Errorf("InputHash unchanged under reordering — the array order leaks into the hash, so the sling MUST sort")
	}
}

// itoa is a tiny int→string helper local to these fixtures (avoids a strconv import for
// the two single-digit member indices).
func itoa(i int) string { return string(rune('0' + i)) }

// countActivated counts node.activated events — a lower bound on fanned members.
func countActivated(events []graphstore.StoredEvent) int {
	n := 0
	for _, e := range events {
		if e.Type == engine.EventNodeActivated {
			n++
		}
	}
	return n
}
