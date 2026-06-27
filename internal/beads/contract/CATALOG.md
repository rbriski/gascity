# Beads ↔ Gas City contract test catalog (consumer side)

This is the enumerated inventory of the **Gas City** half of the cross-version
contract-test system: every test, the contract domain it covers, and the
historical incident it guards against. The **producer** half lives in beads at
`cmd/bd/protocol/CATALOG.md`. The system design is
`engdocs/design/beads-gascity-contract-test-system.md`.

The two sides are bridged by a committed golden-JSON **corpus** generated from
real `bd` output (authoritative copy in beads `cmd/bd/protocol/testdata/corpus/`,
vendored here at `internal/beads/testdata/corpus/`). Refresh the vendored copy
with `make sync-bd-corpus BD_CORPUS_TAG=vX.Y.Z`.

## Cross-version matrix

The contract runs against more than one `bd` so version-gated code is actually
exercised (the #3135 class). Dolt backend only.

| Cell | bd | gc | Runs in | Required |
| ---- | -- | -- | ------- | -------- |
| prev × HEAD | v1.0.4 (min-supported release) | HEAD | gascity `preflight-acceptance` | yes |
| current × HEAD | `BD_CURRENT_REF` build-from-source | HEAD | gascity `contract-acceptance-current` | yes |
| main-HEAD × HEAD | beads `main` (live) | HEAD | gascity `contract-radar-bd-head` | no (radar) |
| HEAD × prev/cur gc | HEAD | pinned gc release / `edge` | beads CI (reverse cells) | mixed |

Pins live in `deps.env` (`BD_PREV_VERSION`, `BD_CURRENT_VERSION`, `BD_CURRENT_REF`)
and are kept consistent by `scripts/bd_version_pin_test.go`.

## Test inventory

| Test / artifact | Domain | What it pins |
| --------------- | ------ | ------------ |
| `scripts/bd_version_pin_test.go` | version-compat | `BD_VERSION` ↔ `bdMinVersion` ↔ `bdReadyProjectionMinVersion` ↔ `bd_compatibility` enum ↔ install-script SHAs, all one source of truth |
| `internal/beads/testdata/corpus/**` (golden) | json-output-shapes | the canonical `--json` blobs for 17 commands × {flat, envelope}; the cross-version diff anchor |
| `internal/beads/corpus_decoder_test.go` — `TestCorpusFlatShowDecodes` | json-output-shapes | `show` array → `bdIssue`, nested dependency shape |
| …`TestCorpusFlatListReadyDecode` | json-output-shapes, ready-projection | `list`/`ready` arrays decode through `parseIssuesTolerant` |
| …`TestCorpusCreateDecodesSingleIssue` | json-output-shapes | `create` bare-object decode |
| …`TestCorpusMutationBlobsDecode` | json-output-shapes, store-mutations | `update`/`close`/`reopen` arrays; **metadata coercion** (`phase=2` int → `"2"` via `StringMap`) |
| …`TestCorpusErrorEnvelopeClassified` | exit-codes-and-errors | `{error, schema_version}` not-found envelope → `bdStdoutErrorDetail` + `isBdNotFound` |
| …`TestCorpusV2EnvelopeIsForwardIncompatible` | json-output-shapes | the v2.0 `{schema_version, data}` envelope migration canary |
| …`TestCorpusFieldsAreModeledOrExplicitlyIgnored` | json-output-shapes | every bd `show` field is decoded or explicitly ignored (new-capability detector) |
| `internal/beads/error_classifier_contract_test.go` | exit-codes-and-errors | the four free-text classifiers: not-found, `bd sql` unsupported, claim-conflict, silent-fallback |
| `internal/beads/beadstest/conformance.go` | store-transactional-semantics | the `Store` interface contract across Mem/File/Exec/Native backends |
| `internal/beads/beadstest/conformance_skips.go` | maintainability | governed, expiring opt-out ledger (no silent skips) |
| `internal/beads/contract/*.go` | file-schemas, topology-and-config, auth | `.beads/metadata.json` / `config.yaml` / identity, endpoint resolution |
| `test/acceptance/beads_cli_contract_test.go` (`acceptance_a`) | cli-command-surface | live bd subset gc calls; runs in both matrix cells |

## Incident → guard traceability

Each historical drift incident, and the test that now catches its class:

| Incident | What broke | Guarded by |
| -------- | ---------- | ---------- |
| **#3135** | `bd list --skip-labels` / ready-projection emitted ahead of the pinned floor | `bd_version_pin_test` + the `bd v1.0.5`/current matrix cell actually runs the gated path |
| **isBdNotFound plural** | stdout envelope `"no issues found"` (plural) unmatched | `TestCorpusErrorEnvelopeClassified` + `TestIsBdNotFoundContract` |
| **corpus `commit` drift** | `bd version --json` `commit` field varies by build env | corpus canonicalizer drops build-provenance + the golden gate |
| **#1726** | bd emitted literal `None` on a `--json` path | corpus shape blobs + `parseIssuesTolerant` tolerance |
| **quad341 / v0.62** | bd removed commands gc depended on | the corpus golden gate (a vanished command fails regen) + `acceptance_a` |
| **sa-41j3kp / #1930** | silent fallback to stale `issues.jsonl` import (write loss) | `TestBdSilentFallbackContract` |
| **claim race** | lost claim misclassified | `TestIsBdClaimConflictContract` |
| **`bd sql` embedded** | ready-projection enrichment must degrade, not error | `TestIsBdSQLUnsupportedContract` |
| **metadata coercion** | `--set-metadata k=2` emitted as integer | `TestCorpusMutationBlobsDecode` + `StringMap.UnmarshalJSON` |
| **bd anchor drift** | four bd version anchors edited independently | `bd_version_pin_test` |

## Coverage map

- **Covered:** json-output-shapes (17 commands), exit-codes-and-errors (4
  classifiers + not-found envelope), version-compat (pin consistency + the
  cross-version matrix), store conformance, file-schemas/topology/auth (existing
  `contract/` package).
- **Partial:** `#3948` close exit-0-revert and `ga-5mym` per-command cost are
  exercised only by the live `acceptance_a` cells, not pinned offline;
  store-transactional graph-atomicity is conformance-level, not live-bd.
- **Gap (tracked):** the beads-side `bd-HEAD × gc-edge` reverse radar and the
  required `gc-contract` cell (need the gc-city smoke harness + the live `edge`
  release); `bd_corpus_drift_test.go` (needs a beads release shipping the
  corpus); doltlite read-schema parity (descoped).

## Extending when bd changes

1. **bd adds/changes a `--json` shape** → beads `make corpus-regen`, review the
   diff, **bump `JSONSchemaVersion`**; the gascity offline decoder + field-
   coverage tests then fail until updated, and the v2-envelope canary flips on
   the envelope migration.
2. **bd adds a command gc will call** → add a capture to `CorpusPlan` (beads) and
   a decode assertion here.
3. **bd rewords an error string** → the live matrix cells catch it; update the
   matching `error_classifier_contract_test.go` table.
4. **A new bd version becomes min-supported / current** → bump `deps.env` pins
   (one `bd_version_pin_test`-guarded PR) and `make sync-bd-corpus`.
