# Lockstep drop — Step 1: circuit-breaker persist → store-authoritative (drop circuitSessionByIdentity)

## Corrected understanding (line numbers drifted from LOCKSTEP-DROP.md)
The Phase-0.5 CB *restore reads* (session_reconciler.go:1369/1378) ALREADY project
`ordered[i].Metadata` directly via `CircuitStateFromMetadata` (Step 5). `circuitSessionByIdentity`
(a `map[string]*beads.Bead`) now survives ONLY to supply the raw bead to the progress-signature
persist at :1389. So "drop circuitSessionByIdentity" == make the circuit persist ID-based.

## The two raw-mirror writers (session_circuit_breaker.go)
- `persistSessionCircuitBreakerMetadata` (:627): equality-skip reads `session.Metadata` (:643),
  mirrors onto `session.Metadata` (:649-654) after `sessFront.ApplyPatch`.
- `recordSessionCircuitBreakerRestart` (:658): same pattern (:685 read, :690-696 mirror).

## Byte-identity facts (verified)
1. `sessionCircuitMetadataKeys` == the 9 CircuitState fields exactly ⇒
   `CircuitStateFromMetadata(a) == CircuitStateFromMetadata(b)` ⟺ `sessionCircuitMetadataEqual(a,b)`.
2. The raw circuit mirror is consumed within-tick ONLY by the two equality checks (:643/:685) for a
   repeat persist of the SAME id. No forward-pass / lifecycle read of raw circuit metadata exists
   (grep: only readers are :1369/:1378 restore reads — BEFORE any persist — and the equality checks).
   CircuitState is NOT part of Info, so no Info reader depends on it.
3. Converting the equality check to `sessFront.CircuitState(id)` (store) is byte-identical: after a
   prior same-tick persist's ApplyPatch, the store reflects it exactly as the OLD raw mirror did.

## Conversion (one commit)
- persist/record take `id string` (not `session *beads.Bead`); equality via `sessFront.CircuitState(id)`
  compared to `session.CircuitStateFromMetadata(metadata)`; drop the raw mirror; ApplyPatch by id.
  record: new Get-error path rolls back the recorded restart (consistent w/ its ApplyPatch-error rollback).
- CB block: `circuitSessionByIdentity map[string]*beads.Bead` → `circuitIDByIdentity map[string]string`
  (last-wins, holds ordered[i].ID). :1381 `&ordered[i]`→`ordered[i].ID`; :1389 `session`→`id` (`!="" `guard).
- :3124 `target.session`→`target.session.ID`; lifecycle_parallel :2434/:2445 `candidate.session`→`.ID`.
- 6 test call sites: `&<bead>` → `<bead>.ID` (all assert on the STORE; restart_request test's bead reuse
  is inert — singleton breaker + restoreFromMetadata no-op on empty/existing-entry).

## Test-safety proofs
- controller_test blockingOpenMetadataBatchStore instruments SetMetadataBatch only; Get passes through;
  cb.mu already held across the blocked write in OLD ⇒ race preserved.
- restart_request test: setSessionCircuitBreakerForTest makes cb the singleton; restoreFromMetadata
  no-ops (empty meta OR entry!=nil); all final asserts read env.store.Get. Bead mirror is dead.

## Fable adversarial review (wf_803d0b26, 6 agents, 4 lenses)
- mirror-consumers (F2), map/call-sites (F4), concurrency/rollback (F5+F6): CLEAN.
- 1 finding REFUTED: an operator-reset race (reviewer's premise about metadataLocked emitting
  a reset generation on an entry-less breaker was factually wrong — metadataLocked early-returns
  all-empty INCLUDING reset_generation when entries[id]==nil, so store!=desired and the write still fires;
  verified byte-identical).
- 1 finding CONFIRMED (LOW, ACCEPTED): the equality-skip now reads sessFront.CircuitState(id)=store.Get
  instead of the free raw in-memory bead, so on a transient store READ fault the 4 persist sites emit a
  new logged-and-ignored stderr diagnostic (no decision/state change), and recordSessionCircuitBreakerRestart
  rolls back the just-recorded restart + defers the start a tick. Verifier: even the record divergence needs a
  PATHOLOGICAL read-fails/write-succeeds store — under any realistic store OLD also reached ApplyPatch there
  (the record skip is effectively dead: a fresh restart always changes the metadata), so both roll back+skip
  identically. Under a HEALTHY store the whole change is byte-identical. This added Get is the unavoidable cost
  of dropping the raw mirror while preserving the redundant-write skip (design §2/§8 "refresh-on-write, fix if hot").
