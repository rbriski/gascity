package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// --- dispatch-at-depth (DAD) behavioral fixtures ----------------------------
//
// The corpus marquee: a dispatch that lives INSIDE a run sub-formula's namespace,
// reached via STATIC run inlines — gascity-port's `implement: dispatch drain_policy {
// "separate": run …, "same-session": run … }` under a two-hop `continue-chain/
// continue-chain/` prefix. Pre-DAD the dispatch prefix fence refused this outright.
// DAD deletes the fence and makes matchingArm's subject evaluation namespace-aware
// (scopeFor(u.ns, scope)), so the deep dispatch selects its arm off the sub-scope and
// the matched arm mints its whole sub-graph at the deep-qualified coordinates.

// dadHopEnv is the marquee run-hop environment: it threads drain_policy and target from
// the parent scope into the next namespace by bare ref (env-bound at EVERY hop).
func dadHopEnv() string {
	return `[` + darEnvRef("drain_policy", "drain_policy") + `,` + darEnvRef("target", "target") + `]`
}

// dadChainWithSubs wraps leafNodes — which lower at ns "continue-chain/continue-chain/" — in the
// two-hop static run chain (main → run continue-chain → midChain → run continue-chain →
// leafChain), threading drain_policy + target (the chain's uniform sub-inputs) through each hop
// by bare ref, and bundles laneSubs (the drain-lane sub-formula JSON) alongside the chain
// formulas.
func dadChainWithSubs(leafNodes, laneSubs string) string {
	return bundleDoc(dadTwoInputFields,
		runNodeRawEnv("continue-chain", nil, "midChain", dadHopEnv()),
		subDoc("midChain", dadTwoInputFields, runNodeRawEnv("continue-chain", nil, "leafChain", dadHopEnv()))+","+
			subDoc("leafChain", dadTwoInputFields, leafNodes)+","+laneSubs)
}

// dadChainNodes is dadChainWithSubs with the default two EXEC drain lanes (drainSeparate/
// drainShared, accept reviewer + target).
func dadChainNodes(leafNodes string) string {
	return dadChainWithSubs(leafNodes,
		darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared"))
}

// dadChainDoc assembles the run→run→dispatch marquee: the leaf holds a dispatch (over the
// sub-input drain_policy) whose arms are the given raw arm JSONs, lowered at ns
// "continue-chain/continue-chain/".
func dadChainDoc(arms ...string) string {
	return dadChainNodes(darDispatch("drain_policy", arms...))
}

// dadBase is the deep dispatch namespace prefix the two-hop chain lowers the leaf into.
const dadBase = "continue-chain/continue-chain/"

// dadTwoInputFields is the uniform chain sub-input block (drain_policy + target, both required),
// threaded env-bound through every hop.
const dadTwoInputFields = `{"name":"drain_policy","type":{"kind":"atomic","name":"string"},"required":true,"body":false},` +
	`{"name":"target","type":{"kind":"atomic","name":"string"},"required":true,"body":false}`

// TestDispatchAtDepthMarqueeInlineMintsChosen (§2.1 marquee, inline) proves the four-level
// build-from-requirements shape at ns "continue-chain/continue-chain/": a dispatch reached
// through two static run hops selects its matched RUN arm off the env-bound sub-scope and
// mints ONLY that arm's sub-graph at the deep-qualified coordinates; the unchosen arm mints
// NOTHING. This is the wall DAD tears down (pre-DAD the fence refused the deep dispatch).
func TestDispatchAtDepthMarqueeInlineMintsChosen(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, dadChainDoc(
		darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")),
		darRunArm("same-session", "sharedLanes", "drainShared", darLaneEnv("shared"))))
	res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": "separate", "target": "release-7"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs[dadBase+"lanes/drain"]; got != "item=fanout target=release-7" {
		t.Errorf("deep chosen arm sub = %q, want %q", got, "item=fanout target=release-7")
	}
	if got := res.NodeOutputs[dadBase+"lanes"]; got != "item=fanout target=release-7" {
		t.Errorf("deep arm aggregate = %q, want the transparent sub output", got)
	}
	if got := res.NodeOutputs[dadBase+"d"]; got != "item=fanout target=release-7" {
		t.Errorf("deep dispatch output = %q, want the transparent arm output", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	for _, id := range []string{dadBase + "lanes/drain", dadBase + "lanes", dadBase + "d"} {
		if settled[id] != engine.OutcomePass {
			t.Errorf("%s settle = %q, want pass", id, settled[id])
		}
	}
	// The unchosen arm minted NOTHING at depth.
	if _, ok := settled[dadBase+"sharedLanes"]; ok {
		t.Errorf("unchosen deep arm %ssharedLanes settled %q, want ZERO activations", dadBase, settled[dadBase+"sharedLanes"])
	}
	if got := res.NodeOutputs[dadBase+"sharedLanes"]; got != "" {
		t.Errorf("unchosen deep arm output = %q, want empty (never minted)", got)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestDispatchAtDepthMarqueeOtherValueMintsOtherArm (§2.1 both-values) proves the SAME deep
// doc selects the OTHER arm when the env-bound subject differs: drain_policy="same-session"
// mints the deep sharedLanes arm and leaves lanes zero-activation.
func TestDispatchAtDepthMarqueeOtherValueMintsOtherArm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, dadChainDoc(
		darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")),
		darRunArm("same-session", "sharedLanes", "drainShared", darLaneEnv("shared"))))
	res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": "same-session", "target": "rel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs[dadBase+"sharedLanes/drain"]; got != "item=shared target=rel" {
		t.Errorf("deep chosen arm sub = %q, want %q", got, "item=shared target=rel")
	}
	if got := res.NodeOutputs[dadBase+"d"]; got != "item=shared target=rel" {
		t.Errorf("deep dispatch output = %q, want the shared arm output", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if _, ok := settled[dadBase+"lanes"]; ok {
		t.Errorf("unchosen deep arm %slanes settled, want ZERO activations", dadBase)
	}
}

// dadDispatchSubject renders a dispatch (id "d") over an arbitrary raw subject JSON — the
// escape hatch for member / interp-parts subjects that darDispatch (bare ref only) can't build.
func dadDispatchSubject(subjectJSON string, arms ...string) string {
	return `{"kind":"dispatch","id":"d","name":"d","after":[],"subject":` + subjectJSON + `,"arms":[` + strings.Join(arms, ",") + `]}`
}

// dadTwoRunArms is the marquee's two-run-arm set (separate → lanes, same-session → sharedLanes).
func dadTwoRunArms() []string {
	return []string{
		darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")),
		darRunArm("same-session", "sharedLanes", "drainShared", darLaneEnv("shared")),
	}
}

// TestDispatchAtDepthSeededDefaultMatches (§2.2 case a / §0.6a) proves the depth-seeded-default
// shape: leafChain declares `drain_policy: string = "separate"` and it is NOT env-bound at the
// last hop, so runInputLayer SEEDS the default at the dispatch namespace and the subject matches
// the "separate" arm. The ROOT case now seeds an omitted default too (ga-ospbql,
// TestDispatchRootDefaultSeededMatchesAtRoot) — this depth shape is unaffected, and the bound-""
// chain (case c) flipped to a match transitively.
func TestDispatchAtDepthSeededDefaultMatches(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	targetHop := `[` + darEnvRef("target", "target") + `]`
	defaultedDP := `{"name":"drain_policy","type":{"kind":"atomic","name":"string"},"required":false,"default":"separate","body":false}`
	doc := decodeIR(t, bundleDoc(
		strField("target"),
		runNodeRawEnv("continue-chain", nil, "midChain", targetHop),
		subDoc("midChain", strField("target"),
			runNodeRawEnv("continue-chain", nil, "leafChain", targetHop))+","+
			subDoc("leafChain", defaultedDP+","+strField("target"),
				darDispatch("drain_policy", dadTwoRunArms()...))+","+
			darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs[dadBase+"lanes/drain"]; got != "item=fanout target=t" {
		t.Errorf("seeded-default deep chosen arm = %q, want item=fanout target=t (the default seeded the subject)", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled[dadBase+"d"] != engine.OutcomePass || settled[dadBase+"lanes"] != engine.OutcomePass {
		t.Errorf("settles = {d:%q lanes:%q}, want both pass", settled[dadBase+"d"], settled[dadBase+"lanes"])
	}
	if _, ok := settled[dadBase+"sharedLanes"]; ok {
		t.Errorf("unchosen deep arm sharedLanes settled, want ZERO activations")
	}
}

// TestDispatchRootDefaultSeededMatchesAtRoot (§2.2 case b / §0.6b, ga-ospbql) pins the FLIPPED
// ROOT behavior: an omitted subject input with a declared default is now SEEDED at genesis
// (resolveDeclaredInput lands the default in d.input, baseScope flattens it), so the subject
// evaluates "separate" → the run arm MATCHES and mints. Pre-INS this was a no-match no-op; the
// bound-"" chain (case c, TestDispatchAtDepthBoundChainSeededMatches) flipped transitively.
// (The DAR twin is TestDispatchRunArmRootDefaultGotcha.)
func TestDispatchRootDefaultSeededMatchesAtRoot(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	defaultedPolicy := `{"name":"policy","type":{"kind":"atomic","name":"string"},"required":false,"body":false,"default":"separate"}`
	doc := decodeIR(t, bundleDoc(
		defaultedPolicy+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout"))),
		darLaneExecSub("drainSeparate")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"target": "t"}) // policy omitted → seeded "separate"
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["d"] != engine.OutcomePass {
		t.Errorf("root dispatch d = %q, want pass (transparent from the chosen arm)", settled["d"])
	}
	if settled["sepLane"] != engine.OutcomePass {
		t.Errorf("arm sepLane = %q, want pass (omitted default seeded at root → subject 'separate' matched)", settled["sepLane"])
	}
	if got := res.NodeOutputs["sepLane/drain"]; got != "item=fanout target=t" {
		t.Errorf("chosen arm drain = %q, want item=fanout target=t (root default seeded the subject)", got)
	}
}

// TestDispatchAtDepthBoundChainSeededMatches (§2.2 case c / §0.6c — THE CORPUS TOPOLOGY) pins the
// ga-ospbql FLIP of the bound-"" chain: the root input drain_policy is declared with a default but
// OMITTED; every hop binds drain_policy <- ref drain_policy. Genesis now SEEDS the root default
// "separate" into d.input/baseScope, so the FIRST hop binds bound=true "separate" and the value
// propagates through every hop to the deep dispatch namespace → the "separate" arm MATCHES deep,
// mints its sub-graph, and the deep {{d}} consumer renders the chosen arm's output. Pre-INS the
// root omission rendered "" and the deep dispatch was a no-match no-op. This asserts the deep
// MATCH is specifically on the STRING "separate" (the "fanout" lane output, not the "shared" one).
func TestDispatchAtDepthBoundChainSeededMatches(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	defaultedDP := `{"name":"drain_policy","type":{"kind":"atomic","name":"string"},"required":false,"default":"separate","body":false}`
	// leafChain also declares drain_policy defaulted — but it arrives BOUND (to the seeded
	// "separate") from the hop, so the default is skipped and the seeded value is what matches.
	doc := decodeIR(t, bundleDoc(
		defaultedDP+","+strField("target"),
		runNodeRawEnv("continue-chain", nil, "midChain", dadHopEnv()),
		subDoc("midChain", strField("drain_policy")+","+strField("target"),
			runNodeRawEnv("continue-chain", nil, "leafChain", dadHopEnv()))+","+
			subDoc("leafChain", defaultedDP+","+strField("target"),
				darDispatch("drain_policy", dadTwoRunArms()...)+","+
					execNode("after", `echo "after: {{ d }}"`, []string{"d"}))+","+
			darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"target": "t"}) // drain_policy omitted → seeded "separate"
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled[dadBase+"d"] != engine.OutcomePass {
		t.Errorf("deep dispatch d = %q, want pass (transparent from the chosen arm)", settled[dadBase+"d"])
	}
	if settled[dadBase+"lanes"] != engine.OutcomePass {
		t.Errorf("deep arm lanes = %q, want pass (seeded 'separate' propagated through the bound chain → match)", settled[dadBase+"lanes"])
	}
	// Deep MATCH is on the STRING "separate": the "separate" arm (lanes, reviewer "fanout") ran,
	// the "same-session" arm (sharedLanes, reviewer "shared") did NOT.
	if got := res.NodeOutputs[dadBase+"lanes/drain"]; got != "item=fanout target=t" {
		t.Errorf("deep chosen arm drain = %q, want item=fanout target=t (the 'separate' arm was chosen)", got)
	}
	if _, ok := settled[dadBase+"sharedLanes"]; ok {
		t.Errorf("unchosen 'same-session' arm sharedLanes settled, want ZERO activations")
	}
	// The deep {{d}} consumer renders the chosen arm's transparent output (transitive seed match).
	if got := res.NodeOutputs[dadBase+"after"]; got != "after: item=fanout target=t" {
		t.Errorf("deep after = %q, want %q ({{d}} carries the chosen arm output)", got, "after: item=fanout target=t")
	}
}

// TestDispatchAtDepthLeafSiblingSubject (§2.2d, the record() read-set — leaf sibling) proves a
// deep dispatch whose subject reads a settled LEAF sibling's output selects the matching arm:
// the sub-scope overlay exposes the direct-child leaf output at its bare id, exactly as
// record() shadows baseScope at root.
func TestDispatchAtDepthLeafSiblingSubject(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	leaf := execNode("pick", `printf separate`, nil) + "," +
		dadDispatchSubject(`{"kind":"ref","name":"pick"}`,
			darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")),
			darRunArm("same-session", "sharedLanes", "drainShared", darLaneEnv("shared")))
	doc := decodeIR(t, dadChainNodes(leaf))
	res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": "ignored", "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs[dadBase+"lanes/drain"]; got != "item=fanout target=t" {
		t.Errorf("leaf-sibling-subject deep arm = %q, want item=fanout target=t (subject read the sibling `pick`)", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if _, ok := settled[dadBase+"sharedLanes"]; ok {
		t.Errorf("unchosen deep arm sharedLanes settled, want ZERO activations")
	}
}

// TestDispatchAtDepthRunSiblingSubject (§2.2d, the record() read-set — run sibling) proves a
// deep dispatch whose subject reads a settled transparent RUN sibling's output selects the
// matching arm: the run sibling records its transparent output into the sub-scope at its bare
// id, and the dispatch reads it.
func TestDispatchAtDepthRunSiblingSubject(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// `src` runs a picker sub-formula whose exec emits "same-session"; the dispatch reads src.
	leaf := runNodeRawEnv("src", nil, "picker", `[]`) + "," +
		dadDispatchSubject(`{"kind":"ref","name":"src"}`,
			darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")),
			darRunArm("same-session", "sharedLanes", "drainShared", darLaneEnv("shared")))
	picker := subDoc("picker", "", execNode("emit", `printf same-session`, nil))
	doc := decodeIR(t, bundleDoc(
		strField("drain_policy")+","+strField("target"),
		runNodeRawEnv("continue-chain", nil, "midChain", dadHopEnv()),
		subDoc("midChain", strField("drain_policy")+","+strField("target"),
			runNodeRawEnv("continue-chain", nil, "leafChain", dadHopEnv()))+","+
			subDoc("leafChain", dadTwoInputFields, leaf)+","+
			darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")+","+picker))
	res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": "ignored", "target": "rel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs[dadBase+"sharedLanes/drain"]; got != "item=shared target=rel" {
		t.Errorf("run-sibling-subject deep arm = %q, want item=shared target=rel (subject read the run sibling `src`)", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if _, ok := settled[dadBase+"lanes"]; ok {
		t.Errorf("unchosen deep arm lanes settled, want ZERO activations")
	}
}

// TestDispatchAggregateSubjectInvisibleRootAndDepth (§2.2e) pins the aggregate-invisibility
// divergence: a dispatch whose subject reads a sibling SCATTER AGGREGATE resolves "" (aggregates
// are nodeOutputs-only, never record()-ed into the render scope), so the dispatch no-match
// no-ops — at ROOT and at DEPTH identically. This is a deliberate divergence from a GUARD cond,
// which (via condScope's fold overlay) DOES see the aggregate.
func TestDispatchAggregateSubjectInvisibleRootAndDepth(t *testing.T) {
	ctx := context.Background()
	// A for-each fan over a 1-element array; the dispatch subject reads the fan aggregate,
	// gated after it. Its output is invisible → "" → no-match.
	fan := forEachNode(nil, "item", "continue", refOver("items"),
		execNode("m", `printf pass`, nil))
	dispatch := dadDispatchSubject(`{"kind":"ref","name":"fan","field":""}`,
		darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")))
	// The subject is a bare ref to the aggregate id `fan`; gate the dispatch after fan.
	dispatchGated := strings.Replace(dispatch, `"after":[]`, `"after":["fan"]`, 1)

	t.Run("root", func(t *testing.T) {
		store := newStore(t)
		doc := decodeIR(t, bundleDoc(
			arrField("items"),
			fan+","+dispatchGated,
			darLaneExecSub("drainSeparate")))
		res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"x"}})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		settled := settledOutcomeByID(t, res.Events)
		if settled["d"] != engine.OutcomePass {
			t.Errorf("root dispatch d = %q, want pass (aggregate subject invisible → no-match)", settled["d"])
		}
		if _, ok := settled["lanes"]; ok {
			t.Errorf("arm lanes minted at root, want ZERO (aggregate subject read \"\")")
		}
	})

	t.Run("depth", func(t *testing.T) {
		store := newStore(t)
		// Thread items through the two hops (drain_policy + target + items).
		hop := `[` + darEnvRef("drain_policy", "drain_policy") + `,` + darEnvRef("target", "target") + `,` + darEnvRef("items", "items") + `]`
		leafFields := dadTwoInputFields + `,` + arrField("items")
		midFields := strField("drain_policy") + `,` + strField("target") + `,` + arrField("items")
		doc := decodeIR(t, bundleDoc(
			strField("drain_policy")+","+strField("target")+","+arrField("items"),
			runNodeRawEnv("continue-chain", nil, "midChain", hop),
			subDoc("midChain", midFields,
				runNodeRawEnv("continue-chain", nil, "leafChain", hop))+","+
				subDoc("leafChain", leafFields, fan+","+dispatchGated)+","+
				darLaneExecSub("drainSeparate")))
		res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": "separate", "target": "t", "items": []any{"x"}})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		settled := settledOutcomeByID(t, res.Events)
		if settled[dadBase+"d"] != engine.OutcomePass {
			t.Errorf("deep dispatch d = %q, want pass (aggregate subject invisible → no-match, root parity)", settled[dadBase+"d"])
		}
		if _, ok := settled[dadBase+"lanes"]; ok {
			t.Errorf("arm lanes minted at depth, want ZERO (deep aggregate subject read \"\")")
		}
	})
}

// TestDispatchMemberSubjectLoudErrorRootAndDepth (§2.2f / §0.5) pins the member-subject LOUD
// error at ROOT and at DEPTH: a member subject `X.Y` with no matching flat scope key falls
// through evalValue to a loud `unsupported value expression kind "member"` (never a silent "").
// Preserving the loud contract is load-bearing for the recover error bindings — DAD must NOT
// "fix" evalValue to resolve "" on a member miss.
func TestDispatchMemberSubjectLoudErrorRootAndDepth(t *testing.T) {
	ctx := context.Background()
	memberSubject := `{"kind":"member","base":{"kind":"ref","name":"nope"},"name":"reason"}`
	arm := darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout"))

	t.Run("root", func(t *testing.T) {
		doc := decodeIR(t, bundleDoc(
			strField("target"),
			dadDispatchSubject(memberSubject, arm),
			darLaneExecSub("drainSeparate")))
		_, err := engine.Run(ctx, newStore(t), doc, map[string]any{"target": "t"})
		if err == nil || !strings.Contains(err.Error(), `unsupported value expression kind "member"`) {
			t.Fatalf("root run err = %v, want a loud member-subject error", err)
		}
	})

	t.Run("depth", func(t *testing.T) {
		doc := decodeIR(t, dadChainNodes(
			dadDispatchSubject(memberSubject, arm)))
		_, err := engine.Run(ctx, newStore(t), doc, map[string]any{"drain_policy": "separate", "target": "t"})
		if err == nil || !strings.Contains(err.Error(), `unsupported value expression kind "member"`) {
			t.Fatalf("depth run err = %v, want a loud member-subject error (parity with root)", err)
		}
	})
}

// TestDispatchInterpPartsSubjectSilentRootAndDepth (§2.2g / §0.5) pins the interp-with-parts
// subject silent-"" parity: an interp value carrying `parts` but NO top-level `expr` evaluates
// to "" through evalValue's interp arm (never an error) at ROOT and at DEPTH — a no-match no-op
// (pre-existing, parity-preserved, corpus-absent).
func TestDispatchInterpPartsSubjectSilentRootAndDepth(t *testing.T) {
	ctx := context.Background()
	interpSubject := `{"kind":"interp","parts":[{"kind":"text","value":"x"}]}`
	arm := darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout"))

	t.Run("root", func(t *testing.T) {
		store := newStore(t)
		doc := decodeIR(t, bundleDoc(
			strField("target"),
			dadDispatchSubject(interpSubject, arm),
			darLaneExecSub("drainSeparate")))
		res, err := engine.Run(ctx, store, doc, map[string]any{"target": "t"})
		if err != nil {
			t.Fatalf("root run: %v (interp-parts subject must be silent \"\", not an error)", err)
		}
		if got := settledOutcomeByID(t, res.Events)["d"]; got != engine.OutcomePass {
			t.Errorf("root dispatch d = %q, want pass (interp-parts subject → \"\" → no-match)", got)
		}
	})

	t.Run("depth", func(t *testing.T) {
		store := newStore(t)
		doc := decodeIR(t, dadChainNodes(dadDispatchSubject(interpSubject, arm)))
		res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": "separate", "target": "t"})
		if err != nil {
			t.Fatalf("depth run: %v (interp-parts subject must be silent \"\" at depth too)", err)
		}
		if got := settledOutcomeByID(t, res.Events)[dadBase+"d"]; got != engine.OutcomePass {
			t.Errorf("deep dispatch d = %q, want pass (interp-parts subject → \"\" → no-match, root parity)", got)
		}
	})
}

// TestDispatchAtDepthNoMatchParity (§2.4, the deep no-match parity leg) proves a deep subject
// matching NO arm settles the dispatch PASS/"" at the QUALIFIED id (settleDecisionSkipped), does
// NOT skip-cascade (a deep {{d}} sibling runs and renders ""), and journals BYTE-IDENTICALLY
// inline (Run) vs pooled (Advance). Catalog: exhaustive is UNDECODED — no-match-PASS is
// exhaustive-blind at every depth.
func TestDispatchAtDepthNoMatchParity(t *testing.T) {
	ctx := context.Background()
	leaf := darDispatch("drain_policy", dadTwoRunArms()...) + "," +
		execNode("after", `echo "after: {{ d }}"`, []string{"d"})
	doc := decodeIR(t, dadChainNodes(leaf))
	input := map[string]any{"drain_policy": "neither", "target": "t"}

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, input)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	settled := settledOutcomeByID(t, inRes.Events)
	if settled[dadBase+"d"] != engine.OutcomePass {
		t.Errorf("deep dispatch d = %q, want pass at the qualified id (no-match settleDecisionSkipped)", settled[dadBase+"d"])
	}
	if _, ok := settled[dadBase+"lanes"]; ok {
		t.Errorf("arm lanes minted, want ZERO (no-match)")
	}
	if got := inRes.NodeOutputs[dadBase+"after"]; got != "after: " {
		t.Errorf("deep sibling after = %q, want %q (no skip-cascade; {{d}} empty)", got, "after: ")
	}

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-dad-nomatch", input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (no-match dispatches nothing)", r, err)
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("pool dispatch count = %d, want 0", fake.dispatchCount())
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestDispatchAtDepthLeafDoArmRendersEnvBoundValue (§2.5, the leaf-arm-at-depth DRIVE — do arm)
// proves a chosen LEAF do arm at depth renders its prompt via scopeFor(deep ns): the prompt
// carries the env-bound chain value (target, threaded through both hops). Both drivers: the pool
// materialize/observe/park cycle at the deep activation, and the inline render via a StubHost.
func TestDispatchAtDepthLeafDoArmRendersEnvBoundValue(t *testing.T) {
	ctx := context.Background()
	leaf := darDispatch("drain_policy", darDoArm("separate", "lanesDo", "drain leaf for {{ target }}"))
	doc := decodeIR(t, dadChainNodes(leaf))
	input := map[string]any{"drain_policy": "separate", "target": "release-9"}
	const doAct = dadBase + "lanesDo:0"

	t.Run("pool", func(t *testing.T) {
		store := newStore(t)
		fake := newFakeWorkStore()
		r1, err := engine.Advance(ctx, store, doc, "gcg-dad-leafdo", input, fake.opts())
		if err != nil || !r1.Parked {
			t.Fatalf("advance 1 = %+v err %v, want Parked (the deep do arm materialized, awaited)", r1, err)
		}
		if len(r1.InFlight) != 1 || r1.InFlight[0].NodeID != dadBase+"lanesDo" {
			t.Fatalf("InFlight = %+v, want the deep do arm %slanesDo materialized", r1.InFlight, dadBase)
		}
		if got := fake.dispatchPromptFor(t, doAct); got != "drain leaf for release-9" {
			t.Errorf("deep do arm prompt = %q, want %q (env-bound chain value rendered at depth)", got, "drain leaf for release-9")
		}
		fake.settleAct(t, doAct, engine.OutcomePass, "leaf-out")
		r2, err := engine.Advance(ctx, store, doc, "gcg-dad-leafdo", input, fake.opts())
		if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
			t.Fatalf("advance 2 = %+v err %v, want Sealed pass", r2, err)
		}
		if got := r2.Run.NodeOutputs[dadBase+"d"]; got != "leaf-out" {
			t.Errorf("deep dispatch output = %q, want leaf-out (transparent from the deep leaf arm)", got)
		}
		assertProjectionEqualsRefold(t, store, "gcg-dad-leafdo")
	})

	t.Run("inline", func(t *testing.T) {
		store := newStore(t)
		host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
			dadBase + "lanesDo": {Outcome: enginehost.OutcomePass, Output: "leaf-out"},
		}}
		res, err := engine.RunWithOptions(ctx, store, doc, input, engine.Options{Host: host})
		if err != nil {
			t.Fatalf("inline run: %v", err)
		}
		prompts := effectPrompts(t, res.Events)
		if len(prompts) != 1 || prompts[0] != "drain leaf for release-9" {
			t.Fatalf("inline effect prompts = %v, want one %q (deep leaf-do render via scopeFor)", prompts, "drain leaf for release-9")
		}
		if got := res.NodeOutputs[dadBase+"d"]; got != "leaf-out" {
			t.Errorf("deep dispatch output = %q, want leaf-out", got)
		}
	})
}

// TestDispatchAtDepthExecArmByteParity (§2.5, the byte-parity leg) proves an EXEC leaf arm at
// depth (which never pools) seals in one pass on BOTH drivers and journals byte-identically —
// the deep arm render + transparent settle carry no driver-dependent divergence.
func TestDispatchAtDepthExecArmByteParity(t *testing.T) {
	ctx := context.Background()
	leaf := darDispatch("drain_policy", darExecArm("lanesExec", `echo "drain {{ target }}"`))
	doc := decodeIR(t, dadChainNodes(leaf))
	input := map[string]any{"drain_policy": "separate", "target": "t"}

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, input)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if got := inRes.NodeOutputs[dadBase+"d"]; got != "drain t" {
		t.Errorf("deep exec arm dispatch output = %q, want %q", got, "drain t")
	}
	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-dad-execparity", input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec arm never pools)", r, err)
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (a deep exec arm dispatches nothing)", fake.dispatchCount())
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestDispatchRootByteParityAfterSlice (§2.3) guards the root path against the ns-gated guard
// and the scopeFor view indirection: a ROOT dispatch (ns=="") whose subject reads a record()-ed
// leaf sibling drives identically inline vs pooled AND drop+refolds byte-identically. scopeFor("")
// is a pure passthrough and the `u.ns != ""` guard short-circuits, so the root journal is
// unchanged by the slice.
func TestDispatchRootByteParityAfterSlice(t *testing.T) {
	ctx := context.Background()
	nodes := execNode("pick", `printf separate`, nil) + "," +
		dadDispatchSubject(`{"kind":"ref","name":"pick"}`,
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")),
			darRunArm("same-session", "sharedLane", "drainShared", darLaneEnv("shared"))) + "," +
		execNode("done", `echo "d={{ d }}"`, []string{"d"})
	doc := decodeIR(t, bundleDoc(strField("target"), nodes,
		darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")))
	input := map[string]any{"target": "t"}

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, input)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if got := inRes.NodeOutputs["d"]; got != "item=fanout target=t" {
		t.Errorf("root dispatch output = %q, want the separate arm (subject read `pick`)", got)
	}
	assertProjectionEqualsRefold(t, inStore, inRes.StreamID)

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-dad-rootparity", input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec run arm)", r, err)
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestDispatchAtDepthDropRefold (§2.7 DET) pins that the deep '/'-bearing dynamic arm rows (the
// deep arm aggregate and its deep sub-node) refold byte-identically, for BOTH the matched-run
// and no-match cases at depth.
func TestDispatchAtDepthDropRefold(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []string{"separate", "neither"} {
		store := newStore(t)
		leaf := darDispatch("drain_policy", dadTwoRunArms()...) + "," +
			execNode("after", `echo "d={{ d }}"`, []string{"d"})
		doc := decodeIR(t, dadChainNodes(leaf))
		res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": policy, "target": "t"})
		if err != nil {
			t.Fatalf("run(policy=%s): %v", policy, err)
		}
		assertProjectionEqualsRefold(t, store, res.StreamID)
	}
}

// TestAdvanceDispatchAtDepthWriteOnceRedundantAdvance (§2.6, per-pass re-decision + write-once on
// a grown fold at depth) proves a redundant Advance over a parked deep run-arm dispatch is a
// no-op: the pure matchingArm re-selects the SAME deep arm every pass, the stateless re-mint
// dedupes write-once (no head movement, no re-dispatch), and the unchosen deep arm stays
// zero-minted across passes and through the seal.
func TestAdvanceDispatchAtDepthWriteOnceRedundantAdvance(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	leaf := darDispatch("drain_policy",
		darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")),
		darRunArm("same-session", "sharedLanes", "drainShared", darLaneEnv("shared")))
	doc := decodeIR(t, dadChainWithSubs(leaf,
		darLaneDoSub("drainSeparate")+","+darLaneDoSub("drainShared")))
	input := map[string]any{"drain_policy": "separate", "target": "rel"}

	r1, err := engine.Advance(ctx, store, doc, "gcg-dad-writeonce", input, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	head := r1.Head
	for i := 0; i < 2; i++ {
		r, err := engine.Advance(ctx, store, doc, "gcg-dad-writeonce", input, fake.opts())
		if err != nil || !r.Parked {
			t.Fatalf("redundant advance %d = %+v err %v, want Parked", i, r, err)
		}
		if r.Head != head {
			t.Fatalf("redundant advance %d moved head %d -> %d (a double append on a grown fold)", i, head, r.Head)
		}
		activated := darActivatedSet(t, streamStored(t, store, "gcg-dad-writeonce"))
		if activated[dadBase+"sharedLanes"] || activated[dadBase+"sharedLanes/drain"] {
			t.Fatalf("redundant advance %d activated the unchosen deep arm; the pure re-select must pick the SAME arm", i)
		}
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("dispatch count across redundant advances = %d, want 1 (write-once)", fake.dispatchCount())
	}
	fake.settleAct(t, dadBase+"lanes/drain:0", engine.OutcomePass, "done")
	r2, err := engine.Advance(ctx, store, doc, "gcg-dad-writeonce", input, fake.opts())
	if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("final advance = %+v err %v, want Sealed pass", r2, err)
	}
	final := darActivatedSet(t, streamStored(t, store, "gcg-dad-writeonce"))
	if final[dadBase+"sharedLanes"] || final[dadBase+"sharedLanes/drain"] {
		t.Fatalf("unchosen deep arm has rows after seal; want ZERO mints across all passes")
	}
	assertProjectionEqualsRefold(t, store, "gcg-dad-writeonce")
}

// TestDispatchAtDepthCrossArmSkipCascade (§2.8) pins the static env-gate union at depth: a deep
// arm B's env reads a FAILED deep sibling; the subject selects deep arm A. The union gates the
// WHOLE deep dispatch on arm B's failed env dep → the dispatch SKIPS with zero mints (sharply
// different from a no-match PASS), and the deep `after` sibling skip-cascades off it.
func TestDispatchAtDepthCrossArmSkipCascade(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// `bad` (a deep sibling) fails; arm B binds target <- ref bad, so the deep dispatch gates on
	// the deep bad node. The subject selects arm A (separate).
	armAEnv := `[` + darEnvReviewer("a") + `,` + darEnvRef("target", "target") + `]`
	armBEnv := `[` + darEnvReviewer("b") + `,` + darEnvRef("target", "bad") + `]`
	leaf := execNode("bad", `echo x; exit 1`, nil) + "," +
		darDispatch("drain_policy",
			darRunArm("separate", "lanes", "drainSeparate", armAEnv),
			darRunArm("same-session", "sharedLanes", "drainShared", armBEnv)) + "," +
		execNode("after", `echo "after: {{ d }}"`, []string{"d"})
	doc := decodeIR(t, dadChainNodes(leaf))
	res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": "separate", "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled[dadBase+"d"] != engine.OutcomeSkipped {
		t.Errorf("deep dispatch d = %q, want skipped (cross-arm env-dep failed → skip-cascade, NOT no-match pass)", settled[dadBase+"d"])
	}
	if _, ok := settled[dadBase+"lanes"]; ok {
		t.Errorf("chosen deep arm lanes settled %q, want ZERO mints (dispatch skip-cascaded)", settled[dadBase+"lanes"])
	}
	if settled[dadBase+"after"] != engine.OutcomeSkipped {
		t.Errorf("deep after = %q, want skipped (skip-cascade off the skipped deep dispatch)", settled[dadBase+"after"])
	}
}

// TestDispatchAtDepthFailedTransparentDownstreamRead (§2.10) pins the downstream reads of a
// FAILED transparent run arm at depth: the deep dispatch id `implement` settles FAILED (the
// arm sub-exec fails → arm aggregate fails → dispatch fails transparently), and a deep sibling
// gated `after:[implement]` reading {{implement}} skip-cascades. A sibling guard whose cond
// reads implement.outcome is skip-cascaded too (cond-ref gates are blocking — the DAR pin).
func TestDispatchAtDepthFailedTransparentDownstreamRead(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	failSub := subDoc("drainSeparate", strField("reviewer")+","+strField("target"),
		execNode("drain", `echo "start"; exit 1`, nil))
	implement := `{"kind":"dispatch","id":"implement","name":"implement","after":[],` +
		`"subject":{"kind":"ref","name":"drain_policy"},"arms":[` +
		darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")) + `]}`
	leaf := implement + "," +
		execNode("after", `echo "after: {{ implement }}"`, []string{"implement"}) + "," +
		guardExecAfter("g", nil, condOutcomeEq("implement", "failed"), "gthen", `echo "saw fail"`)
	doc := decodeIR(t, dadChainWithSubs(leaf, failSub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"drain_policy": "separate", "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled[dadBase+"lanes"] != engine.OutcomeFailed || settled[dadBase+"implement"] != engine.OutcomeFailed {
		t.Errorf("settles = {arm:%q implement:%q}, want both failed (transparent from the failed deep arm)", settled[dadBase+"lanes"], settled[dadBase+"implement"])
	}
	if settled[dadBase+"after"] != engine.OutcomeSkipped {
		t.Errorf("deep after = %q, want skipped (a failed transparent dispatch skip-cascades its `after` dependents)", settled[dadBase+"after"])
	}
	if settled[dadBase+"g"] != engine.OutcomeSkipped {
		t.Errorf("deep guard g = %q, want skipped (cond-ref gate on implement is blocking; the cond never evaluates)", settled[dadBase+"g"])
	}
	if _, ok := settled[dadBase+"gthen"]; ok {
		t.Errorf("deep guard then gthen settled %q, want never-run", settled[dadBase+"gthen"])
	}
}

// dadEmptyChain wraps leafNodes in the two-hop static run chain with EMPTY inputs and empty
// hop environments — the shape the inline crash/resume proofs need (injectCrashThenResume
// threads nil input, so the deep dispatch subject must resolve from a DEEP sibling, not input).
func dadEmptyChain(leafNodes, laneSubs string) string {
	empty := `[]`
	return bundleDoc("",
		runNodeRawEnv("continue-chain", nil, "midChain", empty),
		subDoc("midChain", "", runNodeRawEnv("continue-chain", nil, "leafChain", empty))+","+
			subDoc("leafChain", "", leafNodes)+","+laneSubs)
}

// TestAdvanceDispatchAtDepthCrashAfterDispatchReAdopts (§2.7 crash window, pool) pins the crash
// seam over a DEEP run arm: the deep arm sub-do's work bead is created, the crash fires BEFORE
// its owned.admitted fact commits. The re-Advance re-looks-up the SAME bead via the stateless
// re-mint route (never fast-pathing the nil deep arm agg into leaf machinery) and converges with
// a byte-identical refold.
func TestAdvanceDispatchAtDepthCrashAfterDispatchReAdopts(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	input := map[string]any{"drain_policy": "separate", "target": "t"}
	leaf := darDispatch("drain_policy", darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")))
	doc := decodeIR(t, dadChainWithSubs(leaf, darLaneDoSub("drainSeparate")))
	const subAct = dadBase + "lanes/drain:0"

	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterDispatch && act == subAct {
			return errInjectedDARCrash
		}
		return nil
	})
	_, err := engine.Advance(ctx, store, doc, "gcg-dad-crashdisp", input, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash")
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls before crash = %d, want 1", fake.dispatchCount())
	}
	r2, err := engine.Advance(ctx, store, doc, "gcg-dad-crashdisp", input, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 {
		t.Fatalf("re-advance = %+v err %v, want Parked with the deep sub-do in flight", r2, err)
	}
	fake.mu.Lock()
	minted := fake.seq
	fake.mu.Unlock()
	if minted != 1 {
		t.Fatalf("distinct beads minted = %d, want 1 (re-adopted, not re-minted)", minted)
	}
	fake.settleAllDispatchedPass()
	r3, err := engine.Advance(ctx, store, doc, "gcg-dad-crashdisp", input, fake.opts())
	if err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("final advance = %+v err %v, want Sealed pass", r3, err)
	}
	assertProjectionEqualsRefold(t, store, "gcg-dad-crashdisp")
}

// TestAdvanceDispatchAtDepthAggActivatedUnsettledResumes (§2.7, the flagship window at DEPTH)
// injects the crash BETWEEN the deep arm aggregate's two appends (CrashAfterActivate at the deep
// arm agg): the aggregate node EXISTS but is UNSETTLED, so the resume pass's chosenArm fires
// while the fast path is undecidable. chosenArm may only SKIP re-matchingArm — the route MUST
// still be the kind-route re-mint (byte-identical dedupe, re-registering the deep env seam +
// parentNS) and the drive to settle. Asserted here: convergence to Sealed pass, a SINGLE bead
// across the crash, and a byte-identical refold. The deep prompt assert runs PRE-crash (the
// genesis render); the sub-do is already settled inside this window, so the resume pass renders
// nothing — the post-resume deep re-render pin lives in TestDispatchAtDepthCrashPreMintResumes.
func TestAdvanceDispatchAtDepthAggActivatedUnsettledResumes(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	input := map[string]any{"drain_policy": "separate", "target": "rel"}
	leaf := darDispatch("drain_policy", darRunArm("separate", "lanes", "drainSeparate", darLaneEnv("fanout")))
	doc := decodeIR(t, dadChainWithSubs(leaf, darLaneDoSub("drainSeparate")))
	const armAgg = dadBase + "lanes:0"
	const subAct = dadBase + "lanes/drain:0"

	r1, err := engine.Advance(ctx, store, doc, "gcg-dad-aggact", input, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	if got := fake.dispatchPromptFor(t, subAct); got != "drain fanout for rel" {
		t.Fatalf("deep sub-do prompt = %q, want %q (env-rendered deep arm sub)", got, "drain fanout for rel")
	}
	fake.settleAct(t, subAct, engine.OutcomePass, "done")

	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterActivate && act == armAgg {
			return errInjectedDARCrash
		}
		return nil
	})
	_, err = engine.Advance(ctx, store, doc, "gcg-dad-aggact", input, fake.opts())
	restore()
	if !errors.Is(err, errInjectedDARCrash) {
		t.Fatalf("advance 2 returned %v, want the injected crash between the deep aggregate's two appends", err)
	}
	events := streamStored(t, store, "gcg-dad-aggact")
	mid := settledOutcomeByID(t, events)
	if !darActivatedSet(t, events)[dadBase+"lanes"] {
		t.Fatalf("crash window: deep arm aggregate not activated — the boundary did not leave the activated-unsettled state")
	}
	if _, ok := mid[dadBase+"lanes"]; ok {
		t.Fatalf("crash window: deep arm aggregate settled %q, want ACTIVATED-UNSETTLED", mid[dadBase+"lanes"])
	}
	if _, ok := mid[dadBase+"d"]; ok {
		t.Fatalf("crash window: deep dispatch settled %q, want UNSETTLED", mid[dadBase+"d"])
	}

	r3, err := engine.Advance(ctx, store, doc, "gcg-dad-aggact", input, fake.opts())
	if err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 3 = %+v err %v, want Sealed pass (kind-route re-mint over the activated-unsettled deep aggregate)", r3, err)
	}
	fake.mu.Lock()
	minted := fake.seq
	fake.mu.Unlock()
	if minted != 1 {
		t.Fatalf("distinct beads = %d, want 1 (no dup mints across the crash)", minted)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, "gcg-dad-aggact"))
	if settled[dadBase+"lanes"] != engine.OutcomePass || settled[dadBase+"d"] != engine.OutcomePass {
		t.Errorf("final settles = {arm:%q d:%q}, want both pass", settled[dadBase+"lanes"], settled[dadBase+"d"])
	}
	assertProjectionEqualsRefold(t, store, "gcg-dad-aggact")
}

// TestDispatchAtDepthCrashPreMintResumes (§2.7 pre-mint crash cell, inline) crashes BEFORE
// anything of the matched DEEP arm is on disk (CrashBeforeActivate at the deep arm's sub-do):
// the deep dispatch is activated and the arm was matched in memory, but no sub-node, no
// aggregate, no bead exists. Resume re-selects from scratch (pure matchingArm over the deep
// sibling `pick`, ⚑B1 purity → the same arm, byte-identical mint), drives the sub (host exactly
// once), settles the deep chain. The deep subject reads a DEEP sibling so nil-input resume still
// selects the arm. The deep sub-do's ONLY render happens ON RESUME (nothing rendered pre-crash),
// so its rendered prompt content proves the deep env seam re-registered on the resume path.
func TestDispatchAtDepthCrashPreMintResumes(t *testing.T) {
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		dadBase + "lanes/drain": {Outcome: enginehost.OutcomePass, Output: "r0"},
	}}
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, dadDeepPickDoDoc()), host,
		engine.CrashBeforeActivate, dadBase+"lanes/drain:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	for _, id := range []string{dadBase + "lanes/drain", dadBase + "lanes", dadBase + "d"} {
		if settled[id] != engine.OutcomePass {
			t.Errorf("resumed settle %s = %q, want pass (pure re-select from scratch at depth)", id, settled[id])
		}
	}
	if calls := host.Calls(); len(calls) != 1 {
		t.Errorf("host calls = %d, want 1 (nothing minted pre-crash; the deep sub runs once on resume)", len(calls))
	}
	// The post-resume render pin: the crash fired before the sub-do's activation, so the ONLY
	// effect.scheduled for the deep drain is the RESUME pass's — its prompt must carry the
	// env-bound value through the re-registered deep seam (the ⚑B1 render-content discipline).
	prompts := effectPrompts(t, resumed.Events)
	if len(prompts) != 1 || prompts[0] != "drain fanout" {
		t.Errorf("resume effect prompts = %v, want exactly one %q (the deep render happens ON RESUME through the re-registered env seam)", prompts, "drain fanout")
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// dadDeepPickDoDoc builds the inline-crash chain: an empty two-hop chain whose leaf holds a deep
// `pick` exec (emits "separate") gating a dispatch that reads it, with a single RUN arm minting a
// deep do-drain sub (drainSeparate, reviewer bound to the literal "fanout"). The deep subject
// resolves from the deep sibling, so injectCrashThenResume's nil input still selects the arm.
func dadDeepPickDoDoc() string {
	sub := subDoc("drainSeparate", strField("reviewer"),
		doNode("drain", "drain {{ reviewer }}", nil))
	env := `[` + darEnvReviewer("fanout") + `]`
	leaf := execNode("pick", `printf separate`, nil) + "," +
		dadDispatchSubject(`{"kind":"ref","name":"pick"}`,
			darRunArm("separate", "lanes", "drainSeparate", env))
	// Gate the dispatch after the deep sibling `pick`.
	leaf = strings.Replace(leaf, `"id":"d","name":"d","after":[]`, `"id":"d","name":"d","after":["pick"]`, 1)
	return dadEmptyChain(leaf, sub)
}

// TestDispatchAtDepthAggActivatedUnsettledInlineResumes (§2.7, the INLINE twin of the flagship
// window at depth) crashes the inline driver BETWEEN the deep arm aggregate's two appends
// (CrashAfterActivate at the deep arm agg) and resumes: matchingArm re-selects the same deep arm
// off the deep sibling, the kind-route re-mints (byte-identical dedupe re-registers the deep env
// seam), the settled sub reloads (host at-most-once), the aggregate settles, the dispatch
// settles. The leaf-route mutant would re-append the aggregate activation with a divergent leaf
// payload and wedge loudly.
func TestDispatchAtDepthAggActivatedUnsettledInlineResumes(t *testing.T) {
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		dadBase + "lanes/drain": {Outcome: enginehost.OutcomePass, Output: "r0"},
	}}
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, dadDeepPickDoDoc()), host,
		engine.CrashAfterActivate, dadBase+"lanes:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	if settled[dadBase+"lanes"] != engine.OutcomePass || settled[dadBase+"d"] != engine.OutcomePass {
		t.Errorf("resumed settles = {agg:%q d:%q}, want both pass (kind-route re-mint over the activated-unsettled deep agg)", settled[dadBase+"lanes"], settled[dadBase+"d"])
	}
	if calls := host.Calls(); len(calls) != 1 {
		t.Errorf("host calls = %d across crash+resume, want 1 (sub reloaded, not re-invoked)", len(calls))
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestDispatchAtDepthSubjectEnvBindingErrorPropagates (red-team P1, the swallowed-error mutant
// killer) pins matchingArm's scopeFor error PROPAGATION at depth: the leaf hop binds the subject
// input drain_policy to a MEMBER expr whose flat key misses (evalValue's loud member contract),
// so building the deep view fails inside runInputLayer and the dispatch must surface the wrapped
// error — `dispatch … subject: … run env "drain_policy" … member` — on BOTH drivers. The
// deletion mutant (dropping matchingArm's scopeFor err check; err is legally shadowed by the
// next evalValue assignment) collapses this to view=nil → subject "" → a SILENT no-match PASS —
// the swallowed-error class on the exact surface DAD opened. This test goes red under it.
func TestDispatchAtDepthSubjectEnvBindingErrorPropagates(t *testing.T) {
	ctx := context.Background()
	// The LEAF hop's env: drain_policy <- a member expr (base ref `nope`, name `reason`) — the
	// flat key "nope.reason" misses in the mid view, so evalValue errs loudly (§0.5).
	leafHopEnv := `[{"name":"drain_policy","value":{"kind":"expr","expr":` +
		`{"kind":"member","base":{"kind":"ref","name":"nope"},"name":"reason"}}},` +
		darEnvRef("target", "target") + `]`
	doc := decodeIR(t, bundleDoc(
		dadTwoInputFields,
		runNodeRawEnv("continue-chain", nil, "midChain", dadHopEnv()),
		subDoc("midChain", dadTwoInputFields,
			runNodeRawEnv("continue-chain", nil, "leafChain", leafHopEnv))+","+
			subDoc("leafChain", dadTwoInputFields,
				darDispatch("drain_policy", dadTwoRunArms()...))+","+
			darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")))
	input := map[string]any{"drain_policy": "separate", "target": "t"}

	assertPropagated := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("run completed, want the deep view-build error surfaced (NOT a silent no-match PASS)")
		}
		if !strings.Contains(err.Error(), "dispatch") ||
			!strings.Contains(err.Error(), `run env "drain_policy"`) {
			t.Fatalf("err = %v, want the wrapped chain naming BOTH the dispatch and run env %q", err, "drain_policy")
		}
	}

	t.Run("inline", func(t *testing.T) {
		_, err := engine.Run(ctx, newStore(t), doc, input)
		assertPropagated(t, err)
	})

	t.Run("pool", func(t *testing.T) {
		fake := newFakeWorkStore()
		_, err := engine.Advance(ctx, newStore(t), doc, "gcg-dad-properr", input, fake.opts())
		assertPropagated(t, err)
	})
}
