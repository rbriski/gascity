package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"testing"
	"time"
)

// These keys mirror the production dispatcher vocabulary the serve tick passes
// (beadmeta.RunTargetMetadataKey / RoutedToMetadataKey / InstantiatingMetadataKey,
// dispatch_runtime.go:810-811,781-783). The capability itself is
// vocabulary-agnostic — it takes the keys as params — so the test pins them
// locally rather than importing dispatcher constants upward into this layer.
const (
	fkRunTarget     = "gc.run_target"
	fkRoutedTo      = "gc.routed_to"
	fkInstantiating = "gc.instantiating"
)

// feEdge is the test's own record of a seeded graph edge, kept so the oracle can
// model bd's is_blocked over the exact edge set (including waits-for gate
// metadata, which hydrated Beads do not carry) without reading any store
// internal.
type feEdge struct {
	from    string
	to      string
	depType string
	gate    string // waits-for gate metadata value, e.g. "any-children"
}

// controlFrontierOracle is an INDEPENDENT in-Go model of the bd|jq serve-tick
// frontier (dispatch_runtime.go:771-826). It shares no helper with the
// implementation: it re-derives bd's is_blocked / deferred-child sets, the
// exclude set, the tier sorts, the per-tier cap, and the dedupe directly from
// the bd source, so agreeing with JournalStore.ControlFrontier is a genuine
// parity proof, not a tautology.
func controlFrontierOracle(all []Bead, edges []feEdge, params ControlFrontierParams, now time.Time) []Bead {
	tierBoth := params.IncludeEphemeral

	byID := map[string]Bead{}
	for _, b := range all {
		byID[b.ID] = b
	}
	blocked := oracleBlockedSet(all, edges)
	deferredChild := oracleDeferredChildSet(all, edges, now)

	candidate := func(b Bead) bool {
		if blocked[b.ID] || deferredChild[b.ID] {
			return false
		}
		if b.Ephemeral && !tierBoth {
			return false // TierIssues hides ephemeral
		}
		if b.Status != "open" {
			return false
		}
		if oracleExcludedType(b.Type) {
			return false
		}
		if b.DeferUntil != nil && b.DeferUntil.After(now) {
			return false
		}
		return true
	}

	var merged []Bead

	seenCand := map[string]bool{}
	for _, cand := range params.AssigneeCandidates {
		if cand == "" || seenCand[cand] {
			continue
		}
		seenCand[cand] = true
		var rows []Bead
		for _, b := range all {
			if b.Assignee == cand && candidate(b) {
				rows = append(rows, b)
			}
		}
		oracleSortPriority(rows) // bd default --sort priority
		merged = append(merged, oracleCap(rows, params.LimitPerTier)...)
	}

	for _, route := range params.Routes {
		if route == "" {
			continue
		}
		for _, key := range params.RouteMetadataKeys {
			if key == "" {
				continue
			}
			var rows []Bead
			for _, b := range all {
				if b.Assignee == "" && b.Metadata[key] == route && candidate(b) {
					rows = append(rows, b)
				}
			}
			oracleSortOldest(rows) // --sort oldest
			merged = append(merged, oracleCap(rows, params.LimitPerTier)...)
		}
	}

	return oracleDedupe(merged, params.InstantiatingMetadataKey)
}

// oracleBlockedSet independently reproduces bd's is_blocked mark fixpoint
// (blocked_state.go): blocks/conditional-blocks (INNER-JOIN, dangling => not
// blocked), waits-for gate (spawner has an active parent-child child, released
// under gate=any-children with a closed child), and the transitive parent-child
// cascade off a blocked parent — with closed/pinned subjects never blocked.
func oracleBlockedSet(all []Bead, edges []feEdge) map[string]bool {
	status := map[string]string{}
	for _, b := range all {
		status[b.ID] = b.Status
	}
	active := func(id string) bool {
		st, ok := status[id]
		return ok && st != "closed" && st != "pinned"
	}
	childrenOf := map[string][]string{}
	for _, e := range edges {
		if e.depType == "parent-child" {
			childrenOf[e.to] = append(childrenOf[e.to], e.from)
		}
	}
	gateBlocks := func(spawner, gate string) bool {
		hasActive, hasClosed := false, false
		for _, c := range childrenOf[spawner] {
			switch st := status[c]; {
			case st == "closed":
				hasClosed = true
			case st != "pinned":
				hasActive = true
			}
		}
		if !hasActive {
			return false
		}
		if gate == "any-children" && hasClosed {
			return false
		}
		return true
	}
	direct := func(id string) bool {
		for _, e := range edges {
			if e.from != id {
				continue
			}
			switch e.depType {
			case "blocks", "conditional-blocks":
				if active(e.to) {
					return true
				}
			case "waits-for":
				if gateBlocks(e.to, e.gate) {
					return true
				}
			}
		}
		return false
	}
	eligible := func(id string) bool { return active(id) }

	blocked := map[string]bool{}
	for id := range status {
		if eligible(id) && direct(id) {
			blocked[id] = true
		}
	}
	for {
		changed := false
		for _, e := range edges {
			if e.depType != "parent-child" {
				continue
			}
			if blocked[e.from] || !eligible(e.from) {
				continue
			}
			if blocked[e.to] {
				blocked[e.from] = true
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return blocked
}

// oracleDeferredChildSet reproduces bd's children-of-future-deferred-parent
// exclusion (ready_work.go:487-553), single-hop.
func oracleDeferredChildSet(all []Bead, edges []feEdge, now time.Time) map[string]bool {
	future := map[string]bool{}
	for _, b := range all {
		if b.DeferUntil != nil && b.DeferUntil.After(now) {
			future[b.ID] = true
		}
	}
	out := map[string]bool{}
	for _, e := range edges {
		if e.depType == "parent-child" && future[e.to] {
			out[e.from] = true
		}
	}
	return out
}

// oracleExcludedType is the bd ready exclude set the dispatcher invocation sees:
// ReadyWorkExcludeTypes (merge-request, gate, molecule, rig) + DefaultInfraTypes
// (agent, role, message) + shell --exclude-type=epic. No step/convoy/session, no
// label exclusion.
func oracleExcludedType(t string) bool {
	switch t {
	case "merge-request", "gate", "molecule", "rig", "agent", "role", "message", "epic":
		return true
	}
	return false
}

func oracleSortPriority(items []Bead) {
	pri := func(b Bead) int {
		if b.Priority == nil {
			return 2
		}
		return *b.Priority
	}
	sort.SliceStable(items, func(i, j int) bool {
		if pri(items[i]) != pri(items[j]) {
			return pri(items[i]) < pri(items[j])
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt) // DESC
		}
		return items[i].ID < items[j].ID
	})
}

func oracleSortOldest(items []Bead) {
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
}

func oracleCap(items []Bead, limit int) []Bead {
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

func oracleDedupe(merged []Bead, instantiatingKey string) []Bead {
	out := make([]Bead, 0, len(merged))
	seen := map[string]bool{}
	for _, b := range merged {
		if instantiatingKey != "" && b.Metadata[instantiatingKey] != "" {
			continue
		}
		if seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		out = append(out, b)
	}
	return out
}

// feSeed seeds a bead through the write-closed façade and records its identity
// for the test. Dependencies are seeded separately (feEdgeSeed) so waits-for gate
// metadata — which Create cannot carry on a Dep — is exercised faithfully.
func feSeed(t *testing.T, s *JournalStore, b Bead) Bead {
	t.Helper()
	created, err := s.Create(b)
	if err != nil {
		t.Fatalf("seed create %q: %v", b.Title, err)
	}
	return created
}

// feEdgeSeed writes one edge directly (bypassing Create's empty-metadata dep
// path) so blocks / parent-child / gated waits-for edges are all seeded through a
// single path the oracle mirrors via the returned feEdge. to may dangle
// (nonexistent target) — edges only FK from_id.
func feEdgeSeed(t *testing.T, s *JournalStore, edges *[]feEdge, from, to, depType, gate string) {
	t.Helper()
	metadata := ""
	if gate != "" {
		b, err := json.Marshal(map[string]string{"gate": gate})
		if err != nil {
			t.Fatalf("marshal gate: %v", err)
		}
		metadata = string(b)
	}
	ctx := context.Background()
	if err := s.withTx(ctx, func(tx *sql.Tx) error {
		return journalInsertEdge(ctx, tx, from, to, depType, metadata)
	}); err != nil {
		t.Fatalf("seed edge %s->%s (%s): %v", from, to, depType, err)
	}
	*edges = append(*edges, feEdge{from: from, to: to, depType: depType, gate: gate})
}

// TestControlFrontierMatchesBDReadySemantics is the P2.1 Layer-1 lockstep parity
// proof: it seeds a graph covering every readiness/tier/metadata/blocked/dedupe
// dimension bd's ready|jq frontier discriminates, then asserts ControlFrontier
// returns EXACTLY the ordered id list the independent bd-modeling oracle selects,
// across a matrix of param shapes.
func TestControlFrontierMatchesBDReadySemantics(t *testing.T) {
	s := newJournalTestStore(t)
	var edges []feEdge
	base := journalNow().Add(-time.Hour) // all created in the past; deterministic order
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	p := func(v int) *int { return &v }
	future := journalNow().Add(time.Hour)

	const cand = "core.control-dispatcher"
	const route = "core.control-dispatcher"

	// --- assignee tier: priority sort, created_at DESC tie-break (finding 4) ---
	aP0older := feSeed(t, s, Bead{Title: "assignee-p0-older", Type: "task", Assignee: cand, Priority: p(0), CreatedAt: at(10)})
	aP0newer := feSeed(t, s, Bead{Title: "assignee-p0-newer", Type: "task", Assignee: cand, Priority: p(0), CreatedAt: at(20)})
	aP1 := feSeed(t, s, Bead{Title: "assignee-p1", Type: "task", Assignee: cand, Priority: p(1), CreatedAt: at(5)})
	feSeed(t, s, Bead{Title: "assignee-miss-other-worker", Type: "task", Assignee: "someone-else", CreatedAt: at(11)})

	// gc:session LABEL must NOT be excluded by the frontier (finding 5).
	aLabeled := feSeed(t, s, Bead{Title: "assignee-labeled-still-ready", Type: "task", Assignee: cand, Priority: p(0), CreatedAt: at(30), Labels: []string{"gc:session"}})

	// --- routed tier: routed_to match / miss / --sort oldest ordering ---
	rOld := feSeed(t, s, Bead{Title: "routed-oldest", Type: "task", Metadata: StringMap{fkRoutedTo: route}, CreatedAt: at(1)})
	rNew := feSeed(t, s, Bead{Title: "routed-newer", Type: "task", Metadata: StringMap{fkRoutedTo: route}, Priority: p(0), CreatedAt: at(9)})
	feSeed(t, s, Bead{Title: "routed-miss-wrong-value", Type: "task", Metadata: StringMap{fkRoutedTo: "other-route"}, CreatedAt: at(2)})
	feSeed(t, s, Bead{Title: "routed-but-assigned", Type: "task", Assignee: "w", Metadata: StringMap{fkRoutedTo: route}, CreatedAt: at(3)})

	// --- run_target tier + first-wins dedupe across run_target/routed_to ---
	rtBoth := feSeed(t, s, Bead{Title: "routed-both-keys-dedupe", Type: "task", Metadata: StringMap{fkRunTarget: route, fkRoutedTo: route}, CreatedAt: at(4)})

	// --- excluded types (must never appear): finding 5 exact set ---
	feSeed(t, s, Bead{Title: "epic-excluded", Type: "epic", Assignee: cand, CreatedAt: at(6)})
	feSeed(t, s, Bead{Title: "message-excluded", Type: "message", Assignee: cand, CreatedAt: at(7)})
	feSeed(t, s, Bead{Title: "molecule-excluded", Type: "molecule", Assignee: cand, CreatedAt: at(8)})
	// A type bd does NOT exclude (session/step/convoy are not in bd's set): present.
	aStep := feSeed(t, s, Bead{Title: "step-not-excluded-by-bd", Type: "step", Assignee: cand, Priority: p(1), CreatedAt: at(31)})

	// --- non-ready: closed / deferred / instantiating ---
	feSeed(t, s, Bead{Title: "closed-excluded", Type: "task", Status: "closed", Assignee: cand, CreatedAt: at(12)})
	feSeed(t, s, Bead{Title: "deferred-excluded", Type: "task", Assignee: cand, DeferUntil: &future, CreatedAt: at(13)})
	feSeed(t, s, Bead{Title: "instantiating-dropped", Type: "task", Assignee: cand, Metadata: StringMap{fkInstantiating: "root-x"}, CreatedAt: at(14)})

	// --- finding 2: blocks open dep blocks; dangling dep does NOT block ---
	openTarget := feSeed(t, s, Bead{Title: "open-blocking-target", Type: "task", Assignee: "keeper", CreatedAt: at(15)})
	closedTarget := feSeed(t, s, Bead{Title: "closed-blocking-target", Type: "task", Status: "closed", Assignee: "keeper", CreatedAt: at(16)})
	blockedByOpen := feSeed(t, s, Bead{Title: "blocked-by-open-dep", Type: "task", Assignee: cand, CreatedAt: at(17)})
	feEdgeSeed(t, s, &edges, blockedByOpen.ID, openTarget.ID, "blocks", "")
	unblockedByClosed := feSeed(t, s, Bead{Title: "unblocked-closed-dep", Type: "task", Assignee: cand, Priority: p(1), CreatedAt: at(18)})
	feEdgeSeed(t, s, &edges, unblockedByClosed.ID, closedTarget.ID, "blocks", "")
	danglingDep := feSeed(t, s, Bead{Title: "dangling-dep-not-blocked", Type: "task", Assignee: cand, Priority: p(1), CreatedAt: at(19)})
	feEdgeSeed(t, s, &edges, danglingDep.ID, "gcg-jdoes-not-exist", "blocks", "")

	// --- finding 1: waits-for gate semantics ---
	// Spawner S2 (CLOSED itself, so target-status is irrelevant) with one OPEN
	// child => an all-children gate on it blocks.
	spawnerBlocking := feSeed(t, s, Bead{Title: "spawner-blocking-closed", Type: "task", Status: "closed", Assignee: "keeper", CreatedAt: at(40)})
	s2Kid := feSeed(t, s, Bead{Title: "s2-open-child", Type: "task", Assignee: "keeper", CreatedAt: at(41)})
	feEdgeSeed(t, s, &edges, s2Kid.ID, spawnerBlocking.ID, "parent-child", "")
	gateBlocked := feSeed(t, s, Bead{Title: "gate-blocked-open-child", Type: "task", Assignee: cand, CreatedAt: at(42)})
	feEdgeSeed(t, s, &edges, gateBlocked.ID, spawnerBlocking.ID, "waits-for", "all-children")

	// Spawner S1 with an OPEN child AND a CLOSED child. gate=any-children releases
	// early (a child closed); gate=all-children stays blocked (an open child
	// remains).
	spawnerMixed := feSeed(t, s, Bead{Title: "spawner-mixed-children", Type: "task", Status: "closed", Assignee: "keeper", CreatedAt: at(43)})
	s1Open := feSeed(t, s, Bead{Title: "s1-open-child", Type: "task", Assignee: "keeper", CreatedAt: at(44)})
	feEdgeSeed(t, s, &edges, s1Open.ID, spawnerMixed.ID, "parent-child", "")
	s1Closed := feSeed(t, s, Bead{Title: "s1-closed-child", Type: "task", Status: "closed", Assignee: "keeper", CreatedAt: at(45)})
	feEdgeSeed(t, s, &edges, s1Closed.ID, spawnerMixed.ID, "parent-child", "")
	gateReleased := feSeed(t, s, Bead{Title: "gate-released-any-children", Type: "task", Assignee: cand, Priority: p(0), CreatedAt: at(46)})
	feEdgeSeed(t, s, &edges, gateReleased.ID, spawnerMixed.ID, "waits-for", "any-children")
	gateAllBlocked := feSeed(t, s, Bead{Title: "gate-all-children-not-released", Type: "task", Assignee: cand, CreatedAt: at(47)})
	feEdgeSeed(t, s, &edges, gateAllBlocked.ID, spawnerMixed.ID, "waits-for", "all-children")

	// Spawner S3 with NO children => gate does not block (ready).
	spawnerNoKids := feSeed(t, s, Bead{Title: "spawner-no-children", Type: "task", Assignee: "keeper", CreatedAt: at(48)})
	gateNoKids := feSeed(t, s, Bead{Title: "gate-no-children-ready", Type: "task", Assignee: cand, Priority: p(0), CreatedAt: at(49)})
	feEdgeSeed(t, s, &edges, gateNoKids.ID, spawnerNoKids.ID, "waits-for", "all-children")

	// --- finding 3: parent-child cascade (transitive) + deferred-parent child ---
	blockedParent := feSeed(t, s, Bead{Title: "cascade-blocked-parent", Type: "task", Assignee: "keeper", CreatedAt: at(50)})
	feEdgeSeed(t, s, &edges, blockedParent.ID, openTarget.ID, "blocks", "") // parent blocked by open dep
	cascadeChild := feSeed(t, s, Bead{Title: "cascade-child", Type: "task", Assignee: cand, CreatedAt: at(51)})
	feEdgeSeed(t, s, &edges, cascadeChild.ID, blockedParent.ID, "parent-child", "")
	cascadeGrandchild := feSeed(t, s, Bead{Title: "cascade-grandchild", Type: "task", Assignee: cand, CreatedAt: at(52)})
	feEdgeSeed(t, s, &edges, cascadeGrandchild.ID, cascadeChild.ID, "parent-child", "")

	deferredParent := feSeed(t, s, Bead{Title: "deferred-parent", Type: "task", Assignee: "keeper", DeferUntil: &future, CreatedAt: at(53)})
	deferredParentChild := feSeed(t, s, Bead{Title: "deferred-parent-child", Type: "task", Assignee: cand, CreatedAt: at(54)})
	feEdgeSeed(t, s, &edges, deferredParentChild.ID, deferredParent.ID, "parent-child", "")

	// --- ephemeral: hidden unless IncludeEphemeral ---
	ephRouted := feSeed(t, s, Bead{Title: "ephemeral-routed", Type: "task", Ephemeral: true, Metadata: StringMap{fkRoutedTo: route}, CreatedAt: at(0)})

	// Snapshot every seeded bead as ControlFrontier reads it, for the oracle.
	allBeads := listAllFrontierBeads(t, s)
	now := journalNow()

	cases := []struct {
		name   string
		params ControlFrontierParams
	}{
		{
			name: "durable-full-tiers",
			params: ControlFrontierParams{
				AssigneeCandidates:       []string{cand, "", cand /*dup skipped*/},
				Routes:                   []string{route},
				RouteMetadataKeys:        []string{fkRunTarget, fkRoutedTo},
				InstantiatingMetadataKey: fkInstantiating,
			},
		},
		{
			name: "include-ephemeral",
			params: ControlFrontierParams{
				AssigneeCandidates:       []string{cand},
				Routes:                   []string{route},
				RouteMetadataKeys:        []string{fkRunTarget, fkRoutedTo},
				InstantiatingMetadataKey: fkInstantiating,
				IncludeEphemeral:         true,
			},
		},
		{
			name: "per-tier-limit-1",
			params: ControlFrontierParams{
				AssigneeCandidates:       []string{cand},
				Routes:                   []string{route},
				RouteMetadataKeys:        []string{fkRunTarget, fkRoutedTo},
				InstantiatingMetadataKey: fkInstantiating,
				LimitPerTier:             1,
			},
		},
		{
			name: "routed-only-no-assignee",
			params: ControlFrontierParams{
				Routes:                   []string{route},
				RouteMetadataKeys:        []string{fkRoutedTo},
				InstantiatingMetadataKey: fkInstantiating,
			},
		},
		{
			name: "duplicate-route-deduped",
			params: ControlFrontierParams{
				Routes:                   []string{route, route},
				RouteMetadataKeys:        []string{fkRoutedTo},
				InstantiatingMetadataKey: fkInstantiating,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ControlFrontier(context.Background(), tc.params)
			if err != nil {
				t.Fatalf("ControlFrontier: %v", err)
			}
			want := controlFrontierOracle(allBeads, edges, tc.params, now)
			if idsOf(got) != idsOf(want) {
				t.Fatalf("frontier id/order mismatch\n got:  %v\n want: %v", idsOf(got), idsOf(want))
			}
		})
	}

	// --- Belt: independent hand-computed pins, so an oracle bug cannot hide a
	// SELECT bug. Each finding is pinned concretely. ---

	// Finding 4: assignee tier is `--sort priority` = priority ASC, created_at
	// DESC (newest first), id ASC. Among assignee-tier p0 beads the newest wins.
	assigneeOnly := ControlFrontierParams{AssigneeCandidates: []string{cand}, InstantiatingMetadataKey: fkInstantiating}
	gotAssignee, err := s.ControlFrontier(context.Background(), assigneeOnly)
	if err != nil {
		t.Fatalf("ControlFrontier(assignee): %v", err)
	}
	// p0 group sorts newest-first: gateNoKids(49), gateReleased(46), aLabeled(30),
	// aP0newer(20), aP0older(10). Then the p1 group. The belt below pins the
	// tie-break direction and the finding-specific inclusions/exclusions rather
	// than the whole list (the matrix cases pin the exact list against the oracle).
	assertOrderBefore(t, gotAssignee, aP0newer.ID, aP0older.ID) // same pri, newer first
	assertOrderBefore(t, gotAssignee, aLabeled.ID, aP0newer.ID)
	assertOrderBefore(t, gotAssignee, gateReleased.ID, aLabeled.ID) // p0 by newest still, 45>30
	assertOrderBefore(t, gotAssignee, aP0older.ID, aP1.ID)          // p0 group before p1 group
	assertContains(t, gotAssignee, aLabeled.ID)                     // finding 5: gc:session label NOT excluded
	assertContains(t, gotAssignee, aStep.ID)                        // finding 5: step type NOT excluded by bd
	assertContains(t, gotAssignee, danglingDep.ID)                  // finding 2: dangling dep NOT blocked
	assertContains(t, gotAssignee, unblockedByClosed.ID)            // closed blocking dep does not block
	assertContains(t, gotAssignee, gateReleased.ID)                 // finding 1: any-children early release
	assertContains(t, gotAssignee, gateNoKids.ID)                   // finding 1: no children => not gated
	assertAbsent(t, gotAssignee, blockedByOpen.ID)                  // finding 2: open blocking dep blocks
	assertAbsent(t, gotAssignee, gateBlocked.ID)                    // finding 1: open child gates
	assertAbsent(t, gotAssignee, gateAllBlocked.ID)                 // finding 1: all-children not released
	assertAbsent(t, gotAssignee, cascadeChild.ID)                   // finding 3: parent blocked cascades
	assertAbsent(t, gotAssignee, cascadeGrandchild.ID)              // finding 3: transitive cascade
	assertAbsent(t, gotAssignee, deferredParentChild.ID)            // finding 3: deferred-parent child
	assertAbsent(t, gotAssignee, blockedParent.ID)                  // parent itself blocked

	// Routed tier `--sort oldest`: rOld(1) before rNew(9); rtBoth surfaces under
	// run_target and is deduped out of routed_to.
	routedParams := ControlFrontierParams{
		Routes:                   []string{route},
		RouteMetadataKeys:        []string{fkRunTarget, fkRoutedTo},
		InstantiatingMetadataKey: fkInstantiating,
	}
	gotRouted, err := s.ControlFrontier(context.Background(), routedParams)
	if err != nil {
		t.Fatalf("ControlFrontier(routed): %v", err)
	}
	// run_target tier: rtBoth. routed_to tier oldest: rOld(1), rNew(9); rtBoth
	// already seen. So routed portion == [rtBoth, rOld, rNew].
	if want := join(rtBoth.ID, rOld.ID, rNew.ID); idsOf(gotRouted) != want {
		t.Fatalf("routed frontier shape mismatch\n got:  %v\n want: %v", idsOf(gotRouted), want)
	}
	assertAbsent(t, gotRouted, ephRouted.ID) // durable hides ephemeral

	// Ephemeral routed row present only under IncludeEphemeral.
	gotEph, err := s.ControlFrontier(context.Background(), cases[1].params)
	if err != nil {
		t.Fatalf("ControlFrontier(ephemeral): %v", err)
	}
	assertContains(t, gotEph, ephRouted.ID)
}

// TestControlFrontierArmBFrontierProjection pins Arm B: the frontier_route_order
// covering-index walk over fold_owned=1 rows. The façade cannot write fold-owned
// rows, so the test seeds a fold node + frontier row directly, then asserts
// ControlFrontier surfaces it and honors the future-defer cutoff.
func TestControlFrontierArmBFrontierProjection(t *testing.T) {
	s := newJournalTestStore(t)
	ctx := context.Background()
	const route = "fold-route"

	insertFoldFrontierNode(t, s, "gcg-jfold-ready", route, 1, "2026-01-01T00:00:00Z", nil)
	farFuture := journalNow().Add(24 * time.Hour).Format(time.RFC3339Nano)
	insertFoldFrontierNode(t, s, "gcg-jfold-deferred", route, 1, "2026-01-01T00:00:01Z", &farFuture)

	got, err := s.ControlFrontier(ctx, ControlFrontierParams{
		Routes:            []string{route},
		RouteMetadataKeys: []string{fkRoutedTo},
	})
	if err != nil {
		t.Fatalf("ControlFrontier: %v", err)
	}
	assertContains(t, got, "gcg-jfold-ready")
	assertAbsent(t, got, "gcg-jfold-deferred")
}

func join(ids ...string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

func containsID(beads []Bead, id string) bool {
	for _, b := range beads {
		if b.ID == id {
			return true
		}
	}
	return false
}

func assertContains(t *testing.T, beads []Bead, id string) {
	t.Helper()
	if !containsID(beads, id) {
		t.Fatalf("expected frontier to contain %s: %v", id, idsOf(beads))
	}
}

func assertAbsent(t *testing.T, beads []Bead, id string) {
	t.Helper()
	if containsID(beads, id) {
		t.Fatalf("expected frontier to exclude %s: %v", id, idsOf(beads))
	}
}

func assertOrderBefore(t *testing.T, beads []Bead, first, second string) {
	t.Helper()
	fi, si := -1, -1
	for i, b := range beads {
		if b.ID == first {
			fi = i
		}
		if b.ID == second {
			si = i
		}
	}
	if fi < 0 || si < 0 {
		t.Fatalf("order check: missing id (first=%d second=%d) in %v", fi, si, idsOf(beads))
	}
	if fi >= si {
		t.Fatalf("expected %s before %s, got order %v", first, second, idsOf(beads))
	}
}

// listAllFrontierBeads returns every seeded bead (open and closed) hydrated as
// ControlFrontier reads them, so the oracle sees identical field values.
func listAllFrontierBeads(t *testing.T, s *JournalStore) []Bead {
	t.Helper()
	beads, err := s.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	sort.Slice(beads, func(i, j int) bool { return beads[i].ID < beads[j].ID })
	return beads
}

// insertFoldFrontierNode writes a fold_owned=1 node and its frontier projection
// row directly (bypassing the write-closed façade) to exercise Arm B.
func insertFoldFrontierNode(t *testing.T, s *JournalStore, id, route string, readyPriority int, createdAt string, deferUntil *string) {
	t.Helper()
	ctx := context.Background()
	if err := s.withTx(ctx, func(tx *sql.Tx) error {
		// fold_owned=1 rows are write-closed (I-14); open the gate for this tx the
		// same way the executor's projection applier does (projection.go:142).
		if _, err := tx.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nodes (id, title, status, bead_type, created_at, updated_at, storage_tier, fold_owned, stream_id)
			VALUES (?, ?, 'open', 'task', ?, ?, 'history', 1, ?)`,
			id, id, createdAt, createdAt, "stream-"+id,
		); err != nil {
			return err
		}
		var deferArg any
		if deferUntil != nil {
			deferArg = *deferUntil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO frontier (node_id, root_id, route, ready_priority, created_at, id, defer_until)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, "root-"+id, route, readyPriority, createdAt, id, deferArg,
		); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 0 WHERE singleton = 0`)
		return err
	}); err != nil {
		t.Fatalf("insert fold frontier node %q: %v", id, err)
	}
}
