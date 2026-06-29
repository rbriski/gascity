import type {
  FormulaRecentRunResponse,
  FormulaSummaryResponse,
  GetV0CityByCityNameFormulasByNameRunsData,
  GetV0CityByCityNameFormulasData,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow } from '../api/cityBase';
import type { StatusTone } from '../components/StatusBadge';
import { supervisorApi } from './client';

// Read adapter for the Formulas tab. Routes import ONLY from here (never the
// supervisor client or generated SDK directly), mirroring the stable
// read-adapter pattern used by beadReads. Two jobs:
//   - normalize the wire's `T[] | null` arrays (items / recent_runs) to [], so
//     callers map/length without guards;
//   - depend only on STABLE supervisor DTOs (FormulaListBody / FormulaRunsResponse),
//     never on feat/runproj-event-sourcing run-view/projection internals — so a
//     rebase onto the churning base branch can't break this surface.

export type SupervisorFormula = FormulaSummaryResponse;
export type SupervisorFormulaRun = FormulaRecentRunResponse;

export interface FormulaScope {
  scope_kind?: string;
  scope_ref?: string;
}

/** Catalog list of formula definitions for the active city. */
export async function listSupervisorFormulas(scope?: FormulaScope): Promise<SupervisorFormula[]> {
  const cityName = activeCityOrThrow('list supervisor formulas');
  const body = await supervisorApi().formulas(cityName, scopeQuery(scope));
  return body.items ?? [];
}

/** Recent runs for one formula (newest first, per the supervisor). */
export async function getSupervisorFormulaRuns(
  name: string,
  scope?: FormulaScope & { limit?: number },
): Promise<SupervisorFormulaRun[]> {
  const cityName = activeCityOrThrow('get supervisor formula runs');
  const base = scopeQuery(scope);
  const limit = scope?.limit;
  const query: NonNullable<GetV0CityByCityNameFormulasByNameRunsData['query']> | undefined =
    base === undefined && limit === undefined
      ? undefined
      : { ...(base ?? {}), ...(limit === undefined ? {} : { limit }) };
  const body = await supervisorApi().formulaRuns(cityName, name, query);
  return body.recent_runs ?? [];
}

function scopeQuery(
  scope: FormulaScope | undefined,
): NonNullable<GetV0CityByCityNameFormulasData['query']> | undefined {
  if (scope === undefined) return undefined;
  const query: NonNullable<GetV0CityByCityNameFormulasData['query']> = {};
  if (scope.scope_kind) query.scope_kind = scope.scope_kind;
  if (scope.scope_ref) query.scope_ref = scope.scope_ref;
  return Object.keys(query).length > 0 ? query : undefined;
}

/**
 * Map a formula run's free-form status to a StatusBadge tone. Color is never the
 * sole signal (StatusBadge pairs glyph + word), so two states sharing a tone
 * (done and running are both `ok`) still read distinctly in the greyscale test.
 */
export function recentRunTone(status: string): StatusTone {
  switch (status.trim().toLowerCase()) {
    case 'completed':
    case 'done':
    case 'closed':
    case 'success':
    case 'succeeded':
    case 'running':
    case 'active':
    case 'in_progress':
      return 'ok';
    case 'failed':
    case 'error':
    case 'errored':
      return 'stuck';
    case 'blocked':
    case 'waiting':
      return 'warn';
    default:
      return 'neutral';
  }
}
