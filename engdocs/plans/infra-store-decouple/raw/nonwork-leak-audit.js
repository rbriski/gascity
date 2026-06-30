export const meta = {
  name: 'nonwork-leak-audit',
  description: 'Inventory every remaining direct metadata/bead-field read on NON-WORK objects (session/nudge/mail/order) + the existing typed snapshot API, to plan the clean fix',
  phases: [{ title: 'Inventory' }],
}
const WT = '/data/projects/gascity/.claude/worktrees/object-front-doors'
const SCHEMA = {
  type: 'object',
  properties: {
    area: { type: 'string' },
    findings: { type: 'array', items: { type: 'string' } },
  },
  required: ['area', 'findings'],
}

// Each finding (where applicable) is one pipe-delimited record:
//   RAW_READ | file:line | <field: b.Metadata["key"] / b.Status / b.Title / b.Labels / b.ID / b.Assignee> | <the QUESTION it answers, e.g. "session_name for template" / "is pool-managed" / "is closed"> | <existing typed alternative if any: snapshot.FindX / Info.Field / NONE>
// Plus TYPED_METHOD | name(sig) -> returns | <what it answers> for the snapshot-api agent.

const common = `Worktree ${WT}. We are eliminating ALL direct reads of metadata/bead-fields on NON-WORK objects (session/nudge/mail/order beads); only generic WORK beads may be read raw. A "session bead" is one with Type=="session" or label "gc:session" (IsSessionBeadOrRepairable). Report each direct raw read of a session-bead field as a RAW_READ record (format in the script comment): file:line | field | the QUESTION it answers | existing typed alternative (snapshot.FindX / a session.Info field / NONE). Be exhaustive; quote metadata keys verbatim. Do NOT edit anything.`

const tasks = [
  { label: 'snapshot-api', prompt: `${common}\nRead ${WT}/cmd/gc/session_bead_snapshot.go in full. Catalog the EXISTING typed surface of *sessionBeadSnapshot: for EVERY method emit "TYPED_METHOD | <name(params) -> returns> | <what question it answers> | <reads raw beads internally? which fields>". Include Open()/the index maps. Then list which methods leak raw []beads.Bead out (Open and any returning beads.Bead) vs which are already typed answers. Also note sessionBeadSnapshot's fields/indexes and what each is keyed on.` },
  { label: 'consumers-controller', prompt: `${common}\nGrep+read ${WT}/cmd/gc/build_desired_state.go and ${WT}/cmd/gc/city_runtime.go for every consumer of loadSessionBeadSnapshot / sessionBeadSnapshot / snapshot.Open() and every direct session-bead Metadata/Status/Title/Labels read. Emit RAW_READ records. Note which already call a typed snapshot method vs read raw.` },
  { label: 'consumers-session', prompt: `${common}\nGrep+read the session-arm files ${WT}/cmd/gc/session_beads.go, session_reconciler.go, session_reconcile.go, session_lifecycle_parallel.go, session_bead_cycle.go for direct session-bead field reads NOT already routed through the front door (i.e. reading b.Metadata[...]/b.Status/b.Title off a session bead variable, esp. off snapshot.Open() results or a *beads.Bead session param). Emit RAW_READ records. Skip the front-door WRITES (sessFront.* / ApplyPatch) — only READS of raw fields.` },
  { label: 'consumers-cli-runtime', prompt: `${common}\nGrep+read ${WT}/cmd/gc/cmd_wait.go, providers.go, nudge_dispatcher.go, and grep cmd/gc for other snapshot.Open()/session-bead raw reads. Also: do NUDGE/MAIL/ORDER beads have any remaining direct metadata reads outside their front doors? (grep nudge_beads.go, order_dispatch.go, cmd_order.go, internal/beadmail for raw b.Metadata reads). Emit RAW_READ records tagged by object class.` },
  { label: 'broad-sweep', prompt: `${common}\nRepo-wide safety sweep: from ${WT}, grep cmd/gc and internal/api for '.Metadata[' and '.Status ==' and '.Labels' and '.Title' usages, and for EACH judge whether the bead variable is a SESSION/NUDGE/MAIL/ORDER (non-work) bead vs a generic WORK bead. Emit RAW_READ records ONLY for non-work beads (work beads are legal-raw). Focus on finding leaks the other agents' file lists might miss. Give counts: "TOTAL_NONWORK_RAW_READS | <n> | cmd/gc=<a> internal/api=<b>".` },
  { label: 'fields-needed', prompt: `${common}\nFrom ${WT}/cmd/gc/session_bead_snapshot.go (the indexes + newSessionBeadSnapshot) and ${WT}/internal/session/manager.go (session.Info struct) and info_store.go (InfoFromPersistedBead): produce the CANONICAL list of session-bead fields actually consumed anywhere. For each emit "FIELD | <metadata key or bead field> | <category: IDENTITY/STATE/POOL/RUNTIME/NAMED-SESSION/MISC> | <already on session.Info? YES/NO> | <consumed by: snapshot-index / direct-read / both>". Then "FIELD_COUNT | total=<n> on_Info=<m> missing=<k>". This decides whether 'extend Info' is ~20 fields or fewer once duplicates/index-derived ones are removed.` },
]
phase('Inventory')
const results = await parallel(tasks.map((t) => () => agent(t.prompt, { label: t.label, phase: 'Inventory', schema: SCHEMA })))
return { audit: results.filter(Boolean) }
