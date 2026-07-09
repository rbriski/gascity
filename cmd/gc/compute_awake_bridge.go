package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// buildAwakeInputFromReconciler constructs AwakeInput from the reconciler's
// existing data. Runtime liveness is populated from the already-computed
// wakeTargets; attachment and pending interactions come from provider
// capability probes.
func buildAwakeInputFromReconciler(
	cfg *config.City,
	cityPath string,
	sessionInfos []session.Info,
	poolDesired map[string]int,
	namedSessionDemand map[string]bool,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	assignedWorkBeads []beads.Bead,
	readyAssignedFlags []bool,
	assignedWorkStoreRefs []string,
	wakeTargets []wakeTarget,
	sp runtime.Provider,
	clk time.Time,
) AwakeInput {
	input := AwakeInput{
		ScaleCheckCounts:   poolDesired,
		NamedSessionDemand: cloneBoolMap(namedSessionDemand),
		WorkSet:            workSet,
		ReadyWaitSet:       readyWaitSet,
		RunningSessions:    make(map[string]bool),
		AttachedSessions:   make(map[string]bool),
		PendingSessions:    make(map[string]bool),
		ChatIdleTimeout:    cfg.ChatSessions.IdleTimeoutDuration(),
		ManualGracePeriod:  cfg.ChatSessions.GracePeriodDuration(),
		Now:                clk,
	}

	// Agents. Load runtime suspension state once against the in-scope
	// city path so suspension resolves against the controlled city
	// rather than the process cwd.
	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		agent := AwakeAgent{
			QualifiedName:     a.QualifiedName(),
			Suspended:         isAgentEffectivelySuspendedWith(cfg, a, suspState),
			SleepAfterIdle:    parseSleepDuration(a.SleepAfterIdle),
			MinActiveSessions: a.EffectiveMinActiveSessions(),
		}
		if len(a.DependsOn) > 0 {
			agent.DependsOn = a.DependsOn
		}
		input.Agents = append(input.Agents, agent)
	}

	// Named sessions
	cityName := config.EffectiveCityName(cfg, "")
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		identity := ns.QualifiedName()
		input.NamedSessions = append(input.NamedSessions, AwakeNamedSession{
			Identity:    identity,
			Template:    ns.TemplateQualifiedName(),
			Mode:        ns.Mode,
			RuntimeName: config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity),
		})
	}

	// Per-instance runtime liveness, keyed by session BEAD ID (== GC_SESSION_ID == a
	// Lumen fold row's recorded claimant_id). It is the same live probe the reconciler's
	// close gate reads (wakeTarget.alive → RunningSessions). Keying by bead id (not name)
	// means a same-name respawn cannot revive a dead claimant's demand. A claimant absent
	// from this map (not a wake target this tick) reads not-alive, matching the pre-fix
	// gate: only a recoverable-asleep claimant escapes the not-alive drop below.
	aliveByBeadID := make(map[string]bool, len(wakeTargets))
	for _, tgt := range wakeTargets {
		if tgt.session != nil {
			aliveByBeadID[tgt.session.ID] = tgt.alive
		}
	}

	// Open-set membership + NORMALIZED lifecycle state + stranded marker, keyed by session
	// BEAD ID, built from the reconciler's full open snapshot (sessionInfos, less closed
	// beads). Membership is the same domain the firewall's FindByID reads (open-only), and
	// the durable stranded marker is the same signal it consults — so the fold-row wake
	// gate below mirrors the firewall's instance-death verdict (absent OR open+stranded),
	// while EXEMPTING an asleep-recoverable claimant. The state is the projected CompatState
	// (not the raw metadata) so every recoverable-sleep variant — asleep, and the "stopped"
	// city-stop form — normalizes to asleep and is exempted uniformly.
	openState := make(map[string]string, len(sessionInfos))
	openStranded := make(map[string]bool, len(sessionInfos))
	openSleepReason := make(map[string]string, len(sessionInfos))
	for i := range sessionInfos {
		si := sessionInfos[i]
		if si.Closed {
			continue
		}
		lcInput := session.LifecycleInputFromInfo(si)
		lcInput.Now = clk
		openState[si.ID] = string(session.ProjectLifecycle(lcInput).CompatState)
		openSleepReason[si.ID] = strings.TrimSpace(si.SleepReason)
		if strings.TrimSpace(si.StrandedEventEmittedAt) != "" {
			openStranded[si.ID] = true
		}
	}

	// Work beads. Readiness is the store's verdict (readyAssignedFlags), not a
	// status-only guess: assignedWorkBeads mixes the open-routed orphan-release
	// pass (which admits any open assigned+routed bead with no deps check) into
	// the same slice as the genuinely-ready passes. Fabricating Ready from
	// status alone held a blocked open bead's session awake forever (it never
	// slept, so the resume-on-ShouldWake path never fired). readyAssignedFlags is
	// index-aligned with assignedWorkBeads and resolved from the store-scoped
	// readiness verdict, so a blocked open rig bead is not marked ready by a
	// same-ID ready bead in another store. A missing flag defaults to not-ready.
	for i := range assignedWorkBeads {
		wb := assignedWorkBeads[i]
		a := strings.TrimSpace(wb.Assignee)
		if a != "" && (wb.Status == "open" || wb.Status == "in_progress") {
			// Fold-row wake gate. A Lumen fold-owned journal row (tierBHookStoreName store
			// ref — the same B-2 discriminant filterAssignedWorkBeadsForPoolDemand uses)
			// carries a recorded claimant_id (the claiming session's instance-unique bead
			// id). Such a row holds its claimant's pool bead awake — the assigned-work wake
			// anchor — so a claim whose claimant is genuinely DEAD would wedge the run: the
			// bead never reaches the poolFreeable close and the firewall (which reads only
			// the open session set) never strands it. We drop the anchor ONLY for a dead
			// claimant, by the SAME instance-death definition the firewall uses, EXEMPTING
			// an asleep-recoverable claimant (gc stop/city-stop, max-session-age, idle) that
			// MUST re-wake to resume/adopt. Recovery of a truly dead claimant is the
			// firewall's job (strand → the L5 retry mints a fresh attempt), not a resume. A
			// live mid-do claimant still anchors (the L1 preserve tier). The store-ref
			// discriminant means an ordinary bd bead carrying a stray, user-set claimant_id
			// is never gated (byte-identical).
			if i < len(assignedWorkStoreRefs) && assignedWorkStoreRefs[i] == tierBHookStoreName {
				if cid := strings.TrimSpace(wb.Metadata[engine.ClaimantIDMetaKey]); cid != "" && foldRowClaimantDead(cid, openState, openStranded, openSleepReason, aliveByBeadID) {
					continue
				}
			}
			ready := i < len(readyAssignedFlags) && readyAssignedFlags[i]
			input.WorkBeads = append(input.WorkBeads, AwakeWorkBead{
				ID: wb.ID, Assignee: a, Status: wb.Status, Ready: ready,
			})
		}
	}

	// Session infos. The reconciler passes its coherent typed snapshot — one
	// session.Info per session bead, in the reconciler's `ordered` slice order.
	// Slice order is load-bearing: ComputeAwakeSet resolves SessionName by
	// last-write-wins and first-match, and SessionName is non-unique (a retired
	// duplicate and its winner share it), so the iteration domain must stay
	// order-preserving. Each Info already carries the typed persisted facts, so no
	// raw session bead is cracked here.
	for i := range sessionInfos {
		info := sessionInfos[i]
		if info.Closed {
			continue
		}
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name == "" {
			continue
		}
		lcInput := session.LifecycleInputFromInfo(info)
		lcInput.Now = clk
		lifecycle := session.ProjectLifecycle(lcInput)
		bead := AwakeSessionBead{
			ID:          info.ID,
			SessionName: name,
			// Canonicalize so adopted beads persisted under a legacy identity
			// (e.g. a removed binding) key the awake engine by the current
			// agent template. Unresolvable templates pass through unchanged.
			Template:               normalizeAgentTemplateIdentity(cfg, info.Template),
			State:                  string(lifecycle.CompatState),
			SleepReason:            info.SleepReason,
			ManualSession:          isManualSessionInfo(info),
			PendingCreate:          lifecycle.HasWakeCause(session.WakeCausePendingCreate),
			ExplicitWake:           lifecycle.HasWakeCause(session.WakeCauseExplicit),
			DependencyOnly:         info.DependencyOnly,
			NamedIdentity:          lifecycle.NamedIdentity,
			ConfiguredNamedSession: isNamedSessionInfo(info),
			Pinned:                 lifecycle.HasWakeCause(session.WakeCausePinned),
			Drained:                lifecycle.BaseState == session.BaseStateDrained,
			WaitHold:               info.WaitHold == "true",
			RestartRequested:       strings.TrimSpace(info.RestartRequested) == "true",
			ContinuationResetPending: strings.TrimSpace(info.ContinuationResetPending) == "true" &&
				strings.TrimSpace(info.ResetCommittedAt) != "",
			CurrentlyProcessingBeadID: strings.TrimSpace(info.CurrentlyProcessingBeadID),
		}
		bead.HeldUntil = lifecycle.HeldUntil
		bead.QuarantinedUntil = lifecycle.QuarantinedUntil
		bead.CreatedAt = info.CreatedAt
		if t, err := time.Parse(time.RFC3339, info.DetachedAt); err == nil && !t.IsZero() {
			bead.IdleSince = t
		}
		input.SessionBeads = append(input.SessionBeads, bead)
	}

	// Preserve the reconciler's existing wake continuity for already-materialized
	// on-demand named sessions: when work_query matched the backing template and
	// the canonical bead still exists, carry an explicit named-session work-query
	// signal rather than waking ordinary siblings from the generic WorkSet path.
	for _, ns := range input.NamedSessions {
		if ns.Mode != "on_demand" || !input.WorkSet[ns.Template] {
			continue
		}
		if resolveNamedSessionBeadName(input.SessionBeads, ns) == "" {
			continue
		}
		if input.NamedSessionWorkQ == nil {
			input.NamedSessionWorkQ = make(map[string]bool)
		}
		input.NamedSessionWorkQ[ns.Identity] = true
	}

	// Runtime liveness comes from wakeTargets. Attachment is probed only when
	// it can affect the awake decision; the common active desired-session path
	// is already awake and has no idle reference to suppress. Index the typed
	// snapshot by (unique) session ID for the per-target reads — keying by ID is
	// order-independent, so it does not disturb the SessionName last-write-wins
	// ordering the session scan above depends on. Every wakeTarget's bead is one
	// of the sessionInfos (both derive from the reconciler's `ordered` set), so a
	// miss yields a zero Info whose empty SessionNameMetadata skips the target —
	// the same skip the former empty-session_name read produced.
	infoBy := make(map[string]session.Info, len(sessionInfos))
	for _, in := range sessionInfos {
		infoBy[in.ID] = in
	}
	for _, target := range wakeTargets {
		info := infoBy[target.session.ID]
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name == "" {
			continue
		}
		if target.alive {
			input.RunningSessions[name] = true
		}
		if shouldProbeAttachmentForAwakeInput(info, target.alive, cfg, poolDesired) {
			if attached, err := workerSessionTargetAttachedWithConfig("", nil, sp, nil, name); err == nil && attached {
				input.AttachedSessions[name] = true
			}
		}
		if pendingInteractionReady(sp, name) {
			input.PendingSessions[name] = true
		}
	}

	return input
}

// foldRowClaimantDead reports whether a Lumen fold row's recorded claimant is dead — the
// signal that decides whether the wake gate drops the fold row's session-wake anchor. It
// mirrors the firewall's instance-death verdict (lumen_firewall.go's
// firewallClaimantDeadOrStranded), with one recoverable-sleep exemption:
//
//   - ABSENT from the open set (recycled, closed, or replaced by a fresh-id respawn) — the
//     firewall's FindByID miss → dead;
//   - open and carrying the reconciler's durable stranded marker → dead;
//   - open, ASLEEP (projected CompatState asleep), AND slept for a RECOVERABLE-resumable
//     reason (gc stop/city-stop, max-session-age, idle, idle-timeout) → NOT dead: its
//     anchor MUST survive so the next awake pass re-wakes it to resume or adopt its claim
//     (the HIGH-1/HIGH-2 fix). This is deliberately NOT "any asleep": a crashed claimant
//     slept with runtime-missing (or provider-terminal-error/failed-create) is asleep too,
//     and keeping its anchor would hold shouldWake=true forever so the reconciler's
//     poolFreeable close/strand path (which requires !shouldWake) never fires — the wedge.
//     Only the intentional-stop reasons resume; the death reasons must strand;
//   - otherwise (non-asleep, or asleep for a death/unknown reason) → dead iff its runtime is
//     not alive. This is the reconciler's live close-gate signal (wakeTarget.alive), and a
//     claimant absent from wakeTargets this tick reads not-alive — exactly as the pre-fix
//     gate treated it, so a genuinely killed claimant still strands. A live mid-do claimant
//     reads alive → kept (the L1 preserve tier).
//
// openState carries the projected CompatState (not raw metadata) so the "stopped" city-stop
// form normalizes to asleep uniformly; openStranded/openSleepReason/aliveByBeadID are keyed
// by session bead id.
func foldRowClaimantDead(claimantID string, openState map[string]string, openStranded map[string]bool, openSleepReason map[string]string, aliveByBeadID map[string]bool) bool {
	state, open := openState[claimantID]
	if !open {
		return true // absent from the open set → recycled/closed/fresh-id respawn → dead
	}
	if openStranded[claimantID] {
		return true // the reconciler stamped its durable stranded marker → dead
	}
	if state == string(session.StateAsleep) && isResumableClaimantSleepReason(openSleepReason[claimantID]) {
		return false // asleep for a recoverable reason → keep the anchor so it re-wakes to resume/adopt
	}
	return !aliveByBeadID[claimantID] // otherwise dead iff the runtime is not alive (crashed)
}

// isResumableClaimantSleepReason reports whether an asleep claimant's sleep_reason marks an
// INTENTIONAL stop whose Tier-B claim must resume when the session re-wakes, versus a
// terminal-death reason (runtime-missing, provider-terminal-error, failed-create) whose
// claim must strand. The allowlist is intentional: an unknown or empty reason falls through
// to the runtime-liveness verdict (drop when not alive), matching the deny-by-default stance
// of isPoolSessionSlotFreeable and the pre-fix wake gate.
func isResumableClaimantSleepReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case sleepReasonCityStop, "max-session-age", "idle", "idle-timeout":
		return true
	default:
		return false
	}
}

func shouldProbeAttachmentForAwakeInput(info session.Info, alive bool, cfg *config.City, poolDesired map[string]int) bool {
	if !alive {
		return false
	}
	// MetadataState is the RAW state metadata (verbatim), matching the former
	// raw state field read off the session bead — NOT the normalized,
	// closed-blanked Info.State, which would flip the probe verdict for a closed
	// bead whose raw state is still "active".
	state := info.MetadataState
	if state != string(session.StateActive) && state != string(session.StateAwake) {
		return true
	}
	if info.DetachedAt != "" {
		return true
	}
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = info.Template
	}
	if template != "" && poolDesired[template] > 0 {
		return false
	}
	return true
}

// awakeSetToWakeEvals converts ComputeAwakeSet output to wakeEvaluation map
// for compatibility with advanceSessionDrainsWithSessions.
func awakeSetToWakeEvals(decisions map[string]AwakeDecision, sessionBeads []AwakeSessionBead) map[string]wakeEvaluation {
	evals := make(map[string]wakeEvaluation, len(decisions))
	for _, bead := range sessionBeads {
		d, ok := decisions[bead.SessionName]
		if !ok {
			continue
		}
		var reasons []WakeReason
		if d.ShouldWake {
			switch d.Reason {
			case "pending-create":
				reasons = []WakeReason{WakeCreate}
			case "explicit-wake":
				reasons = []WakeReason{WakeConfig}
			case "attached":
				reasons = []WakeReason{WakeAttached}
			case "pending":
				reasons = []WakeReason{WakePending}
			case "pin":
				reasons = []WakeReason{WakePin}
			case "wait-ready":
				reasons = []WakeReason{WakeWait}
			case "assigned-work", "named-demand", "work-query":
				reasons = []WakeReason{WakeWork}
			case "min-active":
				reasons = []WakeReason{WakeConfig}
			default:
				reasons = []WakeReason{WakeConfig}
			}
		}
		evals[bead.ID] = wakeEvaluation{
			Reasons:          reasons,
			Reason:           d.Reason,
			ConfigSuppressed: d.Reason == "idle-sleep",
			HasAssignedWork:  d.HasAssignedWork,
		}
	}
	return evals
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	if source == nil {
		return nil
	}
	out := make(map[string]bool, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func parseSleepDuration(s string) time.Duration {
	if s == "" || s == "off" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}
