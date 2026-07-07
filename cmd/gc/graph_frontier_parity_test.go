//go:build integration

// This is the P3.1 Layer-2 parity gate: it proves, end to end against the REAL
// bd binary and the REAL rendered serve-tick shell, that for identical
// control-bead graph state the journal ControlFrontier and the legacy
// `bd ready | jq` pipeline produce the identical ordered id list. Green here is
// the precondition every later slice cites for GC_GRAPH_FRONTIER=serve.
//
// It replaces the P2.1 unit oracle (TestControlFrontierMatchesBDReadySemantics
// in internal/beads/journal_frontier_test.go, which models bd in Go) with bd
// itself. One typed fixture seeds two stores — a real bd store (via the bd CLI)
// and a JournalStore façade — then runs BOTH production frontier paths:
//
//   - legacy: workflowServeControlReadyQueryForBeads rendered verbatim and
//     executed via nextWorkflowServeBeads (the exact bd|jq pipeline the
//     dispatcher runs), against the bd store; and
//   - journal: ControlFrontier(controlFrontierInputs(...)) against the journal
//     store, using the PRODUCTION param mapping — so "who is being asked" is
//     derived by the same code in both legs.
//
// Location note: this lives in package main (cmd/gc), NOT test/integration, on
// purpose. The gate must call four production symbols verbatim —
// workflowServeControlReadyQueryForBeads, nextWorkflowServeBeads,
// controlFrontierInputs, and the hookBead shape — and those are package-main
// identifiers in cmd/gc. package integration cannot import package main, and
// P3.1 is a pure test addition (no production change to export them). cmd/gc
// already hosts integration-tagged tests (order_dynamic_integration_test.go,
// phase2_real_transport_test.go) and they run in the same `test-integration` /
// `test-integration-packages` lanes, so this is the idiomatic, forced home.
// Reimplementing the shell string in the test is explicitly out of bounds.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphstore"
)

const (
	parityRoute     = config.ControlDispatcherAgentName // "control-dispatcher": target, route, and assignee-tier identity
	parityKeeper    = "keeper"                          // owns blockers/spawners; never in the control-dispatcher frontier
	parityMissingID = "gcg-jdangling-missing"           // dangling dep target (no such bead) — journal only; bd forbids it
	parityBDPrefix  = "gcg"
)

// fxBead is one node of the typed fixture. One description seeds both stores, so
// the bd leg and the journal leg cannot drift by test-author error.
type fxBead struct {
	name          string
	typ           string // default "task"
	assignee      string
	priority      *int
	routedTo      string
	runTarget     string
	instantiating string
	labels        []string
	closed        bool
	ephemeral     bool
	deferFuture   bool
	rank          int // created_at ordering: created_at = base + rank minutes (distinct → no id tie-break)
}

// fxEdge is one dependency edge. dangling means the target is a nonexistent id
// (seeded on the journal leg only — bd rejects dangling deps by referential
// integrity, so on the bd leg the bead simply carries no dep; both legs then
// agree the bead is ready, which is the property under test).
type fxEdge struct {
	from     string
	to       string
	typ      string // "blocks" | "parent-child" | "waits-for"
	gate     string // waits-for gate: "all-children" | "any-children"
	dangling bool
}

func pInt(v int) *int { return &v }

// parityFixture returns the shape matrix mapped to the P2.1 bd-parity dimensions
// (journal_frontier.go findings 1-5 + the ephemeral/dedupe/exclude arms). Every
// dimension gets at least one row; the two frontiers must agree on all of it.
func parityFixture() ([]fxBead, []fxEdge) {
	cd := parityRoute
	beadsList := []fxBead{
		// Assignee tier: bd default --sort priority (priority ASC, created_at DESC). (finding 4)
		{name: "a_p0_older", assignee: cd, priority: pInt(0), rank: 10},
		{name: "a_p0_newer", assignee: cd, priority: pInt(0), rank: 20},
		{name: "a_p1", assignee: cd, priority: pInt(1), rank: 5},
		{name: "a_labeled", assignee: cd, priority: pInt(0), labels: []string{"gc:session"}, rank: 30}, // finding 5: label NOT excluded
		{name: "a_step", typ: "step", assignee: cd, priority: pInt(1), rank: 31},                       // finding 5/6: step type present
		{name: "a_other", assignee: "someone-else", rank: 11},                                          // assignee miss
		{name: "a_wfc", assignee: "workflow-control", rank: 32},                                        // legacy assignee alias (finding 7)

		// Routed tier: --unassigned + --metadata-field, --sort oldest. (finding 9)
		{name: "r_old", routedTo: cd, rank: 1},
		{name: "r_new", routedTo: cd, priority: pInt(0), rank: 9}, // priority irrelevant under --sort oldest
		{name: "r_miss", routedTo: "other-route", rank: 2},
		{name: "r_assigned", assignee: "w", routedTo: cd, rank: 3},   // assigned → not in --unassigned tier
		{name: "rt_both", runTarget: cd, routedTo: cd, rank: 4},      // run_target tier before routed_to; first-wins dedupe
		{name: "r_legacy", routedTo: "workflow-control", rank: 33},   // legacy route tier (finding 7)
		{name: "eph_routed", routedTo: cd, ephemeral: true, rank: 0}, // ephemeral: hidden unless BD105 (finding 8)

		// Exclude set (finding 6): must never appear.
		{name: "ex_epic", typ: "epic", assignee: cd, rank: 6},
		{name: "ex_message", typ: "message", assignee: cd, rank: 7},
		{name: "ex_molecule", typ: "molecule", assignee: cd, rank: 8},
		{name: "ex_gate", typ: "gate", assignee: cd, rank: 34},
		{name: "ex_rig", typ: "rig", assignee: cd, rank: 35},

		// Non-ready arms.
		{name: "closed_bead", assignee: cd, closed: true, rank: 12},
		{name: "deferred_bead", assignee: cd, deferFuture: true, rank: 13},
		{name: "instantiating_bead", assignee: cd, instantiating: "root-x", rank: 14}, // dropped by dedupe

		// blocks / dangling (finding 2).
		{name: "open_target", assignee: parityKeeper, rank: 15},
		{name: "closed_target", assignee: parityKeeper, closed: true, rank: 16},
		{name: "blocked_by_open", assignee: cd, rank: 17},                        // BLOCKED by open dep
		{name: "unblocked_by_closed", assignee: cd, priority: pInt(1), rank: 18}, // closed dep does not block
		// dangling dep does not block. Seeded ONLY on the journal leg: bd's
		// dependencies.depends_on_issue_id carries an FK to issues (ON DELETE
		// CASCADE), so bd cannot hold a dangling issue-dep at all — purging the
		// target cascade-deletes the edge. The journal edge exercises
		// ControlFrontier's INNER-JOIN (dangling→not-blocked) branch, and bd agrees
		// on the OUTCOME (the bead is ready) because it structurally never has one.
		{name: "dangling_dep", assignee: cd, priority: pInt(1), rank: 19},

		// waits-for gate (finding 1). Spawners are open; the gate reads the
		// spawner's parent-child children, not the spawner's own status.
		{name: "spawner_blocking", assignee: parityKeeper, rank: 40},
		{name: "s2_kid", assignee: parityKeeper, rank: 41},
		{name: "gate_blocked", assignee: cd, rank: 42}, // all-children, open child → BLOCKED
		{name: "spawner_mixed", assignee: parityKeeper, rank: 43},
		{name: "s1_open", assignee: parityKeeper, rank: 44},
		{name: "s1_closed", assignee: parityKeeper, closed: true, rank: 45},
		{name: "gate_released", assignee: cd, priority: pInt(0), rank: 46}, // any-children, one child closed → ready
		{name: "gate_all_blocked", assignee: cd, rank: 47},                 // all-children, open child remains → BLOCKED
		{name: "spawner_no_kids", assignee: parityKeeper, rank: 48},
		{name: "gate_no_kids", assignee: cd, priority: pInt(0), rank: 49}, // no children → ready

		// parent-child blocked cascade, transitive (finding 3).
		{name: "cascade_blocked_parent", assignee: parityKeeper, rank: 50},
		{name: "cascade_child", assignee: cd, rank: 51},
		{name: "cascade_grandchild", assignee: cd, rank: 52},

		// deferred-parent children excluded, single hop (finding 4).
		{name: "deferred_parent", assignee: parityKeeper, deferFuture: true, rank: 53},
		{name: "deferred_parent_child", assignee: cd, rank: 54},
		// Grandchild of a deferred parent is PRESENT: bd's exclusion is single-hop
		// (ready_work.go getChildrenOfDeferredParentsInTx), so a bug making it
		// transitive would drop this and diverge.
		{name: "deferred_grandchild", assignee: cd, priority: pInt(1), rank: 55},

		// nil-priority default sort placement: an unset priority stores as 2 in bd
		// and maps nil→2 in ControlFrontier, so this sorts AFTER the p1 group. A
		// wrong default (nil→0) would hoist it into the p0 group.
		{name: "a_nil", assignee: cd, rank: 60},

		// Complete the exclude set: agent/role (DefaultInfraTypes) and merge-request
		// (ReadyWorkExcludeTypes). merge-request cannot be minted by `bd create`, so
		// the bd leg seeds it as a raw issues row (seedBDStore).
		{name: "ex_agent", typ: "agent", assignee: cd, rank: 36},
		{name: "ex_role", typ: "role", assignee: cd, rank: 37},
		{name: "ex_merge_request", typ: "merge-request", assignee: cd, rank: 38},
	}
	// Per-tier LIMIT (workflowServeScanLimit = 20) must BIND, or a capTierBeads bug
	// (global cap, cap-before-sort, off-by-one) passes invisibly. Route 21 beads to
	// the workflow-control routed_to tier — which otherwise holds only r_legacy —
	// giving 22 candidates. --sort oldest --limit=20 keeps r_legacy (oldest) plus
	// lim00..lim18 and cuts the two newest (lim19, lim20); parity adjudicates WHICH
	// 20 survive and in what order against bd's SQL LIMIT.
	for i := 0; i <= 20; i++ {
		beadsList = append(beadsList, fxBead{
			name:     fmt.Sprintf("lim%02d", i),
			routedTo: "workflow-control",
			rank:     100 + i,
		})
	}
	edges := []fxEdge{
		{from: "blocked_by_open", to: "open_target", typ: "blocks"},
		{from: "unblocked_by_closed", to: "closed_target", typ: "blocks"},
		{from: "dangling_dep", to: parityMissingID, typ: "blocks", dangling: true},
		{from: "s2_kid", to: "spawner_blocking", typ: "parent-child"},
		{from: "gate_blocked", to: "spawner_blocking", typ: "waits-for", gate: "all-children"},
		{from: "s1_open", to: "spawner_mixed", typ: "parent-child"},
		{from: "s1_closed", to: "spawner_mixed", typ: "parent-child"},
		{from: "gate_released", to: "spawner_mixed", typ: "waits-for", gate: "any-children"},
		{from: "gate_all_blocked", to: "spawner_mixed", typ: "waits-for", gate: "all-children"},
		{from: "gate_no_kids", to: "spawner_no_kids", typ: "waits-for", gate: "all-children"},
		{from: "cascade_blocked_parent", to: "open_target", typ: "blocks"},
		{from: "cascade_child", to: "cascade_blocked_parent", typ: "parent-child"},
		{from: "cascade_grandchild", to: "cascade_child", typ: "parent-child"},
		{from: "deferred_parent_child", to: "deferred_parent", typ: "parent-child"},
		{from: "deferred_grandchild", to: "deferred_parent_child", typ: "parent-child"},
	}
	return beadsList, edges
}

// parityToolset resolves the real bd + dolt binaries and skips cleanly when the
// integration toolchain is absent, matching the test/integration convention
// (GC_INTEGRATION_REAL_BD override, else PATH lookup, else skip).
type parityToolset struct {
	bd   string
	dolt string
}

func resolveParityToolset(t *testing.T) parityToolset {
	t.Helper()
	// The cmd/gc test harness (main_test.go) clears GC_INTEGRATION_* env and
	// prepends a fake `bd` shim to PATH, so we cannot simply trust the env var or
	// the first PATH match. resolveRealTool validates each candidate by its
	// `version` output so a testscript stub is never mistaken for the real tool.
	bd := resolveRealTool(t, "bd", "GC_INTEGRATION_REAL_BD")
	dolt := resolveRealTool(t, "dolt", "GC_DOLT_REAL_BINARY")
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("graph frontier parity: no jq binary (the serve-tick shell needs it)")
	}
	return parityToolset{bd: bd, dolt: dolt}
}

// resolveRealTool finds a genuine external binary named `name`, tolerant of the
// cmd/gc test harness. It prefers the caller/harness-set env override
// (GC_INTEGRATION_REAL_BD / GC_DOLT_REAL_BINARY — the latter survives the
// harness's env scrub), then well-known install dirs, then PATH; well-known
// dirs are probed before PATH so the real binary is found without ever having to
// execute the PATH-prepended stub on a normal box.
func resolveRealTool(t *testing.T, name, envKey string) string {
	t.Helper()
	var candidates []string
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		candidates = append(candidates, v)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", name), filepath.Join(home, "go", "bin", name))
	}
	candidates = append(candidates, "/usr/local/bin/"+name, "/usr/bin/"+name)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	seen := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if isRealTool(c, name) {
			return c
		}
	}
	t.Skipf("graph frontier parity: no real %s binary found (set %s or install %s)", name, envKey, name)
	return ""
}

func isRealTool(path, name string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), name+" version")
}

// parityBase is the created_at anchor; created_at = base + rank minutes so every
// fixture bead has a distinct second-resolution timestamp (bd stores created_at
// at second resolution and breaks ties by id — distinct times keep the sort
// determined by (priority, created_at) alone, so bd-id vs journal-id never
// influences order).
var parityBase = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func parityCreatedAt(rank int) time.Time { return parityBase.Add(time.Duration(rank) * time.Minute) }

var parityDeferFuture = time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)

func TestControlFrontierParityAgainstRealBd(t *testing.T) {
	tools := resolveParityToolset(t)
	beadsList, edges := parityFixture()

	root := t.TempDir()
	doltHome := filepath.Join(root, "dolthome")
	bdDir := filepath.Join(root, "bd")
	journalDir := filepath.Join(root, "journal")
	for _, d := range []string{doltHome, bdDir, journalDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	nameToBD := seedBDStore(t, tools, bdDir, doltHome, beadsList, edges)
	journal, nameToJournal := seedJournalStore(t, journalDir, beadsList, edges)

	bdToName := invertIDMap(nameToBD)
	journalToName := invertIDMap(nameToJournal)

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName} // QualifiedName() == "control-dispatcher"
	const controlSessionName = ""                                     // no distinct session identity: keep the candidate list tight
	shellEnv := parityShellEnv(tools.bd, bdDir, doltHome)
	envLookup := func(key string) string { return shellEnv[key] }

	// The full ordered frontier is one comparison; the ephemeral dimension is the
	// matrix axis (BD105 ⇒ --include-ephemeral ⇒ TierBoth).
	cases := []struct {
		name             string
		beadsCfg         config.BeadsConfig
		wantEphemeralRow bool
	}{
		{name: "durable", beadsCfg: config.BeadsConfig{}, wantEphemeralRow: false},
		{name: "include-ephemeral", beadsCfg: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}, wantEphemeralRow: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacyNames := runLegacyFrontier(t, agentCfg, tc.beadsCfg, controlSessionName, bdDir, shellEnv, bdToName)
			journalNames := runJournalFrontier(t, journal, agentCfg, tc.beadsCfg, controlSessionName, envLookup, journalToName)

			if !equalStringSlices(legacyNames, journalNames) {
				t.Fatalf("FRONTIER PARITY DIVERGENCE (%s): ControlFrontier != bd ready|jq for identical state\n  bd:      %v\n  journal: %v\n  only-in-bd:      %v\n  only-in-journal: %v",
					tc.name, legacyNames, journalNames,
					sliceDiff(legacyNames, journalNames), sliceDiff(journalNames, legacyNames))
			}

			// Belts: the comparison must not pass vacuously. Pin each dimension's
			// intended outcome against the (already-equal) journal result.
			assertContainsName(t, journalNames, "gate_released")       // finding 1: any-children early release
			assertContainsName(t, journalNames, "gate_no_kids")        // finding 1: spawner with no children
			assertContainsName(t, journalNames, "dangling_dep")        // finding 2: dangling dep does not block
			assertContainsName(t, journalNames, "unblocked_by_closed") // closed dep does not block
			assertContainsName(t, journalNames, "a_labeled")           // finding 5: label not excluded
			assertContainsName(t, journalNames, "a_step")              // finding 6: step type present
			assertContainsName(t, journalNames, "a_wfc")               // finding 7: legacy assignee alias
			assertContainsName(t, journalNames, "r_legacy")            // finding 7: legacy route tier
			assertContainsName(t, journalNames, "rt_both")             // run_target tier

			assertAbsentName(t, journalNames, "gate_blocked")          // finding 1: open child gates
			assertAbsentName(t, journalNames, "gate_all_blocked")      // finding 1: all-children not released
			assertAbsentName(t, journalNames, "blocked_by_open")       // finding 2: open dep blocks
			assertAbsentName(t, journalNames, "cascade_child")         // finding 3: cascade
			assertAbsentName(t, journalNames, "cascade_grandchild")    // finding 3: transitive cascade
			assertAbsentName(t, journalNames, "deferred_parent_child") // finding 4: deferred-parent child
			assertAbsentName(t, journalNames, "deferred_bead")         // own future defer
			assertAbsentName(t, journalNames, "closed_bead")           // closed
			assertAbsentName(t, journalNames, "instantiating_bead")    // dedupe drop
			assertAbsentName(t, journalNames, "ex_epic")               // exclude set
			assertAbsentName(t, journalNames, "ex_message")
			assertAbsentName(t, journalNames, "ex_molecule")
			assertAbsentName(t, journalNames, "ex_gate")
			assertAbsentName(t, journalNames, "ex_rig")
			assertAbsentName(t, journalNames, "ex_agent")         // exclude set: DefaultInfraTypes
			assertAbsentName(t, journalNames, "ex_role")          // exclude set: DefaultInfraTypes
			assertAbsentName(t, journalNames, "ex_merge_request") // exclude set: ReadyWorkExcludeTypes
			assertAbsentName(t, journalNames, "r_miss")           // wrong route value
			assertAbsentName(t, journalNames, "r_assigned")       // routed tier is --unassigned

			// Deferred-parent exclusion is single-hop: the grandchild is READY.
			assertContainsName(t, journalNames, "deferred_grandchild")

			// nil-priority default sort: nil→2 places a_nil AFTER the p1 group; a
			// wrong nil→0 default would hoist it before a_p1.
			assertContainsName(t, journalNames, "a_nil")
			assertOrderName(t, journalNames, "a_p1", "a_nil")

			// Per-tier LIMIT binds: the workflow-control routed_to tier has 22
			// candidates; --sort oldest --limit=20 keeps r_legacy + lim00..lim18 and
			// cuts the two newest. Adjudicate WHICH survive and their order.
			assertContainsName(t, journalNames, "lim00") // oldest lim, survives
			assertContainsName(t, journalNames, "lim18") // 20th row, last surviving
			assertAbsentName(t, journalNames, "lim19")   // cut by the per-tier LIMIT
			assertAbsentName(t, journalNames, "lim20")   // cut by the per-tier LIMIT
			assertOrderName(t, journalNames, "r_legacy", "lim00")
			assertOrderName(t, journalNames, "lim00", "lim18")

			// finding 4: assignee tier priority ASC, created_at DESC (newest first).
			assertOrderName(t, journalNames, "a_p0_newer", "a_p0_older")
			assertOrderName(t, journalNames, "a_p0_older", "a_p1") // p0 group before p1 group
			// finding 7 / tier order: run_target before routed_to; routed --sort oldest.
			assertOrderName(t, journalNames, "rt_both", "r_old")
			assertOrderName(t, journalNames, "r_old", "r_new")

			// Ephemeral (finding 8): present only under BD105 include-ephemeral, and
			// oldest (rank 0) so it leads the routed_to tier for the route.
			if tc.wantEphemeralRow {
				assertContainsName(t, journalNames, "eph_routed")
				assertOrderName(t, journalNames, "eph_routed", "r_old")
			} else {
				assertAbsentName(t, journalNames, "eph_routed")
			}
		})
	}

	// Dimension 11: Arm B contributes nothing for façade-written (fold_owned=0)
	// state — every journal frontier id maps to a fixture name, no fold rows.
	t.Run("arm-b-empty-on-facade-state", func(t *testing.T) {
		names := runJournalFrontier(t, journal, agentCfg, config.BeadsConfig{}, controlSessionName, envLookup, journalToName)
		for _, n := range names {
			if n == "" {
				t.Fatalf("journal frontier surfaced an unmapped (Arm-B/phantom) id: %v", names)
			}
		}
	})

	// Dimension 10: the stale denormalized is_blocked adjudication arm.
	t.Run("stale-is-blocked-adjudication", func(t *testing.T) {
		runStaleIsBlockedArm(t, tools, bdDir, doltHome, journal,
			agentCfg, controlSessionName, shellEnv, envLookup,
			nameToBD, bdToName, journalToName)
	})
}

// runStaleIsBlockedArm pins that ControlFrontier is adjudicated against bd AFTER
// a recompute: bd's ready filters the denormalized stored is_blocked column,
// which can lag (until `bd recompute-blocked`), while ControlFrontier computes
// blockedness live. We (1) document the known pre-recompute divergence in a
// non-gating check, then (2) require parity restored post-recompute (gating).
func runStaleIsBlockedArm(
	t *testing.T,
	tools parityToolset,
	bdDir, doltHome string,
	journal *beads.JournalStore,
	agentCfg config.Agent,
	controlSessionName string,
	shellEnv map[string]string,
	envLookup func(string) string,
	nameToBD, bdToName, journalToName map[string]string,
) {
	target := nameToBD["a_p1"] // a currently-ready assignee-tier bead
	if target == "" {
		t.Fatal("stale arm: a_p1 not seeded in bd")
	}

	beadsCfg := config.BeadsConfig{}
	before := runJournalFrontier(t, journal, agentCfg, beadsCfg, controlSessionName, envLookup, journalToName)
	if !containsName(before, "a_p1") {
		t.Fatalf("stale arm precondition: a_p1 not in journal frontier: %v", before)
	}

	// Force the stored flag stale WITHOUT committing: bd ready reads the working
	// set, so it now hides a_p1; the journal (live) still shows it.
	doltExec(t, tools.dolt, bdDir, doltHome,
		fmt.Sprintf("USE %s; UPDATE issues SET is_blocked=1 WHERE id='%s';", parityBDPrefix, target))

	staleLegacy := runLegacyFrontier(t, agentCfg, beadsCfg, controlSessionName, bdDir, shellEnv, bdToName)
	staleJournal := runJournalFrontier(t, journal, agentCfg, beadsCfg, controlSessionName, envLookup, journalToName)
	// Non-gating: document that pre-recompute bd is staler than ControlFrontier.
	// If bd did not hide it (e.g. a future bd recomputes on read), the runbook
	// note is simply reaffirmed rather than failing the gate.
	switch {
	case containsName(staleLegacy, "a_p1"):
		t.Logf("stale arm (non-gating): bd still shows a_p1 with is_blocked=1 stored; bd may recompute on read in this build")
	case !containsName(staleJournal, "a_p1"):
		t.Logf("stale arm (non-gating): ControlFrontier also hid a_p1 unexpectedly (journal=%v)", staleJournal)
	default:
		t.Logf("stale arm (non-gating): confirmed bd hides a_p1 on stale is_blocked while ControlFrontier keeps it live")
	}

	// Commit the working set (recompute-blocked requires it), then recompute so
	// bd's stored flag is fresh — this is the state parity is DEFINED against.
	doltExec(t, tools.dolt, bdDir, doltHome,
		fmt.Sprintf("USE %s; CALL DOLT_COMMIT('-Am','stale-arm tamper');", parityBDPrefix))
	runBD(t, tools.bd, bdDir, doltHome, "recompute-blocked")

	postLegacy := runLegacyFrontier(t, agentCfg, beadsCfg, controlSessionName, bdDir, shellEnv, bdToName)
	postJournal := runJournalFrontier(t, journal, agentCfg, beadsCfg, controlSessionName, envLookup, journalToName)
	if !equalStringSlices(postLegacy, postJournal) {
		t.Fatalf("stale arm: post-recompute parity divergence\n  bd:      %v\n  journal: %v", postLegacy, postJournal)
	}
	if !containsName(postLegacy, "a_p1") {
		t.Fatalf("stale arm: a_p1 not restored to bd frontier after recompute-blocked: %v", postLegacy)
	}
}

// ---- legacy leg: the REAL rendered serve shell over the bd store ----

func runLegacyFrontier(t *testing.T, agentCfg config.Agent, beadsCfg config.BeadsConfig, controlSessionName, bdDir string, shellEnv map[string]string, bdToName map[string]string) []string {
	t.Helper()
	query := workflowServeControlReadyQueryForBeads(agentCfg, beadsCfg, controlSessionName)
	rows, err := nextWorkflowServeBeads(query, bdDir, shellEnv)
	if err != nil {
		t.Fatalf("legacy frontier: nextWorkflowServeBeads: %v", err)
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		name, ok := bdToName[r.ID]
		if !ok {
			t.Fatalf("legacy frontier returned unmapped bd id %q (rows=%v)", r.ID, hookBeadIDs(rows))
		}
		names = append(names, name)
	}
	return names
}

func hookBeadIDs(rows []hookBead) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// ---- journal leg: ControlFrontier via the PRODUCTION param mapping ----

func runJournalFrontier(t *testing.T, journal *beads.JournalStore, agentCfg config.Agent, beadsCfg config.BeadsConfig, controlSessionName string, envLookup func(string) string, journalToName map[string]string) []string {
	t.Helper()
	params := controlFrontierInputs(agentCfg, beadsCfg, controlSessionName, envLookup)
	frontier, ok := beads.ControlFrontierStoreFor(journal)
	if !ok {
		t.Fatal("journal frontier: store does not expose ControlFrontier capability")
	}
	rows, err := frontier.ControlFrontier(context.Background(), params)
	if err != nil {
		t.Fatalf("journal frontier: ControlFrontier: %v", err)
	}
	names := make([]string, 0, len(rows))
	for _, b := range rows {
		name, ok := journalToName[b.ID]
		if !ok {
			t.Fatalf("journal frontier returned unmapped id %q", b.ID)
		}
		names = append(names, name)
	}
	return names
}

// ---- seeding: bd leg ----

func seedBDStore(t *testing.T, tools parityToolset, bdDir, doltHome string, beadsList []fxBead, edges []fxEdge) map[string]string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(doltHome, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir dolt home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(doltHome, ".dolt", "config_global.json"),
		[]byte(`{"user.name":"gc-test","user.email":"gc-test@test.local"}`), 0o644); err != nil {
		t.Fatalf("write dolt global config: %v", err)
	}
	parityRunGit(t, bdDir, "init", "--quiet")
	runBD(t, tools.bd, bdDir, doltHome, "init", "-p", parityBDPrefix, "--skip-hooks", "--skip-agents", "--non-interactive")
	runBD(t, tools.bd, bdDir, doltHome, "config", "set", "types.custom", "step,convoy,molecule,rig,session,role,agent")

	waitsFor := waitsForByChild(edges)
	nameToID := make(map[string]string, len(beadsList))

	for _, b := range byRank(beadsList) {
		if b.typ == "merge-request" {
			// bd create rejects merge-request; seed it as a real issues row so bd's
			// type-based ready exclusion is genuinely exercised on the bd leg.
			id := seedBDRawIssue(t, tools, bdDir, doltHome, b)
			nameToID[b.name] = id
			continue
		}
		args := []string{"create", b.name, "--silent"}
		if b.typ != "" && b.typ != "task" {
			args = append(args, "--type", b.typ)
		}
		if b.assignee != "" {
			args = append(args, "--assignee", b.assignee)
		}
		if b.priority != nil {
			args = append(args, "-p", strconv.Itoa(*b.priority))
		}
		if md := beadMetadataJSON(b); md != "" {
			args = append(args, "--metadata", md)
		}
		if len(b.labels) > 0 {
			args = append(args, "--labels", strings.Join(b.labels, ","))
		}
		if b.ephemeral {
			args = append(args, "--ephemeral")
		}
		if b.deferFuture {
			args = append(args, "--defer", parityDeferFuture.Format("2006-01-02"))
		}
		if wf, ok := waitsFor[b.name]; ok {
			spawnerID := nameToID[wf.to]
			if spawnerID == "" {
				t.Fatalf("bd seed: waits-for spawner %q for %q created out of order", wf.to, b.name)
			}
			gate := wf.gate
			if gate == "" {
				gate = "all-children"
			}
			args = append(args, "--waits-for", spawnerID, "--waits-for-gate", gate)
		}
		out := runBD(t, tools.bd, bdDir, doltHome, args...)
		id := strings.TrimSpace(lastNonEmptyLine(out))
		if !strings.HasPrefix(id, parityBDPrefix+"-") {
			t.Fatalf("bd create %q: unexpected id output %q", b.name, out)
		}
		nameToID[b.name] = id
	}

	// Non-waits-for edges (waits-for was applied at create; dangling is journal-only).
	for _, e := range edges {
		if e.typ == "waits-for" || e.dangling {
			continue
		}
		fromID, toID := nameToID[e.from], nameToID[e.to]
		if fromID == "" || toID == "" {
			t.Fatalf("bd seed edge %s->%s: missing id (%q/%q)", e.from, e.to, fromID, toID)
		}
		runBD(t, tools.bd, bdDir, doltHome, "dep", "add", fromID, toID, "--type", e.typ)
	}

	// Close the closed beads (after deps so the gate/cascade see final status).
	for _, b := range beadsList {
		if b.closed {
			runBD(t, tools.bd, bdDir, doltHome, "close", nameToID[b.name])
		}
	}

	// Stamp created_at to controlled, distinct instants, then commit the whole
	// working set (bd auto-commit is off) so bd ready sees fresh state and
	// recompute-blocked can run against a clean set.
	var sb strings.Builder
	fmt.Fprintf(&sb, "USE %s;", parityBDPrefix)
	for _, b := range beadsList {
		// Ephemeral beads are wisps in a separate table; stamp created_at where bd
		// actually reads it so the merged --include-ephemeral --sort oldest order
		// is deterministic (bd merge-sorts wisps and issues by created_at).
		table := "issues"
		if b.ephemeral {
			table = "wisps"
		}
		fmt.Fprintf(&sb, " UPDATE %s SET created_at='%s' WHERE id='%s';",
			table, parityCreatedAt(b.rank).Format("2006-01-02 15:04:05"), nameToID[b.name])
	}
	fmt.Fprintf(&sb, " CALL DOLT_COMMIT('-Am','parity seed');")
	doltExec(t, tools.dolt, bdDir, doltHome, sb.String())
	runBD(t, tools.bd, bdDir, doltHome, "recompute-blocked")

	return nameToID
}

// seedBDRawIssue inserts a real issues row directly, for types `bd create`
// refuses (e.g. merge-request), so bd's type-based ready exclusion is exercised
// against a genuine row rather than a strawman absence. The uncommitted
// working-set write is committed by the created_at stamp's DOLT_COMMIT and then
// re-stamped there, so it participates in the same fresh, committed snapshot.
func seedBDRawIssue(t *testing.T, tools parityToolset, bdDir, doltHome string, b fxBead) string {
	t.Helper()
	id := parityBDPrefix + "-raw" + strings.ReplaceAll(b.name, "_", "")
	status := "open"
	if b.closed {
		status = "closed"
	}
	ts := parityCreatedAt(b.rank).Format("2006-01-02 15:04:05")
	q := fmt.Sprintf("USE %s; INSERT INTO issues "+
		"(id, title, description, design, acceptance_criteria, notes, status, issue_type, assignee, priority, created_by, created_at, updated_at) "+
		"VALUES ('%s','%s','','','','','%s','%s','%s',2,'parity','%s','%s');",
		parityBDPrefix, id, b.name, status, b.typ, b.assignee, ts, ts)
	doltExec(t, tools.dolt, bdDir, doltHome, q)
	return id
}

// ---- seeding: journal leg ----

func seedJournalStore(t *testing.T, journalDir string, beadsList []fxBead, edges []fxEdge) (*beads.JournalStore, map[string]string) {
	t.Helper()
	gs, err := graphstore.Open(context.Background(), filepath.Join(journalDir, "journal.db"), graphstore.Options{})
	if err != nil {
		t.Fatalf("journal seed: graphstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	js := beads.NewJournalStore(gs)

	nameToID := make(map[string]string, len(beadsList))
	for _, b := range beadsList {
		bead := beads.Bead{
			Title:     b.name,
			Type:      parityFirstNonEmpty(b.typ, "task"),
			Assignee:  b.assignee,
			Priority:  b.priority,
			Ephemeral: b.ephemeral,
			CreatedAt: parityCreatedAt(b.rank),
			Metadata:  beadStringMap(b),
			Labels:    b.labels,
		}
		if b.closed {
			bead.Status = "closed"
		}
		if b.deferFuture {
			d := parityDeferFuture
			bead.DeferUntil = &d
		}
		created, err := js.Create(bead)
		if err != nil {
			t.Fatalf("journal seed create %q: %v", b.name, err)
		}
		nameToID[b.name] = created.ID
	}

	// Edges: blocks / parent-child / all-children waits-for (empty gate metadata)
	// go through DepAdd; dangling targets a nonexistent id (edges FK from_id only).
	var anyChildrenEdges []beads.GraphApplyEdge
	for _, e := range edges {
		fromID := nameToID[e.from]
		if fromID == "" {
			t.Fatalf("journal seed edge %s->%s: missing from id", e.from, e.to)
		}
		toID := e.to
		if !e.dangling {
			toID = nameToID[e.to]
			if toID == "" {
				t.Fatalf("journal seed edge %s->%s: missing to id", e.from, e.to)
			}
		}
		if e.typ == "waits-for" && e.gate == "any-children" {
			// any-children early release requires explicit {"gate":"any-children"}
			// edge metadata, which only the graph-apply path can carry.
			anyChildrenEdges = append(anyChildrenEdges, beads.GraphApplyEdge{
				FromID:   fromID,
				ToID:     toID,
				Type:     "waits-for",
				Metadata: `{"gate":"any-children"}`,
			})
			continue
		}
		if err := js.DepAdd(fromID, toID, e.typ); err != nil {
			t.Fatalf("journal seed DepAdd %s->%s (%s): %v", e.from, e.to, e.typ, err)
		}
	}

	if len(anyChildrenEdges) > 0 {
		// ApplyGraphPlan requires ≥1 node; carry the any-children edges on an
		// excluded-type phantom that no tier can surface (molecule, no assignee,
		// no route). The edges reference the real beads by concrete id.
		plan := &beads.GraphApplyPlan{
			Nodes: []beads.GraphApplyNode{{Key: "gate-carrier", Title: "gate metadata carrier", Type: "molecule"}},
			Edges: anyChildrenEdges,
		}
		if _, err := js.ApplyGraphPlan(context.Background(), plan); err != nil {
			t.Fatalf("journal seed any-children gate edges: %v", err)
		}
	}

	return js, nameToID
}

// ---- fixture helpers ----

func waitsForByChild(edges []fxEdge) map[string]fxEdge {
	out := make(map[string]fxEdge)
	for _, e := range edges {
		if e.typ == "waits-for" {
			out[e.from] = e
		}
	}
	return out
}

func byRank(in []fxBead) []fxBead {
	out := append([]fxBead(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].rank < out[j].rank })
	return out
}

func beadMetadataMap(b fxBead) map[string]string {
	md := map[string]string{}
	if b.routedTo != "" {
		md[beadmeta.RoutedToMetadataKey] = b.routedTo
	}
	if b.runTarget != "" {
		md[beadmeta.RunTargetMetadataKey] = b.runTarget
	}
	if b.instantiating != "" {
		md[beadmeta.InstantiatingMetadataKey] = b.instantiating
	}
	return md
}

func beadMetadataJSON(b fxBead) string {
	md := beadMetadataMap(b)
	if len(md) == 0 {
		return ""
	}
	raw, err := json.Marshal(md)
	if err != nil {
		return ""
	}
	return string(raw)
}

func beadStringMap(b fxBead) beads.StringMap {
	md := beadMetadataMap(b)
	if len(md) == 0 {
		return nil
	}
	out := make(beads.StringMap, len(md))
	for k, v := range md {
		out[k] = v
	}
	return out
}

// ---- exec + env helpers ----

func parityShellEnv(bdBinary, bdDir, doltHome string) map[string]string {
	return map[string]string{
		"HOME":               doltHome,
		"DOLT_ROOT_PATH":     doltHome,
		"PATH":               filepath.Dir(bdBinary) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BEADS_DIR":          filepath.Join(bdDir, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"CI":                 "true",
		// Pin the assignee/session identity levers so both legs derive the same
		// candidate list regardless of the ambient test environment.
		"GC_SESSION_NAME": "",
		"GC_ALIAS":        "",
		"GC_SESSION_ID":   "",
	}
}

// parityBDEnv builds a clean env for direct bd/dolt invocations. It strips every
// inherited HOME/DOLT/BEADS/GC/BD key first, then sets the overrides, so the
// child cannot read the developer's real ~/.dolt or global bd config (a
// duplicate HOME entry would otherwise leave glibc resolving the inherited one,
// which reroutes bd to a foreign store).
func parityBDEnv(bdDir, doltHome string) []string {
	out := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if parityStripEnvKey(kv[:eq]) {
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"HOME="+doltHome,
		"DOLT_ROOT_PATH="+doltHome,
		// Pin the store explicitly: t.TempDir() may sit under a TMPDIR that has a
		// foreign .beads ancestor, which bd's upward workspace discovery would
		// otherwise resolve instead of ours.
		"BEADS_DIR="+filepath.Join(bdDir, ".beads"),
		"BD_NON_INTERACTIVE=1",
		"CI=true",
		"BD_EXPORT_AUTO=false",
	)
}

func parityStripEnvKey(key string) bool {
	switch key {
	case "HOME", "DOLT_ROOT_PATH", "BD_NON_INTERACTIVE", "CI", "BD_EXPORT_AUTO":
		return true
	}
	return strings.HasPrefix(key, "BEADS") ||
		strings.HasPrefix(key, "GC_") ||
		strings.HasPrefix(key, "DOLT_") ||
		strings.HasPrefix(key, "BD_")
}

func runBD(t *testing.T, bdBinary, dir, doltHome string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bdBinary, args...)
	cmd.Dir = dir
	cmd.Env = parityBDEnv(dir, doltHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func doltExec(t *testing.T, doltBinary, bdDir, doltHome, query string) {
	t.Helper()
	embedded := filepath.Join(bdDir, ".beads", "embeddeddolt")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltBinary, "sql", "-q", query)
	cmd.Dir = embedded
	cmd.Env = parityBDEnv(bdDir, doltHome)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt sql: %v\n%s", err, out)
	}
}

func parityRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// ---- small utilities ----

func parityFirstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

func invertIDMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, id := range in {
		out[id] = name
	}
	return out
}

func equalStringSlices(a, b []string) bool {
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

func sliceDiff(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	var out []string
	for _, v := range a {
		if !set[v] {
			out = append(out, v)
		}
	}
	return out
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func assertContainsName(t *testing.T, names []string, want string) {
	t.Helper()
	if !containsName(names, want) {
		t.Fatalf("expected frontier to contain %q: %v", want, names)
	}
}

func assertAbsentName(t *testing.T, names []string, want string) {
	t.Helper()
	if containsName(names, want) {
		t.Fatalf("expected frontier to exclude %q: %v", want, names)
	}
}

func assertOrderName(t *testing.T, names []string, first, second string) {
	t.Helper()
	fi, si := -1, -1
	for i, n := range names {
		if n == first {
			fi = i
		}
		if n == second {
			si = i
		}
	}
	if fi < 0 || si < 0 {
		t.Fatalf("order check: missing %q(%d)/%q(%d) in %v", first, fi, second, si, names)
	}
	if fi >= si {
		t.Fatalf("expected %q before %q, got %v", first, second, names)
	}
}
