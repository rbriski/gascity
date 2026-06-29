import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import { type SupervisorApi, resetSupervisorApiForTests, setSupervisorApiForTests } from './client';
import { slingFormula } from './formulaWrites';

function stubSling(sling: ReturnType<typeof vi.fn>): void {
  setSupervisorApiForTests({ sling } as unknown as SupervisorApi);
}

beforeEach(() => setActiveCity('test-city'));
afterEach(() => {
  resetSupervisorApiForTests();
  vi.restoreAllMocks();
});

describe('slingFormula', () => {
  it('builds a formula-native sling body (formula + target + vars, never a bead)', async () => {
    const sling = vi.fn(async () => ({ status: 'ok', target: 'reviewer', workflow_id: 'wf_1' }));
    stubSling(sling);

    await expect(
      slingFormula({ formula: 'code-review', target: 'reviewer', vars: { repo: 'gc/ds' } }),
    ).resolves.toMatchObject({ workflow_id: 'wf_1' });

    expect(sling).toHaveBeenCalledWith('test-city', {
      formula: 'code-review',
      target: 'reviewer',
      vars: { repo: 'gc/ds' },
    });
    const body = sling.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(body).not.toHaveProperty('bead');
  });

  it('drops empty var values and omits vars entirely when none remain', async () => {
    const sling = vi.fn(async () => ({ status: 'ok', target: 'reviewer' }));
    stubSling(sling);

    await slingFormula({ formula: 'demo', target: 'reviewer', vars: { a: 'x', b: '' } });
    expect(sling).toHaveBeenCalledWith('test-city', {
      formula: 'demo',
      target: 'reviewer',
      vars: { a: 'x' },
    });

    sling.mockClear();
    await slingFormula({ formula: 'demo', target: 'reviewer', vars: { a: '', b: '' } });
    expect(sling.mock.calls[0]?.[1]).toEqual({ formula: 'demo', target: 'reviewer' });
  });

  it('forwards scope + title only when provided', async () => {
    const sling = vi.fn(async () => ({ status: 'ok', target: 'reviewer' }));
    stubSling(sling);

    await slingFormula({
      formula: 'demo',
      target: 'reviewer',
      scopeKind: 'rig',
      scopeRef: 'east',
      title: 'nightly',
    });
    expect(sling).toHaveBeenCalledWith('test-city', {
      formula: 'demo',
      target: 'reviewer',
      scope_kind: 'rig',
      scope_ref: 'east',
      title: 'nightly',
    });
  });

  it('rejects an empty formula or target before any sling', async () => {
    const sling = vi.fn();
    stubSling(sling);

    await expect(slingFormula({ formula: '  ', target: 'reviewer' })).rejects.toThrow(/formula/);
    await expect(slingFormula({ formula: 'demo', target: '' })).rejects.toThrow(/target/);
    expect(sling).not.toHaveBeenCalled();
  });
});
