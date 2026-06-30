export const meta = {
  name: 'frontdoor-completion-analysis',
  description: 'Map the session-read field consumers + the cross-class split structure to complete the session front door (read-only)',
  phases: [{ title: 'Map', detail: 'one agent per consumer/helper' }],
}

const WT = '/data/projects/gascity/.claude/worktrees/object-front-doors'

const SCHEMA = {
  type: 'object',
  properties: {
    target: { type: 'string' },
    findings: { type: 'array', items: { type: 'string' }, description: 'pipe-delimited facts (see prompt)' },
  },
  required: ['target', 'findings'],
}

const tasks = [
  {
    label: 'info-current',
    prompt: `Read ${WT}/internal/session/info_store.go AND the session.Info type + InfoFromPersistedBead projector (grep for 'type Info struct' and 'func InfoFromPersistedBead' under ${WT}/internal/session/). Report into findings:
- "INFO_FIELD | <fieldName> <type> | <bead source: b.Metadata[key] / b.Status / b.ID / b.Type / label>" for EVERY field session.Info currently has and where InfoFromPersistedBead reads it from.
- "INFOSTORE_METHOD | <name(sig)> | <what it does>" for every existing *InfoStore method.
- "PROJECTOR | <note>" any side-effect / filtering / IsSessionBeadOrRepairable gate in InfoFromPersistedBead.
Be exhaustive and exact (copy field names + metadata keys verbatim).`,
  },
  {
    label: 'snapshot',
    prompt: `Read ${WT}/cmd/gc/session_bead_snapshot.go (loadSessionBeadSnapshot + the sessionBeadSnapshot type + ListAllSessionBeads usage) and ${WT}/internal/session/list_all.go (ListAllSessionBeads). Report into findings:
- "META_READ | <b.Metadata[key] OR b.Status/.ID/.Type/.Labels> | <how it's used: index key / filter / value stored>" for EVERY raw bead field the snapshot loader and the sessionBeadSnapshot struct read. This is the field list session.Info must carry to replace the raw snapshot. Be EXHAUSTIVE — every metadata key (session_name, template, configured_named_identity, common_name, pool_slot, agent_name, alias, state, ...).
- "SNAPSHOT_INDEX | <field/map name> | <keyed by what>" for each index/map inside sessionBeadSnapshot.
- "LISTALL | <query semantics>" what ListAllSessionBeads does (Type+Label union, Live, Metadata filter).
- "CONSUMER_SHAPE | <returns []beads.Bead or *sessionBeadSnapshot> | <who needs raw beads vs could use Info>".`,
  },
  {
    label: 'adoption',
    prompt: `Read ${WT}/cmd/gc/adoption_barrier.go (runAdoptionBarrier + openSessionBeadExists, focus on the two ListAllSessionBeads(sessFront.Store().Store, ...) sites ~lines 73/278 and what the returned []beads.Bead is used for). Report into findings:
- "ADOPT_READ | <b.Metadata[key] OR b.field> | <use>" for every bead field the adoption logic reads from the ListAllSessionBeads results (dedup by session_name, etc.).
- "QUERY | <site> | <ListQuery passed: Metadata/Live/empty>".
- "INFO_SUFFICIENT | YES/NO | <whether a typed []session.Info (with the fields snapshot needs) would cover adoption, or it needs more>".`,
  },
  {
    label: 'closebead-split',
    prompt: `Read ${WT}/cmd/gc/session_beads.go — the functions closeBead (~2331), releaseWorkFromClosedSessionBead, cancelStateAssignedToRetiredSessionBead (~783), closeFailedCreateBead, and how closeBead is called (grep 'closeBead(' across cmd/gc). Report into findings:
- "CLOSEBEAD_OP | SESSION/WORK/EXTMSG | <the exact op, e.g. Update{Status:closed} / SetMetadataBatch / releaseWorkFromClosedSessionBead / List(Assignee)>" decomposing closeBead into its session-object part vs work/extmsg part — this is for splitting it (session-close → InfoStore method, work-release → workAssignment façade).
- "CANCEL_OP | SESSION/WORK/EXTMSG | <op>" same decomposition for cancelStateAssignedToRetiredSessionBead.
- "CLOSEBEAD_CALLER | <file:func> | <store arg passed: sessFront.Store().Store / raw store / workStore>" every caller of closeBead.
- "EXISTING_FRONTDOOR | <InfoStore.Close or workAssignment.Release exists? what sig>" whether a session-close front-door method and a work-release façade method already exist to reuse.
- "SEQUENCING | <note>" the order constraint (close-then-release? the §5 join-point / mass-closure landmine).`,
  },
  {
    label: 'title-api',
    prompt: `Read ${WT}/internal/api/title_generate.go (MaybeGenerateTitleAsync ~104 + helpers) and its caller ${WT}/cmd/gc/cmd_session.go maybeAutoTitle (~521). Report into findings:
- "TITLE_STORE_OP | <op on the store, e.g. Get(beadID) / SetMetadata(title) / Update>" every store op MaybeGenerateTitleAsync performs.
- "TITLE_CLASS | SESSION/WORK/GENERIC | <is the bead it operates on a session bead? is 'title' a session attribute?>".
- "TITLE_SIG_CHANGE | <proposed: take beads.SessionStore vs *session.InfoStore vs keep beads.Store> | <why>".
- "TITLE_OTHER_CALLERS | <any other caller of MaybeGenerateTitleAsync outside cmd_session.go>".`,
  },
  {
    label: 'createsession',
    prompt: `Read ${WT}/cmd/gc/session_name_lookup.go — createPoolSessionBead (~150) + createPoolSessionBeadWithAlias (~161) and any session-create helper it uses. Also check ${WT}/internal/session/ for an existing CreateSession / CreateSpec type (grep 'CreateSession' and 'CreateSpec'). Report into findings:
- "CREATE_OP | <op: store.Create(beads.Bead{...}) / SetMetadata(session_name) / alias reservation via WithCitySessionIdentifierLocks / config reservation check>" every step createPoolSessionBeadWithAlias performs.
- "CREATE_INPUT | <param> | <type>" the inputs (template, identity, alias, cfg, sessionBeads snapshot).
- "EXISTING_CREATE | <CreateSession/CreateSpec exists in internal/session? sig>".
- "CREATE_FEASIBILITY | CLEAN/COMPLEX | <can it become InfoStore.CreateSession(spec), or does the alias-reservation/config-check logic block confining it behind the front door?>".`,
  },
]

phase('Map')
const results = await parallel(tasks.map((t) => () => agent(t.prompt, { label: t.label, phase: 'Map', schema: SCHEMA })))
return { maps: results.filter(Boolean) }
