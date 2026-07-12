# Reconciler pre-G0 candidate — exact-object review

| Field | Value |
|---|---|
| Date | 2026-07-12 |
| Reviewer | Independent fresh-context agent (`final_exact_reviewer`) |
| Contract commit | `1eca1e05eccc081ad4d359eca6087d8681e342e8` |
| Execution base | `5774497b00ecd0a6c072058069b77fc5874268f2` |
| Contract tree | `b5c50bfaef8ecb586e849a098d77b4050ea89945` |
| Execution-base tree | `41eb14d20527ca0f89677e9e91c9e4ace04a0b70` |
| Plan blob | `baa2cb29560d4d5161c33a880a2eba7fe04938bd` |
| Acceptance-matrix blob | `f960809fd9a28316a08d25c785e4284190fe0950` |
| Scope | P1.0A, P1.0C, P1.0D, P1.1A and their cited RC/INV rows |
| Verdict | **APPROVE** |

The reviewer independently resolved the exact commit types, direct parent,
trees, and file blobs with Git replacement objects disabled; checked the four
pre-G0 slices for internal consistency and feasibility against the execution-
base source; and reported no concrete blocker.

The exact-object source trace covered all factory-selected `Ready` backends,
including the preflight-selected `NativeDoltStore`, the exec protocol, wrappers,
one coherent snapshot across the pass, mutation-boundary invalidation, bounded
snapshot ownership, and error identity. It covered cache-backed commit
ambiguity, dirty marking, and per-row mutation sequencing that prevents a stale
prime or reconciliation refresh from installing pre-write data after a failed
heal.

The CLI/tmux trace covered every production `stop` and `stop-force` caller,
exact tri-state wire classification, exact `ok\n` acknowledgement, fail-closed
post-connect ambiguity, foreground and supervisor lock acquisition before
materialization or provider effects, exactly-once lock transfer, retained
ownership through post-controller store shutdown, and direct-mode joining of
every entered interrupt or stop call across both outer and per-target timeout
boundaries. It also confirmed that isolated tmux testing leaves the default
personal server untouched and that the contract makes no cross-path or
provider-global admission claim.

The convergence trace covered failed-write state preservation, the initial-
state plus rollback-state double fault, fresh repair of the discoverable open
partial root without pouring duplicate work, valid complete-wisp adoption,
manual approve and stop terminal paths, pending-successor lookup and cleanup
errors, child-query uncertainty, marker-last close ordering, explicit-ID closed-
root repair, terminal event ordering, joined rollback errors, and the honest
nonclaims for empty-memory closed-root discovery and guaranteed publication.

This is an exact-object technical review, not a cryptographic signature, G0
ratification, exact-digest owner approval, or permission to merge without the
human-review precondition recorded in `PRE_G0_CANDIDATE_MANIFEST.json`.
