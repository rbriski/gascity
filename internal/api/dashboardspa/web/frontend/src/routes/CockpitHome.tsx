import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import type { RunLane } from 'gas-city-dashboard-shared';
import type {
  SessionResponse,
  StatusBody,
  StatusWorkCounts,
  UsageBody,
  UsageSessionRecent,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow, getActiveCity } from '../api/cityBase';
import { useAttentionModel } from '../attention/context';
import type { AttentionItem } from '../attention/compose';
import {
  Gauge,
  Odometer,
  Oscilloscope,
  PipelineBar,
  RunRings,
  StatusLamps,
  VUBank,
  agoStr,
  fmtCompact,
  fmtCost,
  fmtRate,
  type PipelineSegment,
  type RunRing,
  type StatusLamp,
  type VUMeter,
} from '../components/cockpit';
import { PageHeader } from '../components/PageHeader';
import { useCachedData } from '../hooks/useCachedData';
import { useFaviconSignal } from '../hooks/useFaviconSignal';
import { useGcEventFeed, type GcEventEnvelope } from '../hooks/useGcEvents';
import { cleanWorkerName } from '../hooks/projectOf';
import { useRunSummary } from '../runs/runSummarySubscription';
import { runDetailHref } from '../supervisor/runHref';
import { supervisorApi } from '../supervisor/client';

// CockpitHome — the live-activity dashboard home (specs/plans/0002). The page's
// job is "show everything happening in the city right now": a scrolling event
// oscilloscope, an odometer of beads closed today, dial-row gauges (events,
// burn, throughput), a bead-status pipeline, and a bottom grid of per-session VU
// meters, run-progress rings, and system lamps. Attention/needs-you is a single
// maroon strip below the header (the sole `text-accent` on the page, DESIGN.md
// One Mark Rule); everything else carries status by shape/position/tone
// (ok-sage, warn-ochre), never accent.
//
// Every instrument stays mounted at all times: a degraded source rests at its
// zero/empty state with a small per-instrument microcopy line, never a
// collapsed sentence in place of the dial (the anti-Norse rule). Live rates ride
// `worker.operation` feed events when they carry cost/tokens, falling back to
// the short-poll usage read otherwise — no fabricated zeros.

const TICK_MS = 250; // ~4Hz refresh of the feed-derived gauges + live-chip age
const EVENTS_WINDOW_MS = 60_000; // rolling window backing the events/min reading
const MAIL_WINDOW_MS = 60_000; // rolling window backing the mail-traffic lamp
const VU_WINDOW_MS = 60_000; // rolling window for per-session tok/min
const WORKER_WINDOW_MS = 120_000; // rolling window backing burn + throughput
const WORKER_WINDOW_SECS = 120;
const ORDER_FAIL_RECENT_MS = 90_000; // orders lamp trips warn within this of a failure
const USAGE_POLL_MS = 20_000; // usage endpoint is time-bucket cached server-side
const STATUS_POLL_MS = 15_000;
const SESSIONS_POLL_MS = 15_000;
const CLOSED_RESEED_MS = 300_000; // re-seed the beads-closed odometer every 5 min
const CLOSED_SEED_LIMIT = 5_000; // generous cap; the odometer reads 4 digits anyway

// A single catch-all prefix: every typed gc event (bead./session./worker./
// order./mail./health./… ) feeds the oscilloscope, the events/min rate, and the
// feed-derived instruments. `''` prefixes every type; the feed still requires a
// non-empty prefix array to connect.
const ALL_EVENT_PREFIXES = [''] as const;

const EVENTS_MAX = 150;
const EVENTS_WARN = 120;
const BURN_MAX = 600;
const BURN_WARN = 480;
const TOKENS_MAX = 40_000;
const TOKENS_WARN = 32_000;

// VU full-scale from the cockpit mockup: 620k tokens per minute pins a meter.
const VU_MAX = 620_000;

const MAX_RINGS = 8; // active lanes are wrapped; cap the ring row so it stays legible
const MAX_VU = 8;

// Map a run lane's phase word to a stage index when the lane carries no resolved
// stage position — a fallback so the ring still sweeps to a sensible fraction.
const PHASE_STAGE_INDEX: Record<string, number> = {
  intake: 1,
  implementation: 2,
  review: 3,
  approval: 4,
  finalization: 5,
  complete: 5,
  blocked: 2,
  active: 1,
};

// The pipeline reads bead work statuses left-to-right, deepening in alpha.
const PIPELINE_STAGES: ReadonlyArray<{
  key: string;
  label: string;
  pick: (work: StatusWorkCounts) => number;
}> = [
  { key: 'ready', label: 'ready', pick: (w) => w.ready },
  { key: 'hooked', label: 'hooked', pick: (w) => w.hooked },
  { key: 'in_progress', label: 'in progress', pick: (w) => w.in_progress },
  { key: 'review', label: 'review', pick: (w) => w.review },
];

interface WorkerOpSample {
  t: number;
  tokens: number;
  cost: number;
  key: string | null;
}

interface FeedStats {
  perMin: number;
  lastAgeMs: number | null;
  /** Σcost×(3600/window) over the 120s window; null when no token-bearing ops. */
  windowCostPerHr: number | null;
  /** Σtokens/windowSecs over the 120s window; null when no token-bearing ops. */
  windowTokensPerSec: number | null;
  beadsClosedToday: number;
  orderFailedRecent: boolean;
  mailPerMin: number;
  /** Per-session tokens over the 60s window (= tok/min), keyed by id/name. */
  sessionTokPerMin: Record<string, number>;
}

const EMPTY_FEED_STATS: FeedStats = {
  perMin: 0,
  lastAgeMs: null,
  windowCostPerHr: null,
  windowTokensPerSec: null,
  beadsClosedToday: 0,
  orderFailedRecent: false,
  mailPerMin: 0,
  sessionTokPerMin: {},
};

export function CockpitHomePage() {
  const city = getActiveCity();
  const cityKey = city ?? 'no-city';

  const { source: runsSource } = useRunSummary();
  const attention = useAttentionModel();

  const [paused, setPaused] = useState(false);
  // Ref mirror so interval callbacks read the live pause state without
  // re-subscribing. Written in render (idempotent) so it is current before the
  // effects below run.
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  // While paused, hold the last live projection of the SSE-driven sources so
  // nothing on the wall moves. The tick + polls freeze separately (below).
  const runsView = useFrozenWhilePaused(runsSource, paused);
  const attentionView = useFrozenWhilePaused(attention, paused);

  const usageState = useCachedData<UsageBody>(`home:usage:${cityKey}`, () =>
    supervisorApi().cityUsage(activeCityOrThrow('cockpit usage read')),
  );
  const statusState = useCachedData<StatusBody>(`home:status:${cityKey}`, () =>
    supervisorApi().cityStatus(activeCityOrThrow('cockpit status read')),
  );
  const sessionsState = useCachedData(`home:sessions:${cityKey}`, () =>
    supervisorApi().listSessions(activeCityOrThrow('cockpit sessions read')),
  );
  const usageRefresh = usageState.refresh;
  const statusRefresh = statusState.refresh;
  const sessionsRefresh = sessionsState.refresh;

  // Short-poll the reads. Their server responses are time-bucket cached, so a
  // routine poll rides the cache; the interval keeps the dials live between the
  // event stream's coarser cues. Skipped while paused (poll results are not
  // applied), resuming on unpause.
  useEffect(() => {
    const id = setInterval(() => {
      if (!pausedRef.current) void usageRefresh();
    }, USAGE_POLL_MS);
    return () => clearInterval(id);
  }, [usageRefresh]);
  useEffect(() => {
    const id = setInterval(() => {
      if (!pausedRef.current) void statusRefresh();
    }, STATUS_POLL_MS);
    return () => clearInterval(id);
  }, [statusRefresh]);
  useEffect(() => {
    const id = setInterval(() => {
      if (!pausedRef.current) void sessionsRefresh();
    }, SESSIONS_POLL_MS);
    return () => clearInterval(id);
  }, [sessionsRefresh]);

  // Event feed fan-out. One subscription drives the oscilloscope (via the stable
  // `subscribe` handed to it) and every feed-derived reading. onEvent is
  // captured in a ref inside useGcEventFeed, so its identity is irrelevant.
  const listenersRef = useRef<Set<() => void>>(new Set());
  const eventTimesRef = useRef<number[]>([]);
  const mailSentTimesRef = useRef<number[]>([]);
  const workerOpsRef = useRef<WorkerOpSample[]>([]);
  const lastEventAtRef = useRef<number | null>(null);
  const orderFailedAtRef = useRef<number | null>(null);
  // Beads closed today: null until first seeded, then live-incremented and
  // periodically re-seeded to self-correct.
  const beadClosedCountRef = useRef<number | null>(null);

  const handleFeedEvent = useCallback((event: GcEventEnvelope) => {
    const now = Date.now();
    eventTimesRef.current.push(now);
    lastEventAtRef.current = now;
    switch (event.type) {
      case 'bead.closed':
        beadClosedCountRef.current = (beadClosedCountRef.current ?? 0) + 1;
        break;
      case 'order.failed':
        orderFailedAtRef.current = now;
        break;
      case 'mail.sent':
        mailSentTimesRef.current.push(now);
        break;
      case 'worker.operation': {
        const payload = event.payload;
        const tokens =
          payloadNum(payload, 'prompt_tokens') +
          payloadNum(payload, 'completion_tokens') +
          payloadNum(payload, 'cache_read_tokens') +
          payloadNum(payload, 'cache_creation_tokens');
        const cost = payloadNum(payload, 'cost_usd_estimate');
        const key =
          payloadStr(payload, 'session_id') ??
          payloadStr(payload, 'session_name') ??
          (typeof event.session_id === 'string' ? event.session_id : null);
        workerOpsRef.current.push({ t: now, tokens, cost, key });
        break;
      }
    }
    for (const listener of listenersRef.current) listener();
  }, []);

  const connState = useGcEventFeed(ALL_EVENT_PREFIXES, handleFeedEvent);

  const subscribe = useCallback((onEvent: () => void) => {
    listenersRef.current.add(onEvent);
    return () => {
      listenersRef.current.delete(onEvent);
    };
  }, []);

  // Seed (and periodically re-seed) beads-closed-today by counting bead.closed
  // events since local midnight. A failed seed leaves the live count untouched —
  // it must never zero a running counter.
  useEffect(() => {
    let cancelled = false;
    const seed = async () => {
      const activeCity = getActiveCity();
      if (activeCity === null) return;
      try {
        const res = await supervisorApi().listEvents(activeCity, {
          type: 'bead.closed',
          since: durationSinceMidnight(),
          limit: CLOSED_SEED_LIMIT,
        });
        if (!cancelled) beadClosedCountRef.current = res.items?.length ?? 0;
      } catch {
        // Leave the current count; a failed seed must not zero a live counter.
      }
    };
    void seed();
    const id = setInterval(() => {
      if (!pausedRef.current) void seed();
    }, CLOSED_RESEED_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [cityKey]);

  // Derive the feed-fed readings on a ~4Hz timer rather than per-event, so a
  // burst never storms React. Windows are pruned every tick (bounding memory
  // even while paused); the state write is gated on pause so the wall freezes.
  const [feedStats, setFeedStats] = useState<FeedStats>(EMPTY_FEED_STATS);
  useEffect(() => {
    const id = setInterval(() => {
      const now = Date.now();
      pruneBefore(eventTimesRef.current, now - EVENTS_WINDOW_MS);
      pruneBefore(mailSentTimesRef.current, now - MAIL_WINDOW_MS);
      pruneOpsBefore(workerOpsRef.current, now - WORKER_WINDOW_MS);
      if (pausedRef.current) return;

      let sumCost = 0;
      let sumTokens = 0;
      let hasTokenBearingOp = false;
      const sessionTokPerMin: Record<string, number> = {};
      for (const op of workerOpsRef.current) {
        sumCost += op.cost;
        sumTokens += op.tokens;
        if (op.tokens > 0) hasTokenBearingOp = true;
        if (op.key !== null && now - op.t <= VU_WINDOW_MS) {
          sessionTokPerMin[op.key] = (sessionTokPerMin[op.key] ?? 0) + op.tokens;
        }
      }

      setFeedStats({
        perMin: eventTimesRef.current.length,
        lastAgeMs: lastEventAtRef.current === null ? null : now - lastEventAtRef.current,
        windowCostPerHr: hasTokenBearingOp ? sumCost * (3600 / WORKER_WINDOW_SECS) : null,
        windowTokensPerSec: hasTokenBearingOp ? sumTokens / WORKER_WINDOW_SECS : null,
        beadsClosedToday: beadClosedCountRef.current ?? 0,
        orderFailedRecent:
          orderFailedAtRef.current !== null && now - orderFailedAtRef.current < ORDER_FAIL_RECENT_MS,
        mailPerMin: mailSentTimesRef.current.length,
        sessionTokPerMin,
      });
    }, TICK_MS);
    return () => clearInterval(id);
  }, []);

  // ── source projections ────────────────────────────────────────────────
  const runsLoading = runsView === undefined;
  const runsErrored = runsView?.status === 'error';
  const runs = runsView !== undefined && runsView.status !== 'error' ? runsView.data : null;
  const usage = usageState.data ?? null;
  const status = statusState.data ?? null;
  const sessions = sessionsState.data?.items ?? null;
  const usageMetrics = usage !== null ? computeUsageMetrics(usage) : null;

  // Live-or-fallback rates: the feed window wins when it carries token-bearing
  // ops, otherwise the short-poll usage read stands in. No fabrication — a
  // window of zero-value ops simply leaves the fallback in effect.
  const hasLiveRates = feedStats.windowCostPerHr !== null;
  const usageUnavailable = usage === null && !hasLiveRates;
  const burnPerHr = feedStats.windowCostPerHr ?? usageMetrics?.burnPerHr ?? 0;
  const tokensPerSec = feedStats.windowTokensPerSec ?? usageMetrics?.tokensPerSec ?? 0;

  const rings = useMemo(
    () => (runs !== null ? runs.lanes.slice(0, MAX_RINGS).map(laneToRing) : []),
    [runs],
  );
  const segments = useMemo(() => pipelineSegments(status?.work ?? null), [status]);
  const vuMeters = useMemo(
    () => buildVuMeters(sessions, usage, feedStats.sessionTokPerMin),
    [sessions, usage, feedStats.sessionTokPerMin],
  );
  const lamps = useMemo(
    () => statusLamps(status, feedStats, attentionView.items),
    [status, feedStats, attentionView.items],
  );

  // ── favicon alarm (R8 hysteresis) ─────────────────────────────────────
  // The favicon rides the LIVE attention model (not the pause-frozen view) so an
  // alarm is never silenced by a UI pause. `failing` is the count of actionable
  // ('attention'-severity) items; cycleKey advances per run snapshot.
  const failingCount = attention.items.filter((item) => item.severity === 'attention').length;
  const cycleKey =
    runsSource !== undefined && runsSource.status !== 'error' ? runsSource.fetchedAt : 'pre-snapshot';
  useFaviconSignal({ failing: failingCount, cycleKey });

  // ── header pieces ─────────────────────────────────────────────────────
  const burnKnown = usageMetrics !== null || hasLiveRates;
  const synopsis = (
    <span className="tnum">
      {city ?? 'city'} · {fmtCount(runs !== null ? runs.totalActive : null)} runs ·{' '}
      {fmtCount(status !== null ? (status.session_counts_detail?.active ?? status.running) : null)}{' '}
      sessions · {usageMetrics !== null ? fmtCompact(usageMetrics.tokensToday) : '—'} tokens ·{' '}
      {burnKnown ? `${fmtCost(burnPerHr)}/hr` : '—'}
    </span>
  );

  const liveTone = connState === 'open' ? 'ok' : 'warn';
  const liveLabel = paused ? 'paused' : connLabel(connState);
  const ageText =
    feedStats.lastAgeMs === null
      ? 'no events'
      : agoStr(feedStats.lastAgeMs) === 'now'
        ? 'last event now'
        : `last event ${agoStr(feedStats.lastAgeMs)} ago`;
  // Pulse the dot bright on a fresh event, dim as it ages; forced full under
  // reduced motion (the transition is disabled globally there anyway).
  const dotOpacity =
    prefersReducedMotion() || (feedStats.lastAgeMs !== null && feedStats.lastAgeMs < 1000)
      ? 1
      : 0.35;

  const liveChip = (
    <div className="flex items-center gap-3" data-testid="live-chip">
      <span className="inline-flex items-baseline gap-1.5">
        <span
          aria-hidden
          data-testid="live-dot"
          className={`text-[0.85em] leading-none transition-opacity duration-700 ease-out-quart ${
            liveTone === 'ok' ? 'text-ok' : 'text-warn'
          }`}
          style={{ opacity: dotOpacity }}
        >
          ●
        </span>
        <span className="text-label uppercase tracking-wider text-fg-muted" data-testid="live-label">
          {liveLabel}
        </span>
      </span>
      <span className="text-label uppercase tracking-wider tnum text-fg-faint">{ageText}</span>
      <button
        type="button"
        data-testid="scope-pause"
        onClick={() => setPaused((prev) => !prev)}
        aria-pressed={paused}
        className="inline-flex items-center rounded-sm border border-transparent px-2.5 py-1 text-label lowercase tracking-wider text-fg-muted transition-colors duration-150 ease-out-quart hover:text-fg focus-mark"
      >
        {paused ? 'resume' : 'pause'}
      </button>
    </div>
  );

  // ── needs-you strip (sole maroon, One Mark Rule) ──────────────────────
  const actionable = attentionView.items.filter(
    (item) => item.severity === 'attention' || item.severity === 'watch',
  );
  const needsYou = actionable[0] ?? null;
  const moreCount = Math.max(0, actionable.length - 1);
  const needsYouAgeMs =
    needsYou?.updatedAt !== undefined ? ageMsOf(needsYou.updatedAt) : null;

  const odometerSub =
    usageMetrics !== null ? `${fmtCost(usageMetrics.costToday)} est today` : '— est today';
  const gaugeNote = usageUnavailable ? 'usage unavailable' : undefined;
  const vuNote =
    sessions === null && usage === null
      ? 'usage unavailable'
      : vuMeters.length === 0
        ? 'no live sessions'
        : undefined;
  const systemsNote = status === null ? 'city status unavailable' : undefined;
  const ringsEmptyText = runsErrored
    ? 'run data unavailable'
    : runsLoading
      ? 'loading runs…'
      : 'no runs in flight';

  return (
    <section>
      <PageHeader title="Home" synopsis={synopsis} meta={liveChip} />

      {/* The needs-you strip: the ONLY place maroon may appear (One Mark Rule).
          The glyph + "needs you" label are wrapped in a single `.text-accent`
          element so the strip carries exactly one mark; the item link is not
          maroon. */}
      <div
        data-testid="needs-you"
        className="-mt-5 mb-[1.2rem] flex min-h-[2.4rem] items-baseline gap-3 border-y border-rule py-2"
      >
        {needsYou !== null ? (
          <>
            <span className="inline-flex items-baseline gap-2 text-accent">
              <span aria-hidden>●</span>
              <span className="text-label uppercase tracking-wider">needs you</span>
            </span>
            {needsYou.href !== undefined ? (
              <Link to={needsYou.href} className="focus-mark text-body text-fg hover:underline">
                {needsYou.title}
              </Link>
            ) : (
              <span className="text-body text-fg">{needsYou.title}</span>
            )}
            {needsYouAgeMs !== null && (
              <span className="text-label tnum text-fg-faint">{agoStr(needsYouAgeMs)} ago</span>
            )}
            {moreCount > 0 && (
              <span className="text-label tnum text-fg-muted">· {moreCount} more</span>
            )}
          </>
        ) : (
          <p className="text-body italic text-fg-muted">nothing needs you</p>
        )}
      </div>

      {/* Band 1 — event oscilloscope. */}
      <div className="mb-[1.6rem]">
        <Oscilloscope subscribe={subscribe} paused={paused} />
      </div>

      {/* Band 2 — odometer + dial row. Every instrument stays mounted; a
          degraded source rests at zero with a small microcopy line. */}
      <div className="mb-[1.8rem] flex flex-wrap items-start justify-between gap-x-10 gap-y-8" data-testid="dial-row">
        <Odometer
          value={feedStats.beadsClosedToday}
          digits={4}
          label="beads closed today"
          sub={odometerSub}
        />
        <DialCell>
          <Gauge
            label="events / min"
            value={feedStats.perMin}
            max={EVENTS_MAX}
            warnFrom={EVENTS_WARN}
            format={(value) => String(Math.round(value))}
            tickFormat={(value) => String(Math.round(value))}
            href="/activity?mode=events"
          />
        </DialCell>
        <DialCell note={gaugeNote} noteTestId="burn-note">
          <Gauge
            label="burn · $ / hr"
            value={burnPerHr}
            max={BURN_MAX}
            warnFrom={BURN_WARN}
            format={(value) => fmtCost(value)}
            tickFormat={(value) => String(Math.round(value))}
            href="/activity?mode=events&type=worker.operation"
          />
        </DialCell>
        <DialCell note={gaugeNote} noteTestId="tokens-note">
          <Gauge
            label="tokens / s"
            value={tokensPerSec}
            max={TOKENS_MAX}
            warnFrom={TOKENS_WARN}
            format={(value) => fmtRate(value)}
            tickFormat={(value) => `${Math.round(value / 1000)}k`}
            href="/activity?mode=events&type=worker.operation"
          />
        </DialCell>
      </div>

      {/* Band 3 — bead-status pipeline (always present; zero total = even split). */}
      <div className="mb-[1.8rem]">
        <PipelineBar segments={segments} />
      </div>

      {/* Band 4 — session VU bank / run rings / system lamps. */}
      <div className="grid grid-cols-1 gap-10 lg:[grid-template-columns:5fr_4fr_3fr]">
        <section aria-label="Session token throughput">
          <ColumnHeading>session throughput · tok/min</ColumnHeading>
          <VUBank meters={vuMeters} max={VU_MAX} />
          {vuNote !== undefined && <Microcopy testId="vu-note">{vuNote}</Microcopy>}
        </section>

        <section aria-label="Active run progress">
          <ColumnHeading>formula runs · stage progress</ColumnHeading>
          {rings.length > 0 ? (
            <RunRings runs={rings} />
          ) : (
            <p data-testid="rings-empty" className="text-body italic text-fg-muted">
              {ringsEmptyText}
            </p>
          )}
        </section>

        <section aria-label="System status">
          <ColumnHeading>systems</ColumnHeading>
          <StatusLamps lamps={lamps} />
          {systemsNote !== undefined && <Microcopy testId="systems-note">{systemsNote}</Microcopy>}
        </section>
      </div>
    </section>
  );
}

// ── freeze-while-paused ───────────────────────────────────────────────────

/**
 * Holds the last live value while `paused` is true so an SSE-driven source stops
 * moving on the wall; returns the live value again the moment pause lifts. The
 * ref write during render is idempotent (same input → same output).
 */
function useFrozenWhilePaused<T>(live: T, paused: boolean): T {
  const ref = useRef(live);
  if (!paused) ref.current = live;
  return ref.current;
}

// ── usage metrics ─────────────────────────────────────────────────────────

interface UsageMetrics {
  burnPerHr: number;
  tokensPerSec: number;
  tokensToday: number;
  costToday: number;
}

function computeUsageMetrics(usage: UsageBody): UsageMetrics {
  const windowSecs = Math.max(usage.recent_window_secs, 1);
  return {
    burnPerHr: usage.recent.cost_usd_estimate * (3600 / windowSecs),
    tokensPerSec: sumTokens(usage.recent) / windowSecs,
    tokensToday: sumTokens(usage.today),
    costToday: usage.today.cost_usd_estimate,
  };
}

// The four token dimensions shared by UsageTotals and UsageSessionRecent.
interface TokenCounts {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
}

function sumTokens(tokens: TokenCounts): number {
  return (
    tokens.input_tokens +
    tokens.output_tokens +
    tokens.cache_read_tokens +
    tokens.cache_creation_tokens
  );
}

// ── VU roster ─────────────────────────────────────────────────────────────

interface RosterRow {
  id: string;
  name: string;
  rig: string;
  /** Candidate keys used to match live feed samples and usage rows. */
  keys: string[];
}

/**
 * buildVuMeters produces one meter per live session, sorted by rig then name and
 * capped at MAX_VU. Each value is the session's live tok/min from the
 * `worker.operation` window when present, else its usage-window tokens
 * normalized to per-minute, else zero. Meters deep-link to the agents roster
 * (usage/session names are not clean AgentDetail slugs).
 */
function buildVuMeters(
  sessions: SessionResponse[] | null,
  usage: UsageBody | null,
  liveByKey: Record<string, number>,
): VUMeter[] {
  const roster = sessionRoster(sessions, usage);
  return roster
    .map((row) => ({ row, value: meterValue(row, usage, liveByKey) }))
    .sort((a, b) => a.row.rig.localeCompare(b.row.rig) || a.row.name.localeCompare(b.row.name))
    .slice(0, MAX_VU)
    .map(({ row, value }) => ({ id: row.id, name: row.name, value, href: '/agents' }));
}

function sessionRoster(
  sessions: SessionResponse[] | null,
  usage: UsageBody | null,
): RosterRow[] {
  if (sessions !== null) {
    return sessions.filter((s) => s.running).map(sessionToRoster);
  }
  // Fall back to the usage roster when the sessions read is unavailable, so the
  // bank still populates from whatever live signal exists.
  return (usage?.recent_by_session ?? []).map(usageToRoster);
}

function sessionToRoster(session: SessionResponse): RosterRow {
  const raw = session.session_name;
  const slash = raw.indexOf('/');
  const rig =
    session.rig !== undefined && session.rig.length > 0
      ? session.rig
      : slash > 0
        ? raw.slice(0, slash)
        : '';
  const name = cleanWorkerName(slash > 0 ? raw.slice(slash + 1) : raw);
  const keys = uniqueStrings([session.id, cleanWorkerName(raw), name, session.alias]);
  return { id: session.id, name, rig, keys };
}

function usageToRoster(session: UsageSessionRecent): RosterRow {
  const name = cleanWorkerName(session.session);
  return {
    id: session.session_id ?? session.session,
    name,
    rig: '',
    keys: uniqueStrings([session.session_id, session.session, name]),
  };
}

function meterValue(
  row: RosterRow,
  usage: UsageBody | null,
  liveByKey: Record<string, number>,
): number {
  for (const key of row.keys) {
    const live = liveByKey[key];
    if (live !== undefined) return live;
  }
  const windowSecs = Math.max(usage?.recent_window_secs ?? 0, 1);
  for (const entry of usage?.recent_by_session ?? []) {
    const entryName = cleanWorkerName(entry.session);
    if (
      (entry.session_id !== undefined && row.keys.includes(entry.session_id)) ||
      row.keys.includes(entryName)
    ) {
      return (sumTokens(entry) * 60) / windowSecs;
    }
  }
  return 0;
}

function uniqueStrings(values: Array<string | undefined>): string[] {
  const out: string[] = [];
  for (const value of values) {
    if (value !== undefined && value.length > 0 && !out.includes(value)) out.push(value);
  }
  return out;
}

// ── pipeline / rings / lamps ──────────────────────────────────────────────

function pipelineSegments(work: StatusWorkCounts | null): PipelineSegment[] {
  return PIPELINE_STAGES.map((stage) => ({
    key: stage.key,
    label: stage.label,
    count: work !== null ? stage.pick(work) : 0,
    href: '/beads',
  }));
}

function laneToRing(lane: RunLane): RunRing {
  const progress = lane.progress;
  const stagePos =
    (progress.status === 'active_step' || progress.status === 'stage_only') &&
    progress.stage.status === 'available'
      ? progress.stage
      : null;
  const stage = stagePos !== null ? stagePos.index + 1 : (PHASE_STAGE_INDEX[lane.phase] ?? 1);
  const stageWord = stagePos !== null ? stagePos.label : lane.phaseLabel;
  const attempt =
    progress.status === 'active_step' && progress.attempt.status === 'available'
      ? progress.attempt.value
      : undefined;
  return {
    id: lane.id,
    label: laneLabel(lane),
    stage,
    stageWord,
    ...(attempt === undefined ? {} : { attempt }),
    href: runDetailHref(lane.id, lane.scope),
  };
}

function laneLabel(lane: RunLane): string {
  if (lane.external.status === 'available' || lane.external.status === 'label_only') {
    return lane.external.label;
  }
  if (lane.formula.status === 'known') return lane.formula.name;
  return lane.title;
}

function statusLamps(
  status: StatusBody | null,
  feed: FeedStats,
  attentionItems: readonly AttentionItem[],
): StatusLamp[] {
  const incidents = attentionItems.filter((item) => item.severity === 'attention').length;
  const store = status?.store_health;
  return [
    {
      id: 'orders',
      label: 'orders',
      value: feed.orderFailedRecent ? 'recent failure' : 'firing on time',
      tone: feed.orderFailedRecent ? 'warn' : 'ok',
      href: '/activity?mode=events&type=order.fired',
    },
    {
      id: 'patrol',
      label: 'patrol',
      value: incidents > 0 ? `${incidents} incident${incidents > 1 ? 's' : ''}` : 'quiet',
      tone: incidents > 0 ? 'warn' : 'ok',
      href: '/agents',
    },
    store !== undefined
      ? {
          id: 'store',
          label: 'dolt store',
          value: fmtBytes(store.size_bytes),
          tone: store.warning ? 'warn' : 'ok',
          href: '/health',
        }
      : { id: 'store', label: 'dolt store', value: '—', tone: 'ok', dim: true, href: '/health' },
    {
      id: 'mail',
      label: 'mail traffic',
      value: `${feed.mailPerMin}/min`,
      tone: 'ok',
      dim: feed.mailPerMin === 0,
      href: '/mail',
    },
  ];
}

// ── feed helpers ──────────────────────────────────────────────────────────

function payloadNum(payload: Record<string, unknown> | undefined, key: string): number {
  const value = payload?.[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

function payloadStr(payload: Record<string, unknown> | undefined, key: string): string | null {
  const value = payload?.[key];
  return typeof value === 'string' && value.length > 0 ? value : null;
}

/** Drops leading timestamps older than `cutoff` from an ascending-time array. */
function pruneBefore(times: number[], cutoff: number): void {
  while (times.length > 0 && times[0]! < cutoff) times.shift();
}

function pruneOpsBefore(ops: WorkerOpSample[], cutoff: number): void {
  while (ops.length > 0 && ops[0]!.t < cutoff) ops.shift();
}

// ── formatting ────────────────────────────────────────────────────────────

function connLabel(state: string): string {
  switch (state) {
    case 'open':
      return 'live';
    case 'connecting':
      return 'connecting';
    case 'degraded':
      return 'degraded';
    default:
      return 'offline';
  }
}

function fmtCount(value: number | null): string {
  return value === null ? '—' : String(value);
}

function fmtBytes(bytes: number): string {
  if (bytes >= 1_000_000_000) return `${(bytes / 1_000_000_000).toFixed(1)} GB`;
  return `${(bytes / 1_000_000).toFixed(0)} MB`;
}

/**
 * durationSinceMidnight formats the elapsed time since local midnight as a Go
 * duration string (e.g. "14h23m"), the shape the events `since` filter parses.
 */
function durationSinceMidnight(now: Date = new Date()): string {
  const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const totalMinutes = Math.max(0, Math.floor((now.getTime() - midnight.getTime()) / 60_000));
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours > 0) return `${hours}h${minutes}m`;
  if (minutes > 0) return `${minutes}m`;
  // Just past midnight — a small non-zero window keeps the filter well-formed.
  return '1m';
}

function ageMsOf(iso: string): number | null {
  const parsed = Date.parse(iso);
  if (!Number.isFinite(parsed)) return null;
  return Math.max(0, Date.now() - parsed);
}

function prefersReducedMotion(): boolean {
  return (
    typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches
  );
}

// ── small presentational helpers ──────────────────────────────────────────

function ColumnHeading({ children }: { children: ReactNode }) {
  return <p className="mb-2 text-label uppercase tracking-wider text-fg-faint">{children}</p>;
}

function DialCell({
  children,
  note,
  noteTestId,
}: {
  children: ReactNode;
  note?: string | undefined;
  noteTestId?: string | undefined;
}) {
  return (
    <div className="flex flex-col items-center gap-1">
      {children}
      {note !== undefined && <Microcopy testId={noteTestId}>{note}</Microcopy>}
    </div>
  );
}

function Microcopy({ children, testId }: { children: ReactNode; testId?: string | undefined }) {
  return (
    <p className="text-label italic text-fg-faint" {...(testId ? { 'data-testid': testId } : {})}>
      {children}
    </p>
  );
}
