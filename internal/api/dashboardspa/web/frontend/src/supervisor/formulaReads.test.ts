import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type {
  FormulaRecentRunResponse,
  FormulaSummaryResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { setActiveCity } from '../api/cityBase';
import {
  type SupervisorApi,
  SupervisorApiError,
  resetSupervisorApiForTests,
  setSupervisorApiForTests,
} from './client';
import {
  cityScope,
  getSupervisorFormulaRuns,
  getSupervisorFormulaSteps,
  listSupervisorFormulas,
  recentRunTone,
} from './formulaReads';

function stub(over: Partial<SupervisorApi>): void {
  setSupervisorApiForTests(over as unknown as SupervisorApi);
}

function summary(over: Partial<FormulaSummaryResponse> = {}): FormulaSummaryResponse {
  return {
    name: 'code-review',
    description: 'Review a change.',
    version: 'v2',
    run_count: 3,
    recent_runs: null,
    var_defs: null,
    ...over,
  };
}

function run(over: Partial<FormulaRecentRunResponse> = {}): FormulaRecentRunResponse {
  return {
    workflow_id: 'wf1',
    status: 'done',
    target: 'reviewer',
    started_at: '2026-06-01T00:00:00Z',
    updated_at: '2026-06-01T01:00:00Z',
    ...over,
  };
}

beforeEach(() => setActiveCity('test-city'));
afterEach(() => {
  resetSupervisorApiForTests();
  vi.restoreAllMocks();
});

describe('formulaReads', () => {
  it('lists the active city formulas and passes no query by default', async () => {
    const formulas = vi.fn(async () => ({ items: [summary()], partial: false, total: 1 }));
    stub({ formulas });

    await expect(listSupervisorFormulas()).resolves.toEqual([
      expect.objectContaining({ name: 'code-review', run_count: 3 }),
    ]);
    expect(formulas).toHaveBeenCalledWith('test-city', undefined);
  });

  it('normalizes a null formulas list to []', async () => {
    stub({ formulas: vi.fn(async () => ({ items: null, partial: false, total: 0 })) });
    await expect(listSupervisorFormulas()).resolves.toEqual([]);
  });

  it('forwards scope query params only when present', async () => {
    const formulas = vi.fn(async () => ({ items: [], partial: false, total: 0 }));
    stub({ formulas });

    await listSupervisorFormulas({ scope_kind: 'rig', scope_ref: 'east' });
    expect(formulas).toHaveBeenCalledWith('test-city', { scope_kind: 'rig', scope_ref: 'east' });
  });

  it('scopes the catalog to the active city (backend rejects a scope-less request)', async () => {
    const formulas = vi.fn(async () => ({ items: [], partial: false, total: 0 }));
    stub({ formulas });

    await listSupervisorFormulas(cityScope('test-city'));
    expect(formulas).toHaveBeenCalledWith('test-city', {
      scope_kind: 'city',
      scope_ref: 'test-city',
    });
  });

  it('cityScope returns undefined for an unresolved city', () => {
    expect(cityScope(null)).toBeUndefined();
    expect(cityScope('')).toBeUndefined();
    expect(cityScope('gc')).toEqual({ scope_kind: 'city', scope_ref: 'gc' });
  });

  it('reads recent runs for a formula', async () => {
    const formulaRuns = vi.fn(async () => ({
      formula: 'demo',
      partial: false,
      recent_runs: [run({ workflow_id: 'wf-a' })],
      run_count: 1,
    }));
    stub({ formulaRuns });

    await expect(getSupervisorFormulaRuns('demo')).resolves.toEqual([
      expect.objectContaining({ workflow_id: 'wf-a' }),
    ]);
    expect(formulaRuns).toHaveBeenCalledWith('test-city', 'demo', undefined);
  });

  it('normalizes null recent_runs to [] and forwards a limit', async () => {
    const formulaRuns = vi.fn(async () => ({
      formula: 'demo',
      partial: false,
      recent_runs: null,
      run_count: 0,
    }));
    stub({ formulaRuns });

    await expect(getSupervisorFormulaRuns('demo', { limit: 5 })).resolves.toEqual([]);
    expect(formulaRuns).toHaveBeenCalledWith('test-city', 'demo', { limit: 5 });
  });

  it('propagates a SupervisorApiError from the facade (no swallow)', async () => {
    stub({
      formulas: vi.fn(async () => {
        throw new SupervisorApiError(500, 'boom', undefined);
      }),
    });
    await expect(listSupervisorFormulas()).rejects.toBeInstanceOf(SupervisorApiError);
  });

  it('compiles target-bound steps and normalizes null steps to []', async () => {
    const formulaDetail = vi.fn(async () => ({
      name: 'demo',
      description: '',
      version: 'v1',
      preview: { nodes: [], edges: [] },
      deps: null,
      var_defs: null,
      steps: [{ id: 'review', kind: 'agent', title: 'Review' }],
    }));
    stub({ formulaDetail });

    await expect(getSupervisorFormulaSteps('demo', 'reviewer')).resolves.toEqual([
      expect.objectContaining({ id: 'review' }),
    ]);
    expect(formulaDetail).toHaveBeenCalledWith('test-city', 'demo', { target: 'reviewer' });

    formulaDetail.mockResolvedValueOnce({
      name: 'demo',
      description: '',
      version: 'v1',
      preview: { nodes: [], edges: [] },
      deps: null,
      var_defs: null,
      steps: null,
    });
    await expect(getSupervisorFormulaSteps('demo', 'reviewer')).resolves.toEqual([]);
  });

  it('maps run status to a glyph+word StatusBadge tone', () => {
    expect(recentRunTone('completed')).toBe('ok');
    expect(recentRunTone('done')).toBe('ok');
    expect(recentRunTone('running')).toBe('ok');
    expect(recentRunTone('failed')).toBe('stuck');
    expect(recentRunTone('blocked')).toBe('warn');
    expect(recentRunTone('queued')).toBe('neutral');
    expect(recentRunTone('')).toBe('neutral');
  });
});
