package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// Phase-C exercises the Tier-B hook claim leg against a real graph-scoped temp
// city: a fold-projected pool row is claimed through gc hook --claim's federation
// (in-process), translating the claim into a journal owned.admitted append.

const (
	tbHookRoute  = "rig/claude" // a route string; also the resume-tier agent template
	tbHookStream = "gcg-run-hooktb"
)

func tbHookRouter(string) (string, bool) { return tbHookRoute, true }

// tbHookGraphCity creates a temp city with a graph scope marker so
// cityHasGraphScope / cachedCityGraphJournal / openGraphStore all resolve.
func tbHookGraphCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	graphBeads := filepath.Join(cityPath, ".gc", "graph", ".beads")
	if err := os.MkdirAll(graphBeads, 0o755); err != nil {
		t.Fatalf("mkdir graph scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(graphBeads, "config.yaml"), []byte("backend: sqlite\n"), 0o644); err != nil {
		t.Fatalf("write graph scope marker: %v", err)
	}
	return cityPath
}

func tbHookOpenStore(t *testing.T, cityPath string) *graphstore.Store {
	t.Helper()
	backend, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		t.Fatalf("load graph backend: %v", err)
	}
	gs, err := backend.openGraphStore(context.Background(), cityPath)
	if err != nil {
		t.Fatalf("open graph store: %v", err)
	}
	return gs
}

func tbHookDoc(t *testing.T) *ir.IR {
	t.Helper()
	const doc = `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "greet",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "block", "id": "block_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "members": [
           {"kind": "do", "id": "hello", "name": "hello", "after": [],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "source": {"kind": "prompt"},
            "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "body": {"raw": "Say hello.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}}
         ]}
      ]
    }`
	d, err := ir.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decode IR: %v", err)
	}
	return d
}

// tbHookSeedParked advances a do-only pool run to Parked (one claimable pool row)
// and closes the seed store, so the hook path opens its own handle afterward.
func tbHookSeedParked(t *testing.T, cityPath string) {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	res, err := engine.Advance(context.Background(), gs, tbHookDoc(t), tbHookStream, nil, engine.Options{PoolRouter: tbHookRouter})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Parked {
		t.Fatalf("advance = %+v, want Parked", res)
	}
	if err := gs.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
}

func tbHookCountJournalType(t *testing.T, cityPath, typ string) int {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	var n int
	if err := gs.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM journal WHERE stream_id = ? AND type = ?`, tbHookStream, typ).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", typ, err)
	}
	return n
}

func tbHookNodeStatus(t *testing.T, cityPath string) string {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	var s string
	if err := gs.DB().QueryRowContext(context.Background(),
		`SELECT status FROM nodes WHERE id = 'hello' AND fold_owned = 1`).Scan(&s); err != nil {
		t.Fatalf("read status of hello: %v", err)
	}
	return s
}

// TestFoldProjectedPoolRowPassesClaimSurfaceParity is the blueprint-risk-#2 pin:
// one hydrated Tier-B row must satisfy the claim surface, the demand-shape SELECT,
// and the preserve tier — if any of the three drifts, a pool bead becomes
// unclaimable, invisible to demand, or drains a mid-do session.
func TestFoldProjectedPoolRowPassesClaimSurfaceParity(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	tbHookSeedParked(t, cityPath)

	store := cachedCityGraphJournal(cityPath)
	if store == nil {
		t.Fatal("graph journal unavailable")
	}
	surface, ok := beads.TierBClaimSurfaceStoreFor(store)
	if !ok {
		t.Fatal("tier-b claim surface unavailable")
	}

	// (ii) demand-shape leg: the routed frontier SELECT counts the ready pool row.
	routed, err := surface.TierBRoutedFrontier(ctx, []string{tbHookRoute}, 0)
	if err != nil {
		t.Fatalf("routed frontier: %v", err)
	}
	if len(routed) < 1 {
		t.Fatalf("routed frontier count = %d, want >= 1 (the demand source)", len(routed))
	}

	// (i) claim-surface leg: the hydrated row survives the full hook predicate chain.
	raw, err := json.Marshal([]beads.Bead{routed[0]})
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	normalized := normalizeWorkQueryOutput(strings.TrimSpace(string(raw)))
	filtered := filterUnreadyHookCandidates(normalized, time.Now())
	if !workQueryHasReadyWork(filtered) {
		t.Fatal("hydrated pool row was filtered as unready (claim-surface drift)")
	}
	decoded, err := decodeHookClaimBeads(filtered)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("decoded %d candidates, want 1", len(decoded))
	}
	if !hookCandidateClaimable(decoded[0], []string{tbHookRoute}) {
		t.Fatalf("hydrated pool row is not claimable: %+v", decoded[0])
	}
	if !hookClaimMatchesRoute(decoded[0], []string{tbHookRoute}) {
		t.Fatal("hydrated pool row does not match its route")
	}

	// (iii) preserve leg: the CLAIMED row must survive the REAL pool-demand filter
	// and then yield a resume-tier request, keeping the worker's session alive.
	// Driving filterAssignedWorkBeadsForPoolDemand (not just ComputePoolDesiredStates)
	// is load-bearing: it is the production step that drops a graph-journal-ref row
	// for a rig-scoped pool agent, so skipping it would let this leg pass green while
	// the mid-do session drains.
	gs := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, gs, tbHookStream, "hello:0", "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = gs.Close()
	claimed, found, err := surface.FoldOwnedGet(ctx, "hello")
	if err != nil || !found {
		t.Fatalf("re-read claimed row: found=%v err=%v", found, err)
	}

	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)}}
	sessions := sessionInfosFromBeads([]beads.Bead{tbPreserveWorkerSessionBead()})
	preserved := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, sessions, []beads.Bead{claimed}, []string{tierBHookStoreName})
	if len(preserved) != 1 || preserved[0].ID != "hello" {
		t.Fatalf("pool-demand filter dropped the claimed Tier-B row (the DRAIN bug): preserved=%+v", preserved)
	}
	result := ComputePoolDesiredStates(cfg, preserved, sessions, nil)
	if len(result) != 1 || len(result[0].Requests) == 0 {
		t.Fatalf("desired states = %+v, want one request for the claimed row", result)
	}
	if result[0].Requests[0].Tier != "resume" {
		t.Fatalf("preserve leg tier = %q, want resume (a claimed Tier-B row must keep its session alive)", result[0].Requests[0].Tier)
	}
}

// TestTierBHookStoreClaimEndToEndInProcess drives the full federated claim through
// claimHookWorkWithRunner against a real temp journal: the claim JSON reports the
// claimed pool bead (with its prompt), the journal shows one owned.admitted, and
// the projection is in_progress.
func TestTierBHookStoreClaimEndToEndInProcess(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	tbHookSeedParked(t, cityPath)
	tbStore, ok := tierBHookStore(cityPath, []string{tbHookRoute}, []string{"worker-a"}, "worker-a", "")
	if !ok {
		t.Fatal("tier-b hook store not present for a graph-scoped city")
	}

	opts := hookClaimOptions{
		Assignee:           "worker-a",
		RouteTargets:       []string{tbHookRoute},
		IdentityCandidates: []string{"worker-a"},
		JSON:               true,
	}
	errRunner := func(string, string, []string) (string, error) {
		return "", fmt.Errorf("bd runner must not be called for the tier-b leg")
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("", cityPath, nil, []hookStore{tbStore}, opts, hookClaimOps{}, errRunner,
		func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claim code = %d, want 0; stderr=%s", code, stderr.String())
	}
	var out hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("decode claim JSON: %v; raw=%s", err, stdout.String())
	}
	if out.Action != "work" || out.BeadID != "hello" || out.Route != tbHookRoute || out.Description != "Say hello." {
		t.Fatalf("claim result = %+v, want {work, hello, %s, Say hello.}", out, tbHookRoute)
	}
	if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); n != 1 {
		t.Fatalf("owned.admitted count = %d, want 1", n)
	}
	if st := tbHookNodeStatus(t, cityPath); st != engine.StatusClaimed {
		t.Fatalf("hello status = %q, want in_progress", st)
	}
}

// TestTierBClaimLostRaceEmitsClaimRejected proves a claim lost to a different
// worker surfaces a bead.claim_rejected event (naming the winner) and appends no
// second owned.admitted.
func TestTierBClaimLostRaceEmitsClaimRejected(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	tbHookSeedParked(t, cityPath)
	tbStore, ok := tierBHookStore(cityPath, []string{tbHookRoute}, []string{"worker-a"}, "worker-a", "")
	if !ok {
		t.Fatal("tier-b hook store not present")
	}

	// Capture the OPEN candidate before anyone claims.
	openJSON, err := tbStore.query()
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	// A different worker wins the claim first.
	gs := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, gs, tbHookStream, "hello:0", "worker-x"); err != nil {
		t.Fatalf("pre-claim by worker-x: %v", err)
	}
	_ = gs.Close()

	var got struct {
		beadID, existing, attempted string
		fired                       bool
	}
	opts := hookClaimOptions{Assignee: "worker-a", RouteTargets: []string{tbHookRoute}, IdentityCandidates: []string{"worker-a"}, JSON: true}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return openJSON, nil },
		Claim:  tbStore.claim,
		EmitClaimRejected: func(beadID, existing, attempted string) {
			got.beadID, got.existing, got.attempted, got.fired = beadID, existing, attempted, true
		},
	}
	var stdout, stderr bytes.Buffer
	res := tryHookClaim("", cityPath, &opts, &ops, &stdout, &stderr)
	if res.terminal {
		t.Fatalf("claim returned terminal (code %d), want non-terminal (claim lost); stdout=%s", res.code, stdout.String())
	}
	if !got.fired {
		t.Fatal("bead.claim_rejected did not fire on a lost claim")
	}
	if got.beadID != "hello" || got.existing != "worker-x" || got.attempted != "worker-a" {
		t.Fatalf("claim_rejected = %+v, want {hello, worker-x, worker-a}", got)
	}
	if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); n != 1 {
		t.Fatalf("owned.admitted count = %d, want 1 (no second claim)", n)
	}
}

// TestTierBConcurrentClaimExactlyOneWins is the multi-writer correctness pin
// (mirrors P4.5): N workers race the Tier-B claim fn — each opening its own store
// handle, as separate processes would — and exactly one wins with a single
// owned.admitted; a byte-identical re-claim by the winner dedupes to success.
func TestTierBConcurrentClaimExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	tbHookSeedParked(t, cityPath)
	tbStore, ok := tierBHookStore(cityPath, []string{tbHookRoute}, nil, "", "")
	if !ok {
		t.Fatal("tier-b hook store not present")
	}

	const n = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	oks := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, ok, err := tbStore.claim(ctx, "", nil, "hello", fmt.Sprintf("worker-%d", i))
			oks[i], errs[i] = ok, err
		}(i)
	}
	close(start)
	wg.Wait()

	winners, winner := 0, ""
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("worker-%d errored: %v (want a clean win or a (current,false,nil) loss)", i, errs[i])
		}
		if oks[i] {
			winners++
			winner = fmt.Sprintf("worker-%d", i)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if got := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); got != 1 {
		t.Fatalf("owned.admitted rows = %d, want exactly 1 (write-once)", got)
	}

	// Byte-identical re-claim by the winner is idempotent success, no new event.
	if _, ok, err := tbStore.claim(ctx, "", nil, "hello", winner); err != nil || !ok {
		t.Fatalf("winner re-claim = (ok=%v, err=%v), want idempotent success", ok, err)
	}
	if got := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); got != 1 {
		t.Fatalf("owned.admitted after re-claim = %d, want 1 (deduped)", got)
	}
}

// TestTierBClaimBeadErrorMapping pins claimTierBWorkBead's mapping of each engine
// claim error onto the hookClaimFunc contract, injected through the claim seam:
// ErrTierBNotClaimable skips silently as (zero, false, nil) with no event; a
// generic error surfaces as an error (drained by the federation as claims_errored,
// never laundered into no_work); neither appends an owned.admitted. A raw
// ErrLeaseFenced is NO LONGER a claims_errored error post-L2 — it is a cooperative
// re-acquire race that retries then maps to the conflict shape; that behavior is
// pinned by TestClaimLeaseFencedRetriesThenConflictShape (S17).
func TestTierBClaimBeadErrorMapping(t *testing.T) {
	genericErr := errors.New("tier-b claim boom")
	cases := []struct {
		name      string
		injected  error
		wantErr   bool
		wantErrIs error
	}{
		{name: "not_claimable_skips_no_event", injected: engine.ErrTierBNotClaimable, wantErr: false},
		{name: "generic_error_errors", injected: genericErr, wantErr: true, wantErrIs: genericErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := tbHookGraphCity(t)
			tbHookSeedParked(t, cityPath) // a real fold row so ResolveTierBWorkRef succeeds

			orig := claimTierBWork
			claimTierBWork = func(context.Context, *graphstore.Store, string, string, string, string) error {
				return tc.injected
			}
			defer func() { claimTierBWork = orig }()

			bead, ok, err := claimTierBWorkBead(context.Background(), cityPath, "hello", "worker-a", "")
			if ok {
				t.Fatalf("claim reported success for injected %v; want ok=false", tc.injected)
			}
			if bead.ID != "" {
				t.Fatalf("claim returned bead %q on a failed claim; want the zero bead", bead.ID)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("injected %v produced no error; want a returned error (drains claims_errored)", tc.injected)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("returned error %v does not wrap %v", err, tc.wantErrIs)
				}
			} else if err != nil {
				t.Fatalf("ErrTierBNotClaimable produced error %v; want a (zero,false,nil) skip", err)
			}
			if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); n != 0 {
				t.Fatalf("owned.admitted count = %d, want 0 (no claim landed on an error branch)", n)
			}
		})
	}
}

// TestTierBCrashRecoveryTierAdoptsOwnInProgress proves a claimed-by-me row
// surfaces through the assigned leg and is adopted (reason existing_assignment)
// without a second claim write.
func TestTierBCrashRecoveryTierAdoptsOwnInProgress(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	tbHookSeedParked(t, cityPath)

	gs := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, gs, tbHookStream, "hello:0", "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = gs.Close()

	tbStore, ok := tierBHookStore(cityPath, []string{tbHookRoute}, []string{"worker-a"}, "worker-a", "")
	if !ok {
		t.Fatal("tier-b hook store not present")
	}
	opts := hookClaimOptions{Assignee: "worker-a", RouteTargets: []string{tbHookRoute}, IdentityCandidates: []string{"worker-a"}, JSON: true}
	ops := hookClaimOps{Runner: func(string, string) (string, error) { return tbStore.query() }, Claim: tbStore.claim}
	var stdout, stderr bytes.Buffer
	res := tryHookClaim("", cityPath, &opts, &ops, &stdout, &stderr)
	if !res.terminal || res.code != 0 {
		t.Fatalf("claim = %+v, want terminal success; stderr=%s", res, stderr.String())
	}
	var out hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("decode claim JSON: %v; raw=%s", err, stdout.String())
	}
	if out.Reason != "existing_assignment" || out.BeadID != "hello" {
		t.Fatalf("claim result = %+v, want existing_assignment for hello", out)
	}
	if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); n != 1 {
		t.Fatalf("owned.admitted count = %d, want 1 (adopted, not re-claimed)", n)
	}
}

// TestTierBHookQueryRefusesForeignClaimantAdoption (HIGH B-1) proves the Tier-B hook
// query refuses to surface a fold-owned in_progress row for ADOPTION when its recorded
// claimant_id names a DIFFERENT instance than the adopter: a same-name respawn B cannot
// adopt A's claimed row and re-run its side effects. A same-instance resume (matching
// claimant_id) still adopts, and a legacy adopter (no claimant_id) keeps name-based
// adoption. The "respawn B" case FAILS before the filter (name-only match surfaces A's
// row to B).
func TestTierBHookQueryRefusesForeignClaimantAdoption(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	tbHookSeedParked(t, cityPath)

	// A claims hello:0 recording its instance-unique claimant id "A-id".
	gs := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWorkAs(ctx, gs, tbHookStream, "hello:0", "worker-a", "A-id"); err != nil {
		t.Fatalf("claim by A: %v", err)
	}
	_ = gs.Close()

	// adoptable reports whether the Tier-B hook query surfaces A's in_progress "hello" row
	// as an adoption candidate for an adopter recording adopterID (sharing the NAME).
	adoptable := func(adopterID string) bool {
		st, ok := tierBHookStore(cityPath, []string{tbHookRoute}, []string{"worker-a"}, "worker-a", adopterID)
		if !ok {
			t.Fatal("tier-b hook store not present")
		}
		out, err := st.query()
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var cands []beads.Bead
		if err := json.Unmarshal([]byte(out), &cands); err != nil {
			t.Fatalf("decode candidates: %v; raw=%s", err, out)
		}
		for _, c := range cands {
			if c.ID == "hello" && strings.EqualFold(strings.TrimSpace(c.Status), "in_progress") {
				return true
			}
		}
		return false
	}

	if adoptable("B-id") {
		t.Fatal("respawn B (different claimant id) could adopt A's in_progress row — repeated side effects")
	}
	if !adoptable("A-id") {
		t.Fatal("same-instance resume (matching claimant id) could NOT adopt its own row")
	}
	if !adoptable("") {
		t.Fatal("legacy adopter (no claimant id) lost name-based adoption")
	}
}

// TestClaimLeaseFencedRetriesThenConflictShape (T-F1) pins the S17 claim fence
// mapping: a single lease fence is retried and the re-claim succeeds; a persistent
// fence maps to the conflict shape (winner, false, nil) — never claims_errored for a
// pure re-acquire race.
func TestClaimLeaseFencedRetriesThenConflictShape(t *testing.T) {
	ctx := context.Background()

	t.Run("fence_once_retries_success", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbHookSeedParked(t, cityPath)
		calls := 0
		orig := claimTierBWork
		claimTierBWork = func(ctx context.Context, gs *graphstore.Store, streamID, activation, assignee, claimantID string) error {
			calls++
			if calls == 1 {
				return graphstore.ErrLeaseFenced
			}
			return engine.ClaimTierBWorkAs(ctx, gs, streamID, activation, assignee, claimantID) // the retry lands
		}
		defer func() { claimTierBWork = orig }()

		bead, ok, err := claimTierBWorkBead(ctx, cityPath, "hello", "worker-a", "")
		if err != nil || !ok {
			t.Fatalf("fence-once claim = (bead=%q ok=%v err=%v), want a successful retry", bead.ID, ok, err)
		}
		if calls < 2 {
			t.Fatalf("claim calls = %d, want >= 2 (a fence must retry)", calls)
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); n != 1 {
			t.Fatalf("owned.admitted = %d, want 1 (retry claimed exactly once)", n)
		}
	})

	t.Run("fence_persistent_conflict_shape", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbHookSeedParked(t, cityPath)
		orig := claimTierBWork
		claimTierBWork = func(context.Context, *graphstore.Store, string, string, string, string) error {
			return graphstore.ErrLeaseFenced
		}
		defer func() { claimTierBWork = orig }()

		bead, ok, err := claimTierBWorkBead(ctx, cityPath, "hello", "worker-a", "")
		if err != nil {
			t.Fatalf("persistent fence surfaced an error (would drain claims_errored): %v", err)
		}
		if ok {
			t.Fatalf("persistent fence reported success (bead=%q); want a conflict-shaped rejection", bead.ID)
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); n != 0 {
			t.Fatalf("owned.admitted = %d, want 0 (no claim landed on a persistent fence)", n)
		}
	})
}

// TestClaimRebuildRacedRetriesThenConflictShape (F2) pins ErrRebuildRaced on the
// claim path exactly like a lease fence: a concurrent driver append that raced the
// claim's Tier-A projection rebuild is a transient multi-writer race (the engine's
// own retry contract classifies it, advance_race_test.go), NOT a hard failure. A
// single race retries and the re-claim succeeds; a persistent race maps to the
// conflict shape (winner, false, nil) — never claims_errored.
func TestClaimRebuildRacedRetriesThenConflictShape(t *testing.T) {
	ctx := context.Background()

	t.Run("race_once_retries_success", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbHookSeedParked(t, cityPath)
		calls := 0
		orig := claimTierBWork
		claimTierBWork = func(ctx context.Context, gs *graphstore.Store, streamID, activation, assignee, claimantID string) error {
			calls++
			if calls == 1 {
				return graphstore.ErrRebuildRaced
			}
			return engine.ClaimTierBWorkAs(ctx, gs, streamID, activation, assignee, claimantID) // the retry lands
		}
		defer func() { claimTierBWork = orig }()

		bead, ok, err := claimTierBWorkBead(ctx, cityPath, "hello", "worker-a", "")
		if err != nil || !ok {
			t.Fatalf("race-once claim = (bead=%q ok=%v err=%v), want a successful retry", bead.ID, ok, err)
		}
		if calls < 2 {
			t.Fatalf("claim calls = %d, want >= 2 (a rebuild race must retry)", calls)
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); n != 1 {
			t.Fatalf("owned.admitted = %d, want 1 (retry claimed exactly once, idem token)", n)
		}
	})

	t.Run("race_persistent_conflict_shape", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbHookSeedParked(t, cityPath)
		orig := claimTierBWork
		claimTierBWork = func(context.Context, *graphstore.Store, string, string, string, string) error {
			return graphstore.ErrRebuildRaced
		}
		defer func() { claimTierBWork = orig }()

		bead, ok, err := claimTierBWorkBead(ctx, cityPath, "hello", "worker-a", "")
		if err != nil {
			t.Fatalf("persistent rebuild race surfaced an error (would drain claims_errored): %v", err)
		}
		if ok {
			t.Fatalf("persistent rebuild race reported success (bead=%q); want a conflict-shaped rejection", bead.ID)
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedAdmitted); n != 0 {
			t.Fatalf("owned.admitted = %d, want 0 (no claim landed on a persistent race)", n)
		}
	})
}
