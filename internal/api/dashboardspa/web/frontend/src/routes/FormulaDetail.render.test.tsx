import { cleanup, render, screen, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import { invalidate } from '../api/cache';
import { resetSupervisorApiForTests } from '../supervisor/client';
import { NowProvider } from '../contexts/NowContext';
import { ReadOnlyProvider } from '../contexts/ReadOnlyContext';
import { FormulaDetailPage } from './FormulaDetail';

interface FetchCall {
  method: string;
  path: string;
}
const fetchCalls: FetchCall[] = [];

const FORMULAS_PATH = /^\/v0\/city\/[^/]+\/formulas$/;
const RUNS_PATH = /^\/v0\/city\/[^/]+\/formulas\/[^/]+\/runs$/;
const DETAIL_PATH = /^\/v0\/city\/[^/]+\/formulas\/[^/]+$/; // target-required; must NOT be called

let cityCounter = 0;

function parsedUrl(input: RequestInfo | URL): URL {
  if (input instanceof Request) return new URL(input.url);
  if (input instanceof URL) return input;
  return new URL(String(input), 'http://localhost');
}
function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

const FORMULA = {
  name: 'code-review',
  description: 'Review a change, then publish.',
  version: 'v2',
  run_count: 2,
  recent_runs: [],
  var_defs: [
    { name: 'repo', type: 'string', required: true, description: 'The repo to review.' },
    { name: 'base_branch', type: 'string', default: 'main' },
  ],
};

function stubFetch(opts: { formulas?: unknown[]; runs?: unknown[]; runsStatus?: number } = {}): void {
  const formulas = opts.formulas ?? [FORMULA];
  const runs = opts.runs ?? [];
  fetchCalls.length = 0;
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = parsedUrl(input);
      const method = init?.method ?? (input instanceof Request ? input.method : 'GET');
      fetchCalls.push({ method, path: url.pathname });
      if (FORMULAS_PATH.test(url.pathname)) {
        return jsonResponse({ items: formulas, partial: false, total: formulas.length });
      }
      if (RUNS_PATH.test(url.pathname)) {
        return jsonResponse(
          { formula: 'code-review', partial: false, recent_runs: runs, run_count: runs.length },
          opts.runsStatus ?? 200,
        );
      }
      return jsonResponse({ error: `unexpected ${url.pathname}` }, 404);
    }),
  );
}

function renderDetail(name = 'code-review') {
  return render(
    <MemoryRouter
      initialEntries={[`/formulas/${name}`]}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <NowProvider intervalMs={1_000_000}>
        <ReadOnlyProvider readOnly={false}>
          <Routes>
            <Route path="/formulas/:name" element={<FormulaDetailPage />} />
          </Routes>
        </ReadOnlyProvider>
      </NowProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  setActiveCity(`test-city-${++cityCounter}`);
  invalidate('');
  stubFetch();
});

afterEach(() => {
  cleanup();
  resetSupervisorApiForTests();
  vi.unstubAllGlobals();
});

describe('FormulaDetailPage', () => {
  it('renders name, version, description, and a vars definition list from the catalog summary', async () => {
    renderDetail();

    expect(await screen.findByRole('heading', { name: 'code-review' })).toBeTruthy();
    expect(screen.getByText(/Version v2/)).toBeTruthy();
    expect(screen.getByText('repo')).toBeTruthy();
    expect(screen.getByText(/string · required/)).toBeTruthy();
    expect(screen.getByText('The repo to review.')).toBeTruthy();
    expect(screen.getByText(/default "main"/)).toBeTruthy();
  });

  it('does NOT fetch the target-required compiled detail on initial render', async () => {
    renderDetail();
    await screen.findByRole('heading', { name: 'code-review' });

    const detailHits = fetchCalls.filter(
      (c) => DETAIL_PATH.test(c.path) && !RUNS_PATH.test(c.path),
    );
    expect(detailHits).toHaveLength(0);
  });

  it('lists recent runs, each deep-linking to its run detail', async () => {
    stubFetch({
      runs: [
        {
          workflow_id: 'wf_9a1c',
          status: 'done',
          target: 'reviewer',
          started_at: '2026-06-01T00:00:00Z',
          updated_at: '2026-06-01T01:00:00Z',
        },
      ],
    });
    renderDetail();

    const runLink = await screen.findByRole('link', { name: /wf_9a1c/ });
    expect(runLink.getAttribute('href')).toBe('/runs/wf_9a1c');
    expect(within(runLink as HTMLElement).getByText('done')).toBeTruthy();
  });

  it('shows a no-runs message when the formula has never run', async () => {
    renderDetail();
    await screen.findByRole('heading', { name: 'code-review' });
    expect(screen.getByText('No runs yet.')).toBeTruthy();
  });

  it('reserves the launcher slot (mounted in the next slice)', async () => {
    renderDetail();
    await screen.findByRole('heading', { name: 'code-review' });
    expect(document.querySelector('[data-testid="formula-launcher-slot"]')).not.toBeNull();
  });

  it('renders an honest not-found when the name is absent from the catalog', async () => {
    stubFetch({ formulas: [] });
    renderDetail('ghost');

    expect(await screen.findByText("This formula is not in the city's catalog.")).toBeTruthy();
  });
});
