# Release Gate: Tutorial 01 gc 1.0.0 Workaround Docs

Bead: ga-moh9o
Source bead: ga-6a84.1
Branch: builder/ga-6a84-1-docs-workaround
Base: origin/main 93e927a401249d27f127de587180980dbf4e0b6d
Reviewed commit: 9feeb81e7e9c13fdf63723cb49d217f0f26e0cef
Gate result: PASS

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-moh9o` notes contain `VERDICT: pass` and list all four acceptance criteria as met. |
| 2 | Acceptance criteria met | PASS | `git diff --stat --name-status origin/main...HEAD` shows only `docs/getting-started/quickstart.md` and `docs/tutorials/01-cities-and-rigs.md`. Both files include the clean `gc init` / `gc rig add` workaround, `gc doctor --fix`, `temporary workaround`, a link to `https://github.com/gastownhall/gascity/issues/1670`, and `newer builds include the code fix`. |
| 3 | Tests pass | PASS | `make check-docs` passed: `ok github.com/gastownhall/gascity/test/docsync 1.549s`. `git diff --check origin/main...HEAD` passed with no output. |
| 4 | No high-severity review findings open | PASS | Review notes contain no blocker or HIGH finding; the only note is a PASS verdict with docs-only scope and no security impact. |
| 5 | Final branch is clean | PASS | After the checklist commit, `git status --short --branch` printed only the branch header with `[ahead 1]`; there are no uncommitted files. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` succeeded with no conflict diagnostics, indicating no merge conflicts with `origin/main`. |

## Non-Blocking Observation

`make test-tutorial-goldens` was started because this patch touches tutorial
text. The target is a live acceptance suite with a 90-minute timeout, and it
entered unrelated existing drift before this patch's changed flow:

- `TestTutorialCommandInventoryMatchesPinnedDocs/docs/tutorials/07-orders.md`
  failed on the known `review-check` versus `pancakes-check` command inventory
  drift.
- `TestTutorial01Cities/PrimaryWizardFlow/cat_pack.toml` reported existing
  generated `pack.toml` shape drift: `[[named_session]]` instead of the pinned
  `[[agent]]` expectation.

The changed `docs/tutorials/01-cities-and-rigs.md` command inventory subtest
passed before the optional run was terminated to keep the release gate scoped to
the docs-only patch.
