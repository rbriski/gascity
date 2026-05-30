# Release Gate: BD_VERSION v1.0.5

Verdict: PASS

- Deploy bead: `ga-pxbjxw`
- Source review bead: `ga-wl0hix`
- Branch: `builder/ga-pxbjxw-bd-version-v1-0-5-clean`
- Candidate HEAD before gate commit: `d1d3e8f61395e17311f90c132fb5b39c5b3d2276`
- Current `origin/main`: `cd7fd56869c025c6970a551e071728b231cad9c8`
- Merge base with `origin/main`: `f272db921759b3d8700d3eb7d79961ce24bc52eb`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses the deployer role release criteria from `gc prime` plus the repo testing guidance in `TESTING.md`.

## Summary

This branch bumps the pinned `bd` archive version used by CI and related workflows from v1.0.4 to v1.0.5, adds SHA-256 pins for the v1.0.5 release archives, and removes Trivy suppressions for vulnerabilities fixed by the new `bd` build.

## Release Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `bd show ga-wl0hix` is closed with `VERDICT: PASS` and reviewer notes covering commit `a0dabf306`; deploy bead `ga-pxbjxw` records the clean rebuilt branch at `d1d3e8f61395e17311f90c132fb5b39c5b3d2276`. |
| 2 | Acceptance criteria met | PASS | Deployer rechecked the final branch scope: 9 workflow `BD_VERSION` references now use v1.0.5, all 4 platform archive SHA-256 pins are present in `.github/scripts/install-bd-archive.sh`, and the patched `bd` CVE suppressions were removed from `.trivyignore.yaml` while unrelated suppressions were preserved. |
| 3 | Tests pass | PASS | `bash -n .github/scripts/install-bd-archive.sh`, `git diff --check origin/main...HEAD`, `GOTOOLCHAIN=auto make build`, `GOTOOLCHAIN=auto make test`, `GOTOOLCHAIN=auto go vet ./...`, and `./bin/gc version` all exited 0. |
| 4 | No high-severity review findings open | PASS | Review notes list security/spec/style as PASS and no HIGH findings. The earlier deploy gate failure was branch hygiene only; the builder rebuilt this clean branch and rerouted it for deploy. |
| 5 | Final branch is clean | PASS | The clean deploy worktree had no uncommitted changes before this gate file was added. After committing the gate file, deployer re-runs `git status --short --branch` before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `804c8df7409a1ca47736418c951ee80d2e31e97a`, confirming no merge conflicts with current `origin/main`. |
| 7 | Single feature theme | PASS | The PR diff is one CI/security maintenance theme: update `bd` archive pins and remove obsolete vulnerability suppressions. Touched paths are `.github/scripts/install-bd-archive.sh`, workflow files, and `.trivyignore.yaml`. |

## Acceptance Evidence

| Check | Evidence |
|-------|----------|
| Workflow version references | `.github/workflows/ci.yml`, `mac-regression.yml`, `nightly.yml`, `ollama-acceptance-c.yml`, `rc-gate.yml`, and `review-formulas.yml` pin `BD_VERSION` to v1.0.5. |
| Archive integrity pins | `.github/scripts/install-bd-archive.sh` includes v1.0.5 SHA-256 entries for `linux_amd64`, `linux_arm64`, `darwin_amd64`, and `darwin_arm64`. |
| Obsolete suppressions removed | `.trivyignore.yaml` no longer suppresses the `bd`-fixed `go-jose` and `thrift` findings; non-`bd` suppressions remain. |
| Scope cleanliness | `git diff --name-only origin/main...HEAD` lists only the 8 expected CI/script/security-suppression files. |

## Verification Commands

| Command | Result |
|---------|--------|
| `gh auth status` | PASS: authenticated as `quad341` with repo/workflow scopes. |
| `git fetch origin main` | PASS |
| `git diff --name-only origin/main...HEAD` | PASS: expected 8-file CI/security scope. |
| `git diff --check origin/main...HEAD` | PASS |
| `git merge-tree --write-tree origin/main HEAD` | PASS |
| `bash -n .github/scripts/install-bd-archive.sh` | PASS |
| `GOTOOLCHAIN=auto make build` | PASS |
| `./bin/gc version` | PASS: printed `dev`. |
| `GOTOOLCHAIN=auto make test` | PASS: `observable go test: PASS`. |
| `GOTOOLCHAIN=auto go vet ./...` | PASS |

## Push Target

Remote policy will be resolved immediately before push. If `origin` rejects a dry-run push with a permission error, the branch will be pushed to `fork` and the PR will use `quad341:builder/ga-pxbjxw-bd-version-v1-0-5-clean` as the head.
