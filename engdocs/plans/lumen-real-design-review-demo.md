# Spec: Real-Inference Lumen Design-Review Demo

## Objective

Prove the public Lumen path with a useful workflow rather than a scripted
session fixture. An operator runs `gc run review-quorum.lumen`, observes real
Claude and Codex sessions, and receives a revised copy of a real design plus
durable review evidence.

The demo reviews a disposable copy of
`engdocs/design/gc-reload-design.md` against a read-only snapshot of the same
committed repository. Two independent reviewers run in parallel. A synthesis
Agent consumes their artifacts and is the only Agent that edits the document.
A final verification Agent re-reads the revised document and fails the run if
the review artifacts or revision are not substantive.

## Runtime and commands

The City uses the tmux session provider and the built-in Claude and Codex
provider profiles. It uses Gas City's default managed bd+Dolt task store; no
beads backend override and no DoltLite process are part of the demo.

The operator-facing command, from the prepared demo City, is:

```bash
/path/to/gc run review-quorum.lumen \
  --route synthesisAgent \
  --input '{"document_path":"/absolute/path/design.md","repository_path":"/absolute/path/read-only-repository-snapshot","artifact_dir":"/absolute/path/review-artifacts","objective":"Make the design implementation-ready","lane_one_id":"implementation-realism","lane_two_id":"test-operability"}'
```

While it runs, another terminal can use:

```bash
/path/to/gc session list --state all
/path/to/gc session peek <session-name> --lines 100
```

The checked-in formula IR is regenerated with:

```bash
FORMULA_LANGUAGE=/absolute/path/to/formula-language
REPO=/absolute/path/to/gascity
npm --prefix "$FORMULA_LANGUAGE" -w @formula-language/core run check
(
  cd "$REPO/examples/lumen"
  node "$FORMULA_LANGUAGE/scripts/compile-lumen.mjs" \
    review-quorum.lumen > review-quorum.lumen.json
)
```

## Project structure

```text
examples/lumen/review-quorum.lumen       Authored workflow
examples/lumen/review-quorum.lumen.json  Compiler-generated sibling IR
examples/lumen/review-quorum-live/       Portable City config and demo guidance
  prompts/lumen-worker.md                Real inference worker contract
test/integration/                        Deterministic orchestration contract
test/acceptance/                         Opt-in live-inference acceptance
reports/lumen-demo/                      Local recording and run evidence
```

## Workflow contract

1. `laneOneAgent` and `laneTwoAgent` claim distinct ordinary Lumen work beads
   and run concurrently through real provider CLIs.
2. Each reviewer reads the document, checks claims against the explicit
   read-only repository snapshot, does not edit either input, atomically writes a
   distinct compact JSON artifact, stamps that JSON as `gc.output_json`, and
   closes its bead with an explicit `gc.outcome`.
3. `synthesisAgent` starts only after both reviewer steps settle. It reads both
   artifacts, writes a synthesis report, revises the copied document, records a
   before/after diff, stamps structured output, and closes explicitly.
4. `verifierAgent` starts after synthesis. It validates both lane artifacts,
   the synthesis evidence, and a meaningful document change. It writes
   `verification.json`; the formula passes only when this step closes with
   `gc.outcome=pass`.
5. Demand-created sessions return after their work drains. Review artifacts and
   the copied document remain durable after the sessions exit.

All task instructions name absolute artifact paths. Reviewers never write the
same file concurrently. Structured output uses stable fields: lane or role,
provider, verdict, summary, findings, evidence, artifact paths, and failure
classification.

## Testing strategy

- Compiler contract: the authored source compiles and its checked-in IR stays in
  sync.
- Deterministic integration: scripted workers continue to prove City enqueue,
  concurrent routing, dependency ordering, terminal outcome, and session
  return. The test is labeled as orchestration plumbing, not inference proof.
- Live acceptance: an opt-in `acceptance_c` path requires authenticated provider
  CLIs and checks real session visibility, non-empty structured findings,
  provider provenance, document changes, synthesis evidence, final verification,
  terminal pass, and returned sessions.
- Recording: require a clean tracked tree and a binary built from that exact
  commit, capture the isolated City with asciinema, then render one uniform
  continuous speed-up. The recording visibly identifies real inference and
  default managed bd+Dolt.

## Boundaries

- Always: use a disposable document copy, absolute artifact paths, default
  managed bd+Dolt, tmux-scoped session cleanup, and explicit failure metadata.
- Ask first: changing scatter aggregation semantics, adding a new public CLI
  flag, changing provider authentication, or mutating the source design.
- Never: use scripted workers as inference evidence, use DoltLite, expose
  credentials, kill the default tmux server, or let concurrent reviewers edit
  the document.

## Success criteria

- The literal `gc run review-quorum.lumen <args>` front door reaches a terminal
  `outcome: pass` through the current City's orchestrator.
- One `gc session list` snapshot shows both real reviewer sessions concurrently;
  later snapshots show real synthesis and verification sessions.
- `gc session peek` shows provider activity rather than a barrier script.
- Both review JSON files contain substantive, document-specific findings and
  identify distinct real providers.
- The synthesis Agent changes the copied design meaningfully and writes durable
  synthesis plus diff artifacts.
- The verification Agent inspects the revised document and produces a passing
  durable verdict.
- All four sessions return after the run.
- The terminal recording is a uniformly accelerated representation of the real
  run, with no phase deletion or scripted replacement.

## Open questions

None for this milestone. Native scatter-output aggregation remains separate
engine work; this demo uses durable reviewer artifacts while also preserving
each review in its work bead's `gc.output_json`.
