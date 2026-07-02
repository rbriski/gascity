# Next-session prompt — reconciler front-door Step 5 (CircuitState typed accessor)

Paste the block below into a fresh session.

---

Continue the **session reconciler front-door migration** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `4617c0821`).

**Read first, in order:**
1. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-HANDOFF.md` — the
   authoritative handoff + ordered backlog. **Steps 0–4 DONE**; you are starting
   **Step 5**. (This SUPERSEDES the `SPINE-FLIP-*` docs.)
2. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-SPEC.md` — design v2.

**Where things stand.** Step 4 is complete: all four reconciler session scans read
typed `Info` (no raw session-bead metadata cracking). The `LifecycleInput` is typed
(4B), `buildAwakeInputFromReconciler` reads the coherent `infoByID` snapshot (4C/4D),
the min-floor scan reads the snapshot with a close-refresh discipline (4D phase 2),
and `computeNamedSessionProgressSignatures` / `advanceSessionDrains` read per-bead
`Info` projections at their Phase-0.5 / post-forward-pass points (4D phase 3).

**Confirm a green baseline:**
```
git rev-parse HEAD            # expect 4617c0821 (or later)
go build ./... && go vet ./cmd/gc/... ./internal/session/...
golangci-lint run ./cmd/gc/... ./internal/session/...   # expect 0
go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestOpenPoolSessionCountForTemplateExcludesClosed|TestBuildAwakeInputFromReconcilerReadsInfoSnapshot' -count=1
go test ./internal/session/ -run 'TestLifecycleInputConstructorsProjectIdentically' -count=1
git checkout go.sum
```

**DO STEP 5 (this session): the circuit-breaker typed accessor.** The reconciler's
Phase-0.5 circuit-breaker restore reads raw `ordered[i].Metadata` — the last raw
session-metadata read cluster on the reconciler's decision path. Add a dedicated
typed value (NOT `Info` — this is a distinct concern) and route the reads through it.

1. **`session.Store.CircuitState(id) (CircuitState, error)`** in `internal/session`,
   reading the full `session_circuit_*` cluster (9 keys):
   `session_circuit_breaker` (state), `session_circuit_progress_signature`,
   `session_circuit_restarts`, `session_circuit_last_restart`,
   `session_circuit_last_progress`, `session_circuit_last_observed`,
   `session_circuit_opened_at`, `session_circuit_open_restart_count`,
   `session_circuit_reset_generation` (`SessionCircuitResetGenerationMetadataKey`).
   Define a typed `CircuitState` struct (raw-string fields + parsed forms as the
   consumers need), with the metadata-key literals confined below the codec edge.
   Also add a `CircuitStateFromMetadata(meta map[string]string) CircuitState` (and,
   if useful, a `CircuitStateFromInfo` — but the CB keys are NOT mirrored on `Info`,
   so likely `FromMetadata` only for now).
2. **Route the two Phase-0.5 CB reads** (`session_reconciler.go`@~1277
   `cb.observeResetGenerationFromMetadata(identity, ordered[i].Metadata)` and @~1286
   `cb.restoreFromMetadata(identity, ordered[i].Metadata, cbNow)`) through the typed
   accessor. Change `restoreFromMetadata`/`observeResetGenerationFromMetadata`
   (`session_circuit_breaker.go`) to take a `session.CircuitState` instead of
   `map[string]string`, keeping every parse/compare exactly where it is
   (byte-identical). `persistSessionCircuitBreakerMetadata` (the WRITE half) already
   goes through `sessFront`; leave it unless it also cracks raw reads.
3. **Byte-identical oracle**: extend `TestSessionClassifierInfoEquivalence` (or add a
   dedicated breaker-restore fixture/test) so `restoreFromMetadata`/`observe…` fed a
   `CircuitStateFromMetadata(bead.Metadata)` produce identical breaker state to the
   raw-map form, across representative `session_circuit_*` shapes (open/closed,
   reset-generation stale vs fresh, missing-vs-empty).

**Optional consolidation:** `computeNamedSessionProgressSignatures` also runs in this
Phase-0.5 block on a per-bead projection. If Step 5 naturally builds a coherent typed
view here, both it and the CB reads can share it — but do NOT move the `infoByID`
snapshot construction ahead of the CB restore without handling the CB block's
`persistSessionCircuitBreakerMetadata` mutation of `ordered[i]` (that mutation is
exactly why `infoByID` is built AFTER the CB block today).

**Then Step 6** (the finale): drop every `session.Metadata[k]=v` lockstep, remove the
raw `ordered []beads.Bead` + `beadByID` working set, cut `refreshSessionInfo` over to
`sessFront.Get`, and add explicit intra-tick suppression of `reset_committed_at` /
`started_live_hash` (else the #2345 force-wake regression returns). Only then do the
reconciler files become raw-free and join `snapshotInfoOnlyFiles`.

**Gates per commit:** `go build ./...` · `go vet` · `golangci-lint ./cmd/gc/...
./internal/session/...`=0 · gofmt · the new CircuitState oracle +
`TestSessionClassifierInfoEquivalence` + front-door guards · whole-tick
`TestReconcileSessionBeads*` + circuit/named/pool/wake/sleep/drain/trace (run heavy
suites in the background; the broad sweep is ~70–130s here). `git checkout go.sum`
after. Commit AND push `--no-verify`. Trailer:
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Never
`tmux kill-server` / `go clean -cache` (`-testcache` ok); gascity Dolt is LOCAL-ONLY
(no `bd dolt push`). #3839 stays DRAFT.

**Cautions:** quote grep globs (`--include='*.go'`) — an unquoted `--include=*.go`
errors under zsh and reads as a false "not found". Read-only mapping agents have
repeatedly read the WRONG worktree (`.worktrees/pack-crud`) — pin
`git rev-parse HEAD` and restrict them to this worktree; verify their line numbers.
Update the handoff (check boxes) + memory (`infra-beads-decoupling-plan.md`) as you
land each phase.

---
