package main

import "testing"

// TestWorkerClaimsLumenWorkBeadViaRealBd is the B1/08 named exit gate: a worker
// claims, does, and closes a Lumen-minted Tier-B step-node via the REAL bd binary
// end-to-end, with the corresponding claimed/settled events appearing in the
// journal and the projection converged.
//
// STATUS: NOT YET IMPLEMENTED — registered here so the gate is visible in the
// test ledger and its integration point is documented at the composition root.
//
// P4.5 landed the store-layer claim-as-append MECHANISM, driven through the
// beads.Store journal capabilities, in internal/lumen/engine/tier_b_claim.go:
//
//   - MaterializeTierBWork mints a claimable, fold-owned (write-closed) Tier-B
//     work bead on the journal root (pool-mode node.activated).
//   - ClaimTierBWork translates a claim into a CAS lumen.owned.admitted append
//     folded to assignee/in_progress — loud on a losing race (ErrTierBClaim-
//     Conflict), never a silent overwrite (proven by
//     TestTierBConcurrentClaimCasLoud / TestTierBDoubleClaimIsWriteOnceLoud-
//     AtSubstrate).
//   - SettleTierBWork translates the close into a lumen.owned.settled append
//     folded to closed. The projection is a pure fold — a drop+refold reproduces
//     it byte-for-byte (TestTierBDropRefoldIdentity).
//
// The remaining wiring to satisfy THIS gate (a cmd/gc composition-root adapter,
// which may import both internal/beads and internal/lumen/engine — internal/beads
// itself may not, enginehost→beads would cycle):
//
//  1. Surface Tier-B fold-owned work beads through the JournalStore's Ready()/
//     Get() (kept off the fold_owned=0 façade path today) so `bd ready` and
//     `gc hook --claim` see them.
//  2. Route the worker's claim (ConditionalAssignment / SetMetadata(assignee))
//     and close (Close + gc.outcome) through engine.ClaimTierBWork /
//     engine.SettleTierBWork instead of a direct fold-owned column write (which
//     the write-closure correctly rejects), riding the root's lease holder
//     (controller-mediated single writer) with the held lease epoch.
//  3. Map the raw gc.outcome onto a Lumen outcome via the dispatch firewall
//     (bare close / unknown outcome ⇒ failed), the settlement-observer's job.
//  4. Drive the loop with the real bd binary end-to-end (exec.LookPath("bd")),
//     asserting the claimed/settled events land in the journal and Verify passes.
func TestWorkerClaimsLumenWorkBeadViaRealBd(t *testing.T) {
	t.Skip("P4.5: store-layer claim-as-append landed (internal/lumen/engine/tier_b_claim.go, driven through beads.Store); real-bd worker-loop wiring is the remaining step — see this test's doc comment.")
}
