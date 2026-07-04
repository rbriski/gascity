# Next-session prompt — reconciler front-door: the LOCKSTEP DROP

Paste the block below into a fresh session.

---

Continue the **session reconciler front-door migration** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`; run `git rev-parse HEAD`).

**Read first, in order:**
1. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-LOCKSTEP-DROP.md` — the
   focused plan for THIS phase (the raw-consumer conversions, the exposure set, the
   awake-scan slice-order invariant, the suggested commit sequence). START HERE.
2. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-HANDOFF.md` — the master
   backlog + status (Steps 0-5 + 6a-6d + the pre-pass deletion all DONE).
3. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-STEP6-DESIGN.md` §2 (why
   the snapshot cut is a store `List` at tick start, NOT a per-refresh `Get` — the
   reverted #2345/#2574 hazard) and §8 deletion-order steps 4-6.

**Where things stand.** The reconciler's decision reads AND every snapshot refresh are
fully on the typed `session.Info` snapshot (write-returns-`Info`); the blanket pre-pass,
both aggregating refreshes, and `refreshSessionInfo` are deleted. Nothing re-derives Info
from the raw working bead on the decision or refresh path. What remains is the LOCKSTEP
DROP: the raw `ordered []beads.Bead` / `beadByID` / `circuitSessionByIdentity` /
`sessionLookup` working set and the 13 `session.Metadata[k]=v` lockstep mirrors are still
physically present but READ-DEAD for decisions.

**Confirm a green baseline (isolated GOCACHE; the FULL cmd/gc suite times out at 600s —
use the reconciler subset):**
```
go build ./... && go vet ./cmd/gc/... ./internal/session/...
golangci-lint run ./cmd/gc/... ./internal/session/...   # expect 0
ISO=$(mktemp -d); GOCACHE=$ISO go test ./cmd/gc/ -timeout 25m -run 'Reconcile|Awake|Wake|Sleep|Pool|DrainAck|Recycle|Zombie|Heal|Drift|Churn|Stability|RateLimit|Named|Restart|Progress|Rollback|PendingCreate|MinFloor|Idle|MaxAge|Detach|Rebaseline|Relaunch|Quarantine|Circuit|Lifecycle|Session' -count=1; rm -rf "$ISO"
git checkout go.sum
```

**DO — the LOCKSTEP-DROP sequence (LOCKSTEP-DROP.md has the per-step detail; ONE
reviewed commit per step, re-grep line numbers before every edit):**
1. **CB block → `session.Store.CircuitState(id)`** — route the Phase-0.5 circuit-breaker
   restore off `circuitSessionByIdentity[identity]` (raw `*beads.Bead`) to the typed
   accessor; drop `circuitSessionByIdentity`. Smallest, self-contained.
2. **`advanceSessionDrainsWithSessionsTraced` off the raw bead** — convert its
   `completeDrain`/`cancelSessionDrainFor*` mutations to the typed store; retire
   `sessionLookup`/`beadByID`. Note the DEAD `ordered`/`sessionBeads` param in the prod
   call (§7).
3. **`buildAwakeInputFromReconciler` domain → ORDER-PRESERVING `[]Info`/`[]string`** (NOT
   `range infoByID` — slice order is load-bearing for `ComputeAwakeSet` SessionName
   last-write-wins) + the post-loop `target.session.Metadata["sleep_intent"]=""` write.
4. **`newSessionBeadSnapshot`/`resolvePreservedConfiguredNamedSessionTemplate` off a store
   source** (bucket-D, HARDEST — may need a store `List`).
5. **Drop the 13 lockstep mirrors + `beadByID` + `ordered []beads.Bead`; cut the tick-start
   snapshot build to the store.** Verify the exposure set is handled (reset_committed_at
   already excluded by `restartFold`; started_live_hash unread; buildPreparedStart residue
   inert — LOCKSTEP-DROP.md "exposure set").
6. **6e** — extend `snapshotInfoOnlyFiles` (frontdoor_di_guard_test.go) to forbid raw
   `session.Metadata[` on the decision path; add the reconciler files once raw-free.

**Discipline (unchanged from the pre-pass deletion):** convert each consumer + verify
BEFORE deleting its raw source; the reconciler suite is the byte-identity gate (a
wrong conversion flips an awake/drain decision and fails a test — these are no longer
masked). Keep the awake-scan slice-order invariant (never map-iterate the SessionBeads
domain). Run a fable adversarial review per non-trivial step (owner prefers fable, not
opus). Subagents are fine for the mechanical conversions (delegate + review the diff +
fable-review), as used for the pre-pass fold batches. Gates per commit: build · vet ·
golangci-lint 0 · gofmt · the reconciler subset. `git checkout go.sum` after. Commit AND
push `--no-verify`. Trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Never `tmux kill-server` / `go clean -cache` (`-testcache` ok). gascity Dolt LOCAL-ONLY
(no `bd dolt push`). #3839 stays DRAFT. Update LOCKSTEP-DROP.md + HANDOFF + memory
(`infra-beads-decoupling-plan.md`) as steps land.

---
