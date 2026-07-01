# Next-session prompt — finish the non-work field-door cleanup (the cascade)

Paste the block below into a fresh session.

---

Continue the non-work-bead field-door cleanup on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `4acc591da`).

**Read first, in order:** `engdocs/plans/infra-store-decouple/P4-CASCADE-HANDOFF.md`
(the execution guide — the cascade structure, the foundation gaps, P5/P6, the
finish steps), then `P4-CONVERSION-CONTRACT.md` (per-site swap rules + sibling
table + RAW fidelity-field rules) and `NONWORK-BEAD-FIELDDOOR-PLAN.md`
(architecture). Confirm a green baseline:
`go build ./cmd/gc/` and
`go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestSessionSnapshotInfoEquivalence|TestSnapshotInfoOnlyFilesStayOnInfoAccessors' -count=1`.

**Principle (hard rule):** direct read of metadata/bead FIELDS on any NON-WORK
object (session/nudge/mail/order/graph) is illegal — only generic WORK beads read
raw. This is the precondition for a per-class backend swap.

**What's done:** foundation P1–P3 (Info codec + 23 `*Info` siblings + typed
snapshot accessors, equivalence-proven) and the P4 LOCALIZED slice + a P6
read-guard (`TestSnapshotInfoOnlyFilesStayOnInfoAccessors`, 4 files locked).

**What remains (NOT a mechanical per-file swap — do NOT trust any "safe
mechanical" framing):** the bulk is a coupled `[]beads.Bead`→`[]session.Info`
signature **cascade** through the pool-demand/work-scope/desired-state engine
(~8 files, atomic), plus the reconciler's `*beads.Bead session` threading, plus
~10 foundation gaps (new Info fields + siblings + equivalence cases), plus the
P5 `closeBead` landmine, plus P6 deletion + guard widening. The exact file/
function lists and the recommended order are in `P4-CASCADE-HANDOFF.md`.

**Do, in this order (each is ONE atomic, carefully-reviewed change — do NOT fan
parallel agents at a single connected component):**
1. providers MCP-key vertical slice (smallest complete add-field→sibling→
   equivalence→convert example; then add `providers.go` to `snapshotInfoOnlyFiles`).
2. the pool-demand `[]beads.Bead`→`[]session.Info` cascade (biggest unlock).
3. the reconciler `*session` Info-threading.
4. P5 `closeBead` split (recording-fake oracle; close-THEN-release; preserve
   skip-if-already-closed idempotence).
5. P6 delete dead bead classifiers/snapshot bead-methods (codec edge
   `session_bead_snapshot.go` is EXEMPT) + widen the guard to forbid
   `.Store().Store` in converted files.

**Method:** KEEP each original untouched + ADD the typed sibling + prove
equivalence, then migrate callers — the equivalence tests are the byte-identical
oracle. Build+equivalence-green per commit. `git checkout go.sum` after builds;
commit AND push with `--no-verify` (stale hooksPath + heavy pre-push hook);
trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Never `tmux kill-server` / `go clean -cache`; gascity Dolt LOCAL-ONLY.

**Gates before ready:** `go build ./...` · `go vet ./...` ·
`golangci-lint run ./cmd/gc/...` (0) · the equivalence + guard tests · targeted
subject suites. The build host is oversubscribed — targeted `-run` locally, CI
on dedicated runners is the byte-identical gate.

**Finish (only when #3839 CI is verified GREEN — no premature ready):**
- `gh pr checks 3839 --watch`
- ready (gh pr ready aborts on projectCards — use the API): `gh api graphql -f query='mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}' -f id=$(gh api repos/gastownhall/gascity/pulls/3839 --jq .node_id)`
- label: `gh api --method POST repos/gastownhall/gascity/issues/3839/labels -f 'labels[]=status/needs-review-auto'`

**Done =** every non-work consumer reads via `session.Info` (grep-clean of raw
snapshot accessors + `.Store().Store`), the guard forbids regression, full gates
+ #3839 CI green, #3839 ready + labeled. Update `memory/infra-beads-decoupling-plan.md`.

---
