# S09b — Table-driven Info codec + cmd/gc sleep-reason migration

Follow-on to S09 (#4033, `be1d46258`, merged to main 2026-07-07), which landed
`internal/session/sleep_reason.go` (`type SleepReason string` + 15 constants +
`IsDeliberateSleepReason`) and the beadmeta dispatch keys
(`MoleculeIDMetadataKey = "molecule_id"`, `MergeStrategyMetadataKey`,
`WorkflowIDMetadataKey`, target-branch). S09b is the two deliberately-unfolded
follow-ups named in that commit message:

- **Part A (mechanical, low risk):** migrate cmd/gc's parallel sleep-reason
  string literals and shadow constants (`sleepReasonCityStop`,
  `sleepReasonRuntimeMissing`, `sleepReasonProviderTerminalError`, plus raw
  `"idle-timeout"` / `"drained"` / `"wait-hold"` / `"failed-create"` / `"idle"`
  literals) onto `session.SleepReason*` constants.
- **Part B (parity-gated):** the deferred Part-1 table-driven Info codec —
  replace the parallel per-key mapping in `InfoFromPersistedBead`
  (`internal/session/info_store.go:23-149`) and the 180-line `ApplyPatch`
  switch (`internal/session/info_apply_patch.go:37-217`) with ONE runtime
  table of typed getter/setter closures, and repoint the raw
  `"molecule_id"` readers in t3bridge/runproj/api onto
  `beadmeta.MoleculeIDMetadataKey`.

**IMPORTANT baseline note:** this worktree's HEAD does NOT contain #4033
(`git merge-base --is-ancestor be1d46258 HEAD` fails). The implementation
branch for S09b MUST be cut from `origin/main` (≥ `be1d46258`), not from this
worktree's branch, or `session.SleepReason` will not exist.

## Target design

### Part B: the codec table (`internal/session/info_codec.go`, new file)

One package-private table drives both directions of the metadata⇄Info codec:

```go
// infoKeySpec is one metadata key's codec: how a raw metadata value becomes
// Info fields (project) and how a patch value folds onto an existing Info
// (the SAME closure — fold == re-projection by construction).
type infoKeySpec struct {
    key string                     // exact on-store metadata key (byte-identical to today)
    set func(info *Info, v string) // typed setter: writes ALL Info fields derived from this key
}

// infoKeyCodec is the single source of truth. Ordering is irrelevant for
// correctness (each key writes disjoint Info fields); keep it grouped in the
// same clusters as today's struct literal for reviewability.
var infoKeyCodec = []infoKeySpec{ ... }

// infoKeyIndex is built once in init() for ApplyPatch's map lookup.
var infoKeyIndex = map[string]*infoKeySpec{}
```

Key properties:

- **One `set` closure per key, used by BOTH `InfoFromPersistedBead` and
  `ApplyPatch`.** The projection becomes
  `for _, spec := range infoKeyCodec { spec.set(&info, b.Metadata[spec.key]) }`;
  `ApplyPatch` becomes `if spec, ok := infoKeyIndex[key]; ok { spec.set(&info, v) }`.
  The `TestInfoApplyPatchMatchesReprojection` equivalence oracle becomes true
  by construction for every table-driven key, but is KEPT as the parity gate.
- **Setters are total over the empty string.** In the projection direction a
  missing metadata key reads as `""` from `b.Metadata[...]`; in the patch
  direction `""` is the empty-string-clear contract. Every setter must
  therefore produce the correct cleared state for `v == ""` (e.g.
  `wake_attempts` setter sets `WakeAttempts = 0` when Atoi fails — matching
  today's ApplyPatch `else` branch AND today's projection no-op-on-zero-value,
  which agree because Info starts zero-valued in the projection direction).
- **Multi-field keys stay single-entry.** Keys that derive multiple fields
  (`dependency_only` → bool + raw mirror; `manual_session`;
  `pending_create_claim`; `wake_attempts` → int + raw mirror;
  `session_name` → SessionNameMetadata + fallback-defaulted SessionName;
  `state` → MetadataState + normalized/closed-blanked State) write all their
  fields in one setter, exactly as both existing sites do today.
- **Cross-field keys (`provider`, `transport`) are table entries whose setters
  read the OTHER field's current raw mirror** (`info.TransportMetadata` /
  `info.Provider`) — legal because setters receive `*Info` with all
  previously-applied state, and byte-identical to today's ApplyPatch cases.
  In the projection direction this introduces an ORDER dependency
  (see Invariants I6): `provider` and `transport` raw mirrors must be applied
  before the derived `Transport` computation. Resolved by making `transport`'s
  setter assign `info.TransportMetadata = v` then
  `info.Transport = normalizeTransport(info.Provider, v)`, and ordering
  `provider` (raw only + derive) before `transport` in the table — with the
  final derived value computed identically regardless of which of the two
  arrives in a patch (matching today's two ApplyPatch cases). The projection
  loop then yields exactly `transportFromMetadata(b)`'s result (verified in
  the parity test).
- **Genuine non-table special cases stay hand-written** in
  `InfoFromPersistedBead`, exactly as the S09 proposal says: bead-level fields
  (ID, Type, Title, Labels, CreatedAt), `Closed = (b.Status == "closed")` and
  the closed-blanking of State, the `session_name` → `sessionNameFor(b.ID)`
  fallback (parameterized on `info.ID`, so it lives in the setter reading
  `info.ID` — set bead-level fields FIRST), and `AliasHistory` (projection
  uses `AliasHistory(b.Metadata)` over the whole map; patch uses
  `normalizeAliasList(strings.Split(v, ","), "")` — but `AliasHistory(m)` is
  literally defined as `normalizeAliasList(strings.Split(m[aliasHistoryMetadataKey], ","), "")`
  with a `len(m)==0 → nil` fast path (`internal/session/alias.go:9-14`), so
  the two directions compute identically for any single key value and
  `alias_history` CAN be a plain table entry with setter
  `info.AliasHistory = normalizeAliasList(strings.Split(v, ","), "")`; the
  `len(metadata)==0` fast path also yields nil through the setter since
  `Split("", ",") = [""]` normalizes to nil. See E-AH.)
- **`MarkClosed` is untouched.** Status-close folding is not metadata codec.
- **Default/unknown keys remain no-ops in ApplyPatch** (keys with no Info
  field: `live_hash`, `startup_dialog_verified`, `env.*`, …) — map-lookup miss
  falls through, byte-identical to today's `default:` branch.

### Part B2: molecule_id reader repoint (mechanical)

`internal/runtime/t3bridge/provider.go` (4 sites), `internal/runproj/summary.go`
(1 site), `internal/api/handler_beads.go` (1 site in the `{"molecule_id",
"workflow_id"}` key list) switch their raw `"molecule_id"` literals to
`beadmeta.MoleculeIDMetadataKey`. Zero behavior change (the constant IS
`"molecule_id"`). beadmeta is a leaf stdlib-only package, so no import-cycle
or layering risk. (`handler_beads.go`'s `"workflow_id"` literal is a
NON-prefixed key — beadmeta's `WorkflowIDMetadataKey` is `"gc.workflow_id"`,
a DIFFERENT string — leave `"workflow_id"` as a literal or introduce a
separate constant; do NOT substitute the gc.-prefixed constant. See risk R3.)

### Part A: cmd/gc sleep-reason migration (mechanical)

Delete cmd/gc's shadow constants and repoint every literal onto
`session.SleepReason*`, comparing via `string(session.SleepReasonX)` where the
surrounding code traffics in `string` (Info.SleepReason stays `string` in this
item — retyping the Info field is out of scope). Files: `cmd_stop.go`,
`session_reconcile.go`, `session_state_helpers.go`, `compute_awake_set.go`,
`session_wake.go`, `session_lifecycle_parallel.go`, `city_runtime.go` (writer
call sites only; the metadata KEY literal `"sleep_reason"` is unchanged).

## Current behavior (site-by-site enumeration)

Line numbers reference `origin/main` (post-#4033). The worktree copies of
`info_store.go` / `info_apply_patch.go` differ from main only by one comment
in `info_store.go` (sessionMatchesFilters doc), verified by
`git diff origin/main -- internal/session/info_store.go internal/session/info_apply_patch.go`.

### B. Complete Info key inventory (the parity contract)

Every metadata key the codec handles, with its EXACT per-key semantics in both
directions. "P" = projection (`InfoFromPersistedBead`, info_store.go), "A" =
ApplyPatch (info_apply_patch.go). Unless noted, both directions are the
identical assignment `field = v` (verbatim, NO trimming) and the key becomes a
trivial table entry.

**E-1. Verbatim string pass-through — single field (59 keys).** Both P and A
assign the raw value. Table entry: `set: func(i *Info, v string) { i.Field = v }`.

| key | Info field |
|---|---|
| `template` | Template |
| `alias` | Alias |
| `agent_name` | AgentName |
| `command` | Command |
| `work_dir` | WorkDir |
| `session_key` | SessionKey |
| `resume_flag` | ResumeFlag |
| `resume_style` | ResumeStyle |
| `resume_command` | ResumeCommand |
| `continuation_epoch` | ContinuationEpoch |
| `sleep_reason` | SleepReason |
| `NamedSessionIdentityMetadata` (const) | ConfiguredNamedIdentity |
| `NamedSessionModeMetadata` (const) | ConfiguredNamedMode |
| `common_name` | CommonName |
| `pool_slot` | PoolSlot |
| `session_origin` | SessionOrigin |
| `MCPIdentityMetadataKey` (const) | MCPIdentity |
| `MCPServersSnapshotMetadataKey` (const) | MCPServersSnapshot |
| `provider_terminal_error` | ProviderTerminalError |
| `session_health` | HealthState |
| `session_health_reason` | HealthReason |
| `beadmeta.TriggerBeadIDMetadataKey` | TriggerBeadID |
| `beadmeta.TriggerBeadStoreRefMetadataKey` | TriggerBeadStoreRef |
| `beadmeta.BrainParentSIDMetadataKey` | BrainParentSID |
| `beadmeta.PackMetadataKey` | Pack |
| `pending_create_started_at` | PendingCreateStartedAt |
| `quarantined_until` | QuarantinedUntil |
| `continuity_eligible` | ContinuityEligible |
| `last_woke_at` | LastWokeAt |
| `state_reason` | StateReason |
| `creation_complete_at` | CreationCompleteAt |
| `continuation_reset_pending` | ContinuationResetPending |
| `ResetCommittedAtKey` (const) | ResetCommittedAt |
| `generation` | Generation |
| `started_config_hash` | StartedConfigHash |
| `pin_awake` | PinAwake |
| `held_until` | HeldUntil |
| `wait_hold` | WaitHold |
| `churn_count` | ChurnCount |
| `wake_mode` | WakeMode |
| `sleep_intent` | SleepIntent |
| `instance_token` | InstanceToken |
| `detached_at` | DetachedAt |
| `CurrentBeadIDKey` (const) | CurrentlyProcessingBeadID |
| `core_hash_breakdown` | CoreHashBreakdown |
| `started_provision_hash` | StartedProvisionHash |
| `started_launch_hash` | StartedLaunchHash |
| `started_live_hash` | StartedLiveHash |
| `config_drift_deferred_at` | ConfigDriftDeferredAt |
| `config_drift_deferred_key` | ConfigDriftDeferredKey |
| `attached_config_drift_deferred_at` | AttachedConfigDriftDeferredAt |
| `attached_config_drift_deferred_key` | AttachedConfigDriftDeferredKey |
| `stranded_event_emitted_at` | StrandedEventEmittedAt |
| `session_name_explicit` | SessionNameExplicit |
| `wake_request` | WakeRequest |
| `restart_requested` | RestartRequested |
| `session_id_flag` | SessionIDFlag |
| `template_overrides` | TemplateOverrides |
| `provider_kind` | ProviderKind |

**E-2. Bool + raw-mirror pairs (6 keys).** Both directions:
`Bool = strings.TrimSpace(v) == "true"`; where a raw mirror exists it gets the
verbatim value. NOTE the two mirror-less bools: `NamedSessionMetadataKey` and
`pool_managed` derive ONLY the bool (no raw mirror field exists) — the table
setter must not invent one.

| key | fields set |
|---|---|
| `NamedSessionMetadataKey` (const) | ConfiguredNamedSession (bool only) |
| `pool_managed` | PoolManaged (bool only) |
| `session_drainable` | Drainable (bool only) |
| `dependency_only` | DependencyOnly (bool) + DependencyOnlyMetadata (raw) |
| `manual_session` | ManualSession (bool) + ManualSessionMetadata (raw) |
| `pending_create_claim` | PendingCreateClaim (bool) + PendingCreateClaimMetadata (raw) |

(6 rows; `session_drainable` counted with the mirror-less bools.)

**E-3. `session_name` (fallback-defaulted, 2 fields).**
- P (info_store.go:24-27, 48, 92): `SessionNameMetadata = raw`;
  `SessionName = raw`, or `sessionNameFor(b.ID)` when raw is `""`.
- A (info_apply_patch.go:40-46): identical, using `info.ID`.
- Table setter: `i.SessionNameMetadata = v; if v == "" { i.SessionName = sessionNameFor(i.ID) } else { i.SessionName = v }`.
  ORDER REQUIREMENT: projection must populate `info.ID` before running the
  table (bead-level fields are set in the hand-written prologue).

**E-4. `state` (normalize + closed-blank, 2 fields).**
- P (info_store.go:30-33, 91): `MetadataState = raw` (verbatim);
  `State = normalizeInfoState(State(raw))`, then blanked to `""` if
  `b.Status == "closed"`. `normalizeInfoState` (manager.go:376-384) maps
  `awake→active`, `drained→asleep`, else identity.
- A (info_apply_patch.go:47-53): identical, reading carried-forward
  `info.Closed` (ApplyPatch never flips Closed).
- Table setter: `i.MetadataState = v; if i.Closed { i.State = "" } else { i.State = normalizeInfoState(State(v)) }`.
  ORDER REQUIREMENT: projection sets `info.Closed` in the prologue before the
  table runs. Identical logic then serves both directions.

**E-5. `provider` / `transport` (cross-field pair).**
- P (info_store.go:44-45, 99): `Provider = raw provider`;
  `TransportMetadata = raw transport`;
  `Transport = transportFromMetadata(b) = normalizeTransport(meta["provider"], meta["transport"])`
  (manager.go:439-451: transport wins if non-empty; else `"acp"` iff
  provider == `"acp"`; else `""`).
- A (info_apply_patch.go:60-65): `provider` patch sets Provider then
  `Transport = normalizeTransport(v, info.TransportMetadata)`; `transport`
  patch sets TransportMetadata then
  `Transport = normalizeTransport(info.Provider, v)`.
- Table setters (each reads the sibling's current field):
  `provider`: `i.Provider = v; i.Transport = normalizeTransport(v, i.TransportMetadata)`;
  `transport`: `i.TransportMetadata = v; i.Transport = normalizeTransport(i.Provider, v)`.
- PARITY ARGUMENT (projection): running these two setters in ANY order over a
  zero/populated Info yields `normalizeTransport(provider, transport)` — after
  both run, Transport was last computed with both true raw values in scope.
  Order between them still fixed in the table (provider first) for
  determinism; the property test covers both empty/non-empty quadrants.

**E-6. `wake_attempts` (int + raw mirror).**
- P (info_store.go:137, 140-142): `WakeAttemptsMetadata = raw`; `WakeAttempts`
  set ONLY when `strconv.Atoi(raw)` succeeds (zero-valued Info means the
  no-set == set-0 for projection).
- A (info_apply_patch.go:193-199): raw mirror; Atoi success sets n, failure
  EXPLICITLY sets 0 (must clear a carried-forward value).
- Table setter (A's form is the total one, and agrees with P on a fresh Info):
  `i.WakeAttemptsMetadata = v; if n, err := strconv.Atoi(v); err == nil { i.WakeAttempts = n } else { i.WakeAttempts = 0 }`.
  Note Atoi accepts leading `+`/`-` but NOT whitespace — do not add trimming.

**E-7. `MetadataLastNudgeDeliveredAt` (const key; RFC3339 time).**
- P (info_store.go:143-147): parse `time.Parse(time.RFC3339, strings.TrimSpace(raw))`
  only when trimmed raw non-empty AND parse succeeds; else leave zero.
- A (info_apply_patch.go:202-208): FIRST resets to `time.Time{}`, then same
  trimmed-parse. (Reset needed to clear carried-forward state.)
- Table setter (total form): `i.LastNudgeDeliveredAt = time.Time{}; if raw := strings.TrimSpace(v); raw != "" { if p, err := time.Parse(time.RFC3339, raw); err == nil { i.LastNudgeDeliveredAt = p } }`.
  Agrees with P on fresh Info (reset-to-zero is a no-op there).

**E-AH. `alias_history` (aliasHistoryMetadataKey const).**
- P (info_store.go:97): `AliasHistory(b.Metadata)` ≡
  `normalizeAliasList(strings.Split(meta[aliasHistoryMetadataKey], ","), "")`
  (alias.go:9-14; the `len(metadata)==0 → nil` fast path is equivalent since
  splitting `""` normalizes to nil).
- A (info_apply_patch.go:129-130): `normalizeAliasList(strings.Split(v, ","), "")`.
- Identical computation → plain table entry. Slice freshness note: the setter
  allocates a new slice per call, same as both current sites; nil for empty.

**E-9. Non-table (hand-written prologue in InfoFromPersistedBead).**
Bead-level, not metadata-derived; ApplyPatch already carries them forward
untouched: `ID = b.ID`, `Type = b.Type`, `Title = b.Title`,
`Labels = b.Labels`, `CreatedAt = b.CreatedAt`,
`Closed = (b.Status == "closed")`. These MUST be assigned before the table
loop (E-3/E-4 read `info.ID` / `info.Closed`).

**E-10. ApplyPatch `default:` branch.** Keys with no Info field (`live_hash`,
`startup_dialog_verified`, `env.*`, …) are ignored — preserved automatically
by map-lookup miss. No table entry may be added for them.

### A. cmd/gc sleep-reason sites (grep of origin/main; migrate VALUE literals only)

Shadow constants to delete and replace with `session.SleepReason*`:

- `cmd/gc/cmd_stop.go:57` — `const sleepReasonCityStop = "city-stop"` →
  `session.SleepReasonCityStop`. Users: cmd_stop.go:369 (`SetMarker(..., "sleep_reason", ...)`),
  session_state_helpers.go:78,95, session_lifecycle_parallel.go:3042,3049.
- `cmd/gc/session_reconcile.go:42` — `const sleepReasonRuntimeMissing = "runtime-missing"`
  → `session.SleepReasonRuntimeMissing` (which aliases
  `LifecycleReasonRuntimeMissing`). Users: session_reconcile.go:1233,
  session_state_helpers.go:78,95.
- `cmd/gc/session_reconcile.go:47` — `const sleepReasonProviderTerminalError = "provider-terminal-error"`
  → `session.SleepReasonProviderTerminalError`. Users:
  session_reconcile.go:791, session_state_helpers.go:79,96.

Raw sleep-reason VALUE literals (per file, origin/main line numbers):

- `session_state_helpers.go:78,95` — case lists `"idle", "idle-timeout", ..., "failed-create", ...`
  → `SleepReasonIdle/IdleTimeout/FailedCreate` (string-converted); :15,:30
  `== "drained"` compares SLEEP_REASON → `SleepReasonDrained`, but :12,:27
  `state == "drained"` compares STATE — do NOT touch (see caution below).
- `session_wake.go:53,56` — `"idle-timeout"` read/write of sleep_reason →
  `SleepReasonIdleTimeout`. (:205,:290,:313 `"config-drift"/"orphaned"/"suspended"/"no-wake-reason"`
  are wake-blocker/ack reason vocabulary, NOT sleep_reason at :205/:313 —
  verify each site's source field before converting; only convert where the
  compared value was read from `sleep_reason`.)
- `session_sleep.go:227,230,274` — `Metadata["sleep_reason"] == "idle-timeout"/"idle"`
  → constants; :339 `SleepPatch(clk.Now(), "idle")` → `SleepPatch(..., string(session.SleepReasonIdle))`
  (or retype SleepPatch's reason param — see staging M-A2).
- `session_reconcile.go:1173` (`batch["sleep_reason"] = "drained"`), `:1238`
  (`= "failed-create"`) → constants. `:244,:259` switch on STATE `"drained"`
  and `:1195,:1237` `meta["state"] == "failed-create"` are STATE values —
  do NOT convert; `:1449` `"drained": true` is a state-set entry — do NOT
  convert.
- `compute_awake_set.go:433` (`SleepReason != "idle-timeout"`), `:648`
  (`SleepReason == "city-stop"`) → constants. `:61,:70` comments/state —
  untouched.
- `cmd_wait.go:1349` — `markers.SleepReason == "wait-hold"` →
  `SleepReasonWaitHold`. `:299` `"sleep_intent": "wait-hold"` is the
  SLEEP_INTENT key's vocabulary; the value string is shared but the field is
  different — convert ONLY if a shared constant is semantically correct;
  default: leave as-is (out of scope).
- `session_reconciler.go:48,63` (`return "quarantine"`) →
  `SleepReasonQuarantine` IF the return feeds a sleep_reason write (verify);
  `:420,:422,:468,:2426,:2431,:3269,:3273` `"drained"/"idle"` feed
  ClosePatch/CompleteDrainPatch/SleepPatch reason params → constants;
  `:2107,:2109,:2707` `"config-drift"` is drain/ack reason vocabulary —
  verify source field, convert only true sleep_reason flows.
- `city_runtime.go:2491-2500` — passes through raw `Metadata["sleep_reason"]`
  values (no literals) — only the KEY literal, unchanged.
- `session_wake.go` + `session_index.go` + `session_beads.go` — no value
  literals beyond the above; Info.SleepReason plumbing stays `string`.

**Caution (the whole risk of Part A):** several strings are SHARED across
vocabularies — `"drained"` (state AND sleep_reason), `"failed-create"` (state
AND sleep_reason), `"idle"`/`"config-drift"`/`"wait-hold"` (sleep_intent /
drain-reason / wake-ack vocabularies). Migrate a literal ONLY when the value
at that site is written to or compared against the `sleep_reason` metadata
key / `Info.SleepReason` / a `reason` parameter that lands in `sleep_reason`.
State-value sites keep their literals (S20's typed SessionState owns those).

### B2. molecule_id reader sites (repoint to beadmeta.MoleculeIDMetadataKey)

- `internal/runtime/t3bridge/provider.go:1734,1748,1758` —
  `"moleculeId": bead.Metadata["molecule_id"]` (UI thread-metadata emission);
  `:1799` — `next.Assignment.MoleculeID = bead.Metadata["molecule_id"]`.
  Only the READ key changes to the constant; the emitted `"moleculeId"`
  JSON field name is t3bridge UI vocabulary — unchanged.
- `internal/runproj/summary.go:275` — `stringValue(md["molecule_id"])`.
- `internal/api/handler_beads.go:29` — `for _, key := range []string{"molecule_id", "workflow_id"}` —
  replace `"molecule_id"` with the constant; KEEP `"workflow_id"` as the bare
  literal (beadmeta.WorkflowIDMetadataKey is `"gc.workflow_id"` — a different
  key; substituting it would silently change which metadata key is surfaced).
- Out of scope: `cmd/gc/cmd_sling.go:979` (JSON tag, not a metadata read),
  `cmd/gc/usage_compute.go:52` (comment), dashboard TS (`relation-index.ts`)
  and dist bundles (client-side, generated/asset).

## Invariants — the correctness contract

- **I1 — Byte-identical projection.** For every bead b,
  `InfoFromPersistedBead_new(b) == InfoFromPersistedBead_old(b)` via
  `reflect.DeepEqual` over the full Info struct — every key in E-1…E-AH,
  including untrimmed raw mirrors, trimmed bools, Atoi/RFC3339 parse
  edge-cases, nil-vs-empty AliasHistory slices, and the sparse-bead (all keys
  absent) case. No trimming, normalization, or key added or removed.
- **I2 — Fold == re-projection (kept as gate, now by construction).**
  `info.ApplyPatch(p) == InfoFromPersistedBead(bead{..., Metadata: p.Apply(meta)})`
  for metadata-derived fields. `TestInfoApplyPatchMatchesReprojection`
  (single-key set + clear for all ~70 keys, plus ~28 edge patches, over 5
  base beads) must pass UNMODIFIED — the test file (including the
  hand-maintained `allProjectedMetadataKeys` list) is not edited in the codec
  commit, so it independently gates the swap.
- **I3 — Empty-string-clear.** A patch value of `""` clears the derived field
  to its projected-absent form for EVERY key (bools→false, ints→0,
  times→zero, State→normalized-empty, SessionName→`sessionNameFor(ID)`
  fallback, AliasHistory→nil). Every table setter is total over `""`.
- **I4 — ApplyPatch never flips Closed; unknown keys are no-ops.** The
  closed-blanking of State inside the `state` setter reads carried-forward
  `info.Closed`; `MarkClosed` remains the only status-close fold; map-lookup
  miss preserves the `default:` ignore semantics for non-projected keys.
- **I5 — On-store/on-wire strings unchanged.** Zero metadata KEY or VALUE
  strings change anywhere in this item: `SleepReason` constants equal the
  literals they replace (compile-time `const` equality), and
  `beadmeta.MoleculeIDMetadataKey == "molecule_id"`. No wire type, JSON field,
  or event payload changes → typed-wire and RegisterPayload invariants
  untouched by construction (no wire path is edited).
- **I6 — Deterministic table order with the documented dependencies.**
  Bead-level prologue (ID/Type/Title/Labels/CreatedAt/Closed) runs BEFORE the
  table; `provider` before `transport`; everything else order-independent
  (disjoint field sets — assert disjointness in a test, E-5 pair excepted).
- **I7 — Parallel projection sites stay untouched and consistent.**
  `Store.PersistedMarkers` (store.go:174-197), `LifecycleInputFromMetadata` /
  `FromInfo` (lifecycle_projection.go:200-246), and `Manager.infoFromBead`'s
  runtime overlay are NOT rewritten in this item; their existing
  classifier-equivalence oracles keep guarding them. (Folding them into the
  table is a possible S09c — explicitly out of scope.)
- **I8 — Vocabulary discipline in Part A.** Only values flowing to/from the
  `sleep_reason` key convert to `SleepReason` constants; shared-string STATE
  values (`"drained"`, `"failed-create"`) and sleep_intent/drain-ack
  vocabularies keep their literals. The two deliberately-divergent membership
  lists (`IsDeliberateSleepReason` vs `shouldResetContinuation`) are already
  typed post-#4033 and are not touched.
- **I9 — Layering.** New table lives in `internal/session` (no new imports
  beyond existing stdlib + beadmeta); t3bridge/runproj/api gain only a
  `beadmeta` import (leaf package — no cycle); cmd/gc already imports
  `session`. No upward imports, no worker-boundary bypass (no session
  creation touched), zero role names.

## Behavior-preserving migration/staging

Branch from `origin/main` (must contain `be1d46258`). Four commits in
dependency order, each independently revertable and green:

- **M-0 (pre-work, optional but recommended):** add the NEW parity tests
  (property test over the table shape does not exist yet, but the
  golden-projection test in T2 below can be written against the OLD code
  first and must pass) — "write the test first, watch it pass on old code."
- **M-1 — Part B2 (molecule_id constants).** Repoint the 6 read sites in
  t3bridge/runproj/api onto `beadmeta.MoleculeIDMetadataKey`. Pure literal →
  constant substitution; `go build` + existing tests are the gate. Also grep
  for stragglers: `grep -rn '"molecule_id"' --include='*.go' internal/ cmd/ | grep -v _test`
  and enumerate each remaining hit as either converted or documented-out-of-scope
  (test fixtures, JSON tags).
- **M-2 — Part A (cmd/gc SleepReason).** Delete the 3 shadow constants,
  convert value literals per the E-numbered site list, applying the I8
  vocabulary discipline site by site. Where a helper takes a `reason string`
  param that always lands in `sleep_reason` (e.g. `SleepPatch`,
  `CompleteDrainPatch`, `ClosePatch` — VERIFY each writes `sleep_reason`
  before assuming), pass `string(session.SleepReasonX)`; do NOT retype
  exported signatures in this item (that forces a wider ripple — defer).
  Gate: `go build ./cmd/gc && go test ./cmd/gc/... ./internal/session/...`
  plus a byte-identity self-check: the commit must contain NO string-literal
  changes, only literal→constant swaps (review the diff for exactly-equal
  strings).
- **M-3 — Part B codec table, single commit, no interleaved refactors.**
  1. Add `internal/session/info_codec.go` with the table + `init()` index +
     the disjointness/order unit test.
  2. Rewrite `InfoFromPersistedBead` as prologue + table loop (delete the
     struct-literal body); rewrite `ApplyPatch` as map lookup (delete the
     switch). `MarkClosed`, `Store`, and all other files untouched.
  3. Run the UNMODIFIED oracle tests (I2) + new tests (T1-T4). If any oracle
     fails, fix the table — never the oracle.
  4. `git revert`-ability: the commit touches exactly 3 files
     (info_codec.go new, info_store.go, info_apply_patch.go) + new test file.
- **M-4 (follow-up hygiene, same PR or immediate follow-on):** update the
  stale cross-reference comments that describe "the 180-line switch" /
  "deliberately parallel" mapping (info_apply_patch.go doc comment,
  info_store.go cluster comments) to describe the table; keep the
  cluster-grouping comments ON the table entries so review provenance
  survives.

Rollback story: each commit reverts cleanly in isolation; M-3's revert
restores the exact old switch/struct-literal text (no other file depends on
the table's existence).

## Test plan (incl. -race/parity if applicable)

Existing gates (must pass UNMODIFIED — they are the parity oracle):

- `TestInfoApplyPatchMatchesReprojection` (info_apply_patch_test.go:170) —
  fold==re-projection over every key + edge patches × 5 base beads.
- `TestInfoMarkClosedMatchesReprojection` (info_apply_patch_test.go:255).
- info_store_test.go projection tests; the classifier-equivalence oracles
  (guarding the untouched I7 sites); the full `internal/session` suite.

New tests (written before/with M-3):

- **T1 — Table completeness vs the hand list.** Assert the set of
  `infoKeyCodec` keys EQUALS the test-side `allProjectedMetadataKeys` minus
  the hand-written prologue's non-metadata fields — i.e. exactly the ~70
  metadata keys. Catches a silently dropped table entry (which I2 alone
  would miss only if the test list also lost it; belt-and-braces).
- **T2 — Golden full-projection parity.** A frozen, fully-populated bead
  fixture (every key set to a distinct sentinel value + the closed / no-name
  / acp / sparse variants from oracleBaseBeads) round-tripped through
  `InfoFromPersistedBead` and compared `reflect.DeepEqual` against a
  CHECKED-IN expected Info literal captured from the OLD code (M-0). This is
  the explicit old-vs-new byte-identity proof for the projection direction,
  independent of the fold oracle.
- **T3 — Setter totality over "" (empty-string-clear property).** For every
  table entry: project a fully-populated bead, apply `MetadataPatch{key: ""}`,
  and assert equality with projecting the bead with that key deleted.
  (Generalizes the existing per-key clear patches; driven off the table so
  new keys are covered automatically.)
- **T4 — Order-independence + disjointness.** Assert every pair of table
  entries (except the documented provider/transport pair) writes disjoint
  Info fields — implement by applying each setter to a zero Info with a
  sentinel value and diffing touched fields via reflection. Locks in I6.
- **T5 — Part A byte-identity.** `TestSleepReasonConstantValues` +
  `TestSleepReasonListDivergence` (sleep_reason_test.go, from #4033) already
  pin every constant's string; for cmd/gc add assertions where cheap (e.g. `string(session.SleepReasonCityStop) == "city-stop"`
  is compile-time; the real gate is the diff review rule in M-2) plus the
  existing cmd/gc reconciler/state-helper tests.
- **T6 — molecule_id repoint.** Existing t3bridge/runproj/api tests pass
  unmodified (constant == literal, so no fixture changes).

Execution: `go vet ./...`; `go test -race ./internal/session/ ./internal/beadmeta/ ./internal/runtime/t3bridge/ ./internal/runproj/ ./internal/api/` —
-race is relevant because `infoKeyIndex` is built in `init()` and read
concurrently by reconciler goroutines; init-time construction + never-mutated
map is race-free by construction, and -race verifies no test path mutates it.
Then `make test` (fast baseline) per project gates. Full sweep: the sharded
targets in TESTING.md if the change is contested.

## Top correctness risks

- **R1 — Projection-direction totality drift.** The old projection leaves
  zero-valued fields untouched for absent keys (`wake_attempts` Atoi-fail =
  no-set), while the shared setters use ApplyPatch's total form (explicit
  `= 0` / `= time.Time{}`). These agree ONLY because projection starts from a
  zero Info. If anyone later reuses a table setter against a non-fresh Info
  outside ApplyPatch semantics, the equivalence breaks silently. Mitigation:
  T2 golden test + a doc comment on `infoKeySpec.set` stating the "fresh-Info
  OR patch-fold only" contract.
- **R2 — Order dependencies hidden by the loop.** `session_name` needs
  `info.ID`, `state` needs `info.Closed`, `transport`/`provider` need each
  other's raw mirrors. A future table reorder or a new cross-field key breaks
  parity in a way single-key patch tests may not catch (multi-key patches
  iterate Go map order!). NOTE: ApplyPatch iterates `for key, v := range patch`
  — MAP ORDER, nondeterministic today. The old switch was order-insensitive
  for all shipped key pairs except provider/transport, whose setters are
  written to converge to the same final Transport in either order — the new
  table must preserve exactly that convergence property (E-5 parity
  argument), and T4 must include a provider+transport two-key patch applied
  in both orders. Mitigation: I6 + T4; keep the cross-field set minimal.
- **R3 — Vocabulary collision in Part A (`"drained"` / `"failed-create"` /
  `"workflow_id"`).** The same byte-string lives in the STATE vocabulary, the
  sleep_reason vocabulary, and (for workflow_id) as both a bare and a
  gc.-prefixed metadata key. A careless constant substitution silently
  changes WHICH key/vocabulary a site touches (worst case:
  `handler_beads.go` surfacing `gc.workflow_id` instead of `workflow_id`, or
  a state comparison converted to a sleep-reason constant that later gets
  renamed). Mitigation: I8 site-by-site discipline; the enumeration in this
  spec marks every do-NOT-convert site; diff review rule "no string value
  changes".
- **R4 — Oracle self-blinding.** If M-3 "fixes" a failing oracle test instead
  of the table, the parity gate is destroyed exactly when it matters.
  Mitigation: hard rule in M-3.3 (oracle files are read-only in the codec
  commit; CI diff check that info_apply_patch_test.go is untouched).
- **R5 — Missed reader repoint / stale references.** grep-only discovery can
  miss dynamic or test-fixture references to `molecule_id` (per the No
  Semantic Search rule: check type refs, string literals, fixtures,
  re-exports). Mitigation: M-1's documented straggler grep + the constant
  equals the literal so any missed site keeps working (zero-behavior-change
  fallback — the failure mode is incomplete cleanup, not breakage).

**Perfectionist vs pragmatist:** the perfectionist notes the table doesn't
eliminate the OTHER two parallel maps (PersistedMarkers, lifecycle
projections) so "adding a key" is still >1 edit (2-3, down from 4-5), and
wants S09c to fold them in; the pragmatist accepts I7 — those have their own
oracles, smaller key sets, and folding them here would bloat the parity
surface of one commit. Ship A+B2 first (near-zero risk), then B.
