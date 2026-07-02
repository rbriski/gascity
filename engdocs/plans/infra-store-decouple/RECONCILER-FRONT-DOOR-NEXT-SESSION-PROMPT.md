# Next-session prompt — knock down the reconciler front-door backlog

Paste the block below into a fresh session.

---

Continue the **session reconciler front-door migration** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `990076d86`).

**Read first, in order:**
1. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-HANDOFF.md` — the
   authoritative handoff with the ordered backlog. It SUPERSEDES the
   `SPINE-FLIP-*` docs (the `InfoFromPersistedBead(*session)` re-derive approach is
   retired).
2. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-SPEC.md` — the
   review-hardened design (v2).

**The goal:** the reconciler stops touching raw `beads.Bead.Metadata`; all session
reads/writes go through the typed **`session.Store`** front door (renamed from
`InfoStore` this session). Store-centric (`store.Method(id, …)`), every mutation
persists, reads re-`Get` off a coherent snapshot. Owner: "we'll fix [perf] if hot" —
proceed with refresh-on-write, no upfront benchmark.

**GOVERNING SAFETY PRINCIPLE (do not violate):** never drop a
`session.Metadata[k]=v` lockstep until its dependent same-tick reads are on the
coherent snapshot; convert each write + its non-`continue` read-after-write as ONE
unit per commit. The byte-identical write oracle is BLIND to same-tick stale reads —
each lockstep drop needs a multi-session / read-after-write test.

**Confirm a green baseline:**
```
git rev-parse HEAD            # expect 990076d86 (or later)
go build ./cmd/gc/ ./internal/session/
go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestSnapshotInfoOnlyFilesStayOnInfoAccessors|TestFrontDoorStoreFreeFilesStayStoreFree' -count=1
git checkout go.sum
```

**DONE:** Step 0 — `InfoStore` → `session.Store` rename (`990076d86`).

**FIRST INCREMENT — Step 1 (missing `Info` mirrors):**
1. **Regenerate the exhaustive key inventory** — grep every `Metadata[...]` read
   reachable from the reconciler decision paths (`session_reconciler.go`,
   `session_reconcile.go`, `session_wake.go`, `compute_awake_bridge.go`) +
   `internal/session/lifecycle_projection.go`. Do NOT trust the handoff's list as
   complete.
2. Add the still-missing keys as raw-string `Info` mirrors (Generation/
   StartedConfigHash pattern — struct field + codec in `info_store.go` + a
   `TestSessionClassifierInfoEquivalence` stringChecks case each). Known-missing:
   `held_until`, `wait_hold`, `restart_requested`, `churn_count`, `wake_mode`,
   `session_name_explicit`. **`PoolSlot`/`CommonName`/`ConfiguredNamedIdentity`
   ALREADY EXIST — do not re-add.**
3. Add hold/quarantine/wait-hold + churn-spiral parity fixtures to the oracle.
   No call-site change this step (foundation only, like the 4c-foundation commit).

Then proceed through the backlog (Steps 2–6) in `RECONCILER-FRONT-DOOR-HANDOFF.md`,
one verified commit per item, in order. Do NOT skip ahead — the ordering is
correctness-load-bearing (dropping a lockstep before its reads move is the failure
mode a 4-lens review caught).

**Gates per commit:** `go build ./...` · `go vet` · `golangci-lint ./cmd/gc/...
./internal/session/...`=0 · byte-identical write assertions · a multi-session /
read-after-write test wherever a lockstep is dropped · `TestReconcileSessionBeads*`
(≥420s, split if it overloads) + pool/named/chaos/trace. `git checkout go.sum`
after. Commit AND push with `--no-verify`. Trailer:
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Never
`tmux kill-server` / `go clean -cache` (`-testcache` ok); gascity Dolt is
LOCAL-ONLY (no `bd dolt push`). #3839 stays DRAFT.

**Cautions:** read-only mapping agents have repeatedly read the wrong worktree
(`.worktrees/pack-crud`) — pin HEAD and restrict them to this worktree; verify their
line numbers. Update `RECONCILER-FRONT-DOOR-HANDOFF.md` (check the box) + memory
(`infra-beads-decoupling-plan.md`) as you land each step.

---
