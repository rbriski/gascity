# Lockstep drop — Step 3: buildAwakeInputFromReconciler domain → order-preserving []session.Info

## Goal
The awake scan already READS every field off the typed `session.Info` (Steps 4C/4D); only the
iteration DOMAIN was still raw. Retire the `sessionBeads []beads.Bead` +
`sessionInfoByID map[string]session.Info` params in favor of a single order-preserving
`sessionInfos []session.Info`, so the last raw working-set consumer on the awake-decision path is
gone (`ordered` and the forward-pass mirrors survive for Steps 4–5).

## The change
`buildAwakeInputFromReconciler` (`compute_awake_bridge.go`):
- Params: `sessionBeads []beads.Bead, sessionInfoByID map[string]session.Info` → `sessionInfos []session.Info`.
- Loop: `for i := range sessionBeads { b := &sessionBeads[i]; info, ok := sessionInfoByID[b.ID];
  if !ok { info = session.InfoFromPersistedBead(*b) } ... }` → `for i := range sessionInfos { info := sessionInfos[i]; ... }`.
- The per-iteration body (all `info.*` reads → `AwakeSessionBead`) is UNCHANGED.

Reconciler call site (`session_reconciler.go` ~3007): build in `ordered` order, then pass:
```go
sessionInfos := make([]sessionpkg.Info, len(ordered))
for i := range ordered { sessionInfos[i] = infoByID[ordered[i].ID] }
```

## Byte-identity argument (prod reconciler path)
- **Coverage.** `infoByID` is built at `:1412-1415` as `InfoFromPersistedBead(ordered[i])` for EVERY
  `ordered[i]`. No `delete(infoByID, …)` exists; `ordered` is `:1338` `topoOrder(...)` and is never
  resliced/reassigned/appended (only element CONTENTS mutate via `&ordered[i]` pointers). So every
  `ordered[i].ID` is a live key ⇒ `infoByID[ordered[i].ID]` is the current snapshot Info. OLD read the
  same value via the `ok==true` branch (`sessionInfoByID` WAS `infoByID`, `b.ID` present). The
  `InfoFromPersistedBead` fallback was NEVER taken on the reconciler path — dead for prod, live only
  for nil-map unit tests. Identical.
- **Order.** New builds `sessionInfos` strictly in `ordered` index order and iterates it in index
  order — same visitation order as OLD's `range sessionBeads`(=ordered). `ComputeAwakeSet`'s
  `SessionName` last-write-wins + first-match `resolveNamedSessionBeadName` over a NON-unique
  `SessionName` therefore see the identical order. No `range infoByID` anywhere (map-order leak avoided).
  Duplicate IDs (if any) collapse identically in OLD and NEW (both read `infoByID[dupID]` for each
  position).
- **Residue is not a NEW divergence.** `recoverRunningPendingCreate`'s un-threaded stale-resume clears
  (`session_key`/`started_config_hash`/`continuation_reset_pending`) desync the raw bead/store from
  `infoByID`, and the awake scan reads `info.ContinuationResetPending`/`ResetCommittedAt`. But OLD
  already read `infoByID` (via the `ok` branch) for the reconciler, so OLD and NEW read the SAME
  (identically stale-or-fresh) value. Step 3 changes the DOMAIN, not the SOURCE, so it introduces no
  new residue exposure. No threading required for byte-identity of this step (confirmed by review).

## Test-site conversions (15 sites)
`compute_awake_bridge_test.go` (13) + `compute_awake_set_min_active_test.go` (2). Each former
`[]beads.Bead{X}, nil,` (nil snapshot ⇒ OLD used the `InfoFromPersistedBead(X)` fallback) became
`[]session.Info{session.InfoFromPersistedBead(X)},` — reproducing the fallback verbatim. Specials:
- `TestBuildAwakeInputFromReconcilerReadsInfoSnapshot`: OLD drove bead (`sleep_reason=from-bead`) and a
  divergent snapshot Info (`SleepReason=from-snapshot`) apart to prove the scan honors the snapshot.
  NEW passes `[]session.Info{info}` directly (`SleepReason=from-snapshot`); assertion unchanged, now
  guards that the projection copies `info.SleepReason` through. `snapshot` map removed; `b` still feeds
  `info`. Comment rewritten.
- `TestBuildAwakeInputFromReconcilerPopulatesPendingInteractions`: local `session` (a `beads.Bead`)
  shadowed the `session` package → renamed to `sessionBead`; the `wakeTarget` still carries
  `&sessionBead` (raw bead source, out of scope for Step 3); the `wakeTarget` struct field `session:`
  is untouched.
- `compute_awake_set_min_active_test.go` gains the `session` import; the propagates-min site drops its
  extra nil (10→9 nils = one fewer param).

## Verification
- build · `go vet` · `golangci-lint run` (0 issues) · `gofmt -l` empty.
- Reconciler subset `go test ./cmd/gc/ -run 'Reconcile|Awake|Wake|Sleep|Pool|DrainAck|…|Session'` →
  ok 212s; the converted bridge+min-active tests pass under `-v`.
- Fable 4-lens adversarial review (wf_21c330af): order-preservation, snapshot-equivalence,
  test-site-fidelity, field-and-deadcode — **0 findings** (each finder read the actual code, 13–18 tool
  calls; nothing reached the verify stage).
