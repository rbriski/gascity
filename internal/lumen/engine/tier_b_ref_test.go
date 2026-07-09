package engine_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestResolveTierBWorkRefBijection is the §2 crux: a projected bead id maps back
// to its journal coordinates (stream id + activation handle) unambiguously, and
// the reverse is the single decode cmd/gc relies on. It pins the four corners of
// the bijection: a Materialize-minted pool row, an Advance-materialized pool row,
// the absent/façade "not ours" signal, and the settled-drops-activation asymmetry.
func TestResolveTierBWorkRefBijection(t *testing.T) {
	ctx := context.Background()

	t.Run("materialized_pool_row_round_trips", func(t *testing.T) {
		store := newStore(t)
		act := materializeTierB(t, store) // stream tierBStream, node "summarize"

		ref, ok, err := engine.ResolveTierBWorkRef(ctx, store, tierBNodeID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !ok {
			t.Fatal("pool row did not resolve; want found")
		}
		if ref.BeadID != tierBNodeID || ref.StreamID != tierBStream || ref.Activation != act {
			t.Fatalf("ref = {bead:%q stream:%q activation:%q}, want {%q, %q, %q}",
				ref.BeadID, ref.StreamID, ref.Activation, tierBNodeID, tierBStream, act)
		}
		if ref.DispatchMode != engine.DispatchModePool {
			t.Fatalf("dispatch_mode = %q, want pool", ref.DispatchMode)
		}
		if ref.Settled || ref.Status != "open" || ref.Assignee != "" || ref.Outcome != "" {
			t.Fatalf("unsettled ref = %+v, want {open, unsettled, no assignee/outcome}", ref)
		}
		// The resolved coordinates round-trip to the claimable handle.
		if err := engine.ClaimTierBWork(ctx, store, ref.StreamID, ref.Activation, "worker-a"); err != nil {
			t.Fatalf("claim via resolved handle: %v", err)
		}
	})

	t.Run("advance_materialized_pool_row_round_trips", func(t *testing.T) {
		store := newStore(t)
		docJSON, streamID := doOnlyDoc() // node "hello"
		doc := decodeIR(t, docJSON)
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, engine.Options{PoolRouter: advRouter}); err != nil {
			t.Fatalf("advance: %v", err)
		}

		ref, ok, err := engine.ResolveTierBWorkRef(ctx, store, "hello")
		if err != nil || !ok {
			t.Fatalf("resolve hello: ok=%v err=%v", ok, err)
		}
		if ref.StreamID != streamID || ref.Activation != "hello:0" || ref.DispatchMode != engine.DispatchModePool {
			t.Fatalf("advance ref = {stream:%q activation:%q dm:%q}, want {%q, hello:0, pool}",
				ref.StreamID, ref.Activation, ref.DispatchMode, streamID)
		}
		if err := engine.ClaimTierBWork(ctx, store, ref.StreamID, ref.Activation, "worker-b"); err != nil {
			t.Fatalf("claim via resolved advance handle: %v", err)
		}
	})

	t.Run("unknown_id_is_not_ours", func(t *testing.T) {
		store := newStore(t)
		materializeTierB(t, store)
		ref, ok, err := engine.ResolveTierBWorkRef(ctx, store, "does-not-exist")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if ok {
			t.Fatalf("absent id resolved to %+v, want (zero, false)", ref)
		}
		if ref != (engine.TierBWorkRef{}) {
			t.Fatalf("absent id ref = %+v, want zero value", ref)
		}
	})

	t.Run("fold_owned_zero_row_is_not_ours", func(t *testing.T) {
		store := newStore(t)
		// A façade (fold_owned=0) row with the same bare-id shape must not resolve:
		// the resolver only decodes fold-owned journal coordinates.
		if _, err := store.DB().ExecContext(ctx,
			`INSERT INTO nodes (id, status, bead_type, created_at, fold_owned) VALUES (?, 'open', 'task', ?, 0)`,
			tierBNodeID, tierBCreatedAt); err != nil {
			t.Fatalf("insert façade row: %v", err)
		}
		ref, ok, err := engine.ResolveTierBWorkRef(ctx, store, tierBNodeID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if ok {
			t.Fatalf("fold_owned=0 row resolved to %+v, want not found", ref)
		}
	})

	t.Run("settled_row_drops_activation_carries_outcome", func(t *testing.T) {
		store := newStore(t)
		act := materializeTierB(t, store)
		if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "3 bullets"); err != nil {
			t.Fatalf("settle: %v", err)
		}
		ref, ok, err := engine.ResolveTierBWorkRef(ctx, store, tierBNodeID)
		if err != nil || !ok {
			t.Fatalf("resolve settled: ok=%v err=%v", ok, err)
		}
		if !ref.Settled {
			t.Fatal("settled row ref.Settled = false, want true")
		}
		if ref.Activation != "" {
			t.Fatalf("settled ref.Activation = %q, want \"\" (metadata dropped at settle)", ref.Activation)
		}
		if ref.Outcome != engine.OutcomePass {
			t.Fatalf("settled ref.Outcome = %q, want pass", ref.Outcome)
		}
		if ref.Status != "done" {
			t.Fatalf("settled ref.Status = %q, want done", ref.Status)
		}
		// Stream id and dispatch-mode provenance survive the settle.
		if ref.StreamID != tierBStream || ref.DispatchMode != engine.DispatchModePool {
			t.Fatalf("settled ref = {stream:%q dm:%q}, want {%q, pool}", ref.StreamID, ref.DispatchMode, tierBStream)
		}
	})
}

// TestResolveTierBWorkRefSurfacesRetryable (L-1) pins the Retryable axis of the
// decode: a firewall infrastructure strand (settled retryable=true) surfaces
// ref.Retryable=true, while an ordinary worker settle (retryable=false) surfaces
// false — so a divergent-reclose compare of (outcome, retryable) can refuse to
// launder a firewall failed{retryable:true} strand under a worker's fail close.
func TestResolveTierBWorkRefSurfacesRetryable(t *testing.T) {
	ctx := context.Background()

	t.Run("firewall_strand_surfaces_retryable", func(t *testing.T) {
		store := newStore(t)
		act := materializeTierB(t, store)
		if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := engine.SettleTierBWorkAs(ctx, store, tierBStream, act, engine.OutcomeFailed, "stranded: worker-a", "", "", true); err != nil {
			t.Fatalf("firewall strand: %v", err)
		}
		ref, ok, err := engine.ResolveTierBWorkRef(ctx, store, tierBNodeID)
		if err != nil || !ok {
			t.Fatalf("resolve strand: ok=%v err=%v", ok, err)
		}
		if !ref.Retryable {
			t.Fatal("stranded ref.Retryable = false, want true")
		}
		if ref.Outcome != engine.OutcomeFailed || !ref.Settled {
			t.Fatalf("stranded ref = {outcome:%q settled:%v}, want {failed, true}", ref.Outcome, ref.Settled)
		}
	})

	t.Run("worker_settle_absent_retryable", func(t *testing.T) {
		store := newStore(t)
		act := materializeTierB(t, store)
		if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "done"); err != nil {
			t.Fatalf("settle: %v", err)
		}
		ref, ok, err := engine.ResolveTierBWorkRef(ctx, store, tierBNodeID)
		if err != nil || !ok {
			t.Fatalf("resolve settled: ok=%v err=%v", ok, err)
		}
		if ref.Retryable {
			t.Fatal("worker-settled ref.Retryable = true, want false (retryable omitted)")
		}
	})
}

// TestLumenOutcomeForGCOutcomeFirewall pins the S7 dispatch firewall: the raw
// gc.outcome value a worker closes with maps to a Lumen outcome, with everything
// unknown (bare/empty/case-variant/control-plane skipped) fail-closed to failed,
// and only the recognized pass/fail plus the Lumen-native degraded passing.
func TestLumenOutcomeForGCOutcomeFirewall(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pass", engine.OutcomePass},
		{"fail", engine.OutcomeFailed},
		{"degraded", engine.OutcomeDegraded},
		{"", engine.OutcomeFailed},
		{"skipped", engine.OutcomeFailed},
		{"shipped", engine.OutcomeFailed},
		{"PASS", engine.OutcomeFailed},
	}
	for _, c := range cases {
		if got := engine.LumenOutcomeForGCOutcome(c.in); got != c.want {
			t.Errorf("LumenOutcomeForGCOutcome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
