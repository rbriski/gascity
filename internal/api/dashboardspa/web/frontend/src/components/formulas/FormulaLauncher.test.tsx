import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FormulaVarDefResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { setActiveCity } from '../../api/cityBase';
import { invalidate } from '../../api/cache';
import { resetSupervisorApiForTests } from '../../supervisor/client';
import { READ_ONLY_CONTROL_TITLE, ReadOnlyProvider } from '../../contexts/ReadOnlyContext';
import { FormulaLauncher } from './FormulaLauncher';

interface Recorded {
  method: string;
  path: string;
  body?: unknown;
}
const calls: Recorded[] = [];
let cityCounter = 0;

const AGENTS_PATH = /\/agents$/;
const DETAIL_PATH = /\/formulas\/[^/]+$/;
const SLING_PATH = /\/sling$/;

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

function stubFetch(opts: { slingStatus?: number; slingBody?: unknown } = {}): void {
  calls.length = 0;
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = parsedUrl(input);
      const method = init?.method ?? (input instanceof Request ? input.method : 'GET');
      let body: unknown;
      try {
        if (input instanceof Request) body = await input.clone().json();
        else if (init?.body) body = JSON.parse(String(init.body));
      } catch {
        body = undefined;
      }
      calls.push({ method, path: url.pathname, body });

      if (SLING_PATH.test(url.pathname)) {
        return jsonResponse(
          opts.slingBody ?? { status: 'ok', target: 'reviewer', workflow_id: 'wf_new' },
          opts.slingStatus ?? 200,
        );
      }
      if (AGENTS_PATH.test(url.pathname)) {
        return jsonResponse({
          items: [
            {
              name: 'reviewer',
              available: true,
              running: true,
              suspended: false,
              state: 'idle',
              provider: 'claude',
            },
            {
              name: 'fixer',
              available: true,
              running: false,
              suspended: false,
              state: 'idle',
              provider: 'codex',
            },
          ],
          total: 2,
        });
      }
      if (DETAIL_PATH.test(url.pathname)) {
        return jsonResponse({
          name: 'code-review',
          description: '',
          version: 'v2',
          preview: { nodes: [], edges: [] },
          deps: null,
          var_defs: null,
          steps: [
            { id: 'review', kind: 'agent', title: 'Review the change', assignee: 'reviewer' },
          ],
        });
      }
      return jsonResponse({ error: `unexpected ${url.pathname}` }, 404);
    }),
  );
}

const VAR_DEFS: FormulaVarDefResponse[] = [
  { name: 'repo', type: 'string', required: true },
  { name: 'mode', type: 'string', enum: ['fast', 'slow'] },
];

function renderLauncher(opts: { readOnly?: boolean; onLaunched?: (id: string) => void } = {}) {
  return render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <ReadOnlyProvider readOnly={opts.readOnly ?? false}>
        <FormulaLauncher name="code-review" varDefs={VAR_DEFS} onLaunched={opts.onLaunched} />
      </ReadOnlyProvider>
    </MemoryRouter>,
  );
}

function targetInput(): HTMLInputElement {
  return screen.getByPlaceholderText('agent or pool') as HTMLInputElement;
}
function launchButton(): HTMLButtonElement {
  return screen.getByRole('button', { name: 'Launch run' }) as HTMLButtonElement;
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

describe('FormulaLauncher', () => {
  it('disables Launch and shows the read-only affordance in read-only mode', () => {
    renderLauncher({ readOnly: true });
    const btn = launchButton();
    expect(btn.disabled).toBe(true);
    expect(btn.getAttribute('title')).toBe(READ_ONLY_CONTROL_TITLE);
    expect(screen.getByText('Read-only')).toBeTruthy();
  });

  it('does not sling when read-only even if a programmatic click slips through', () => {
    renderLauncher({ readOnly: true });
    fireEvent.click(launchButton());
    expect(calls.some((c) => c.method === 'POST' && SLING_PATH.test(c.path))).toBe(false);
  });

  it('keeps Launch disabled until a target and required vars are filled', () => {
    renderLauncher();
    expect(launchButton().disabled).toBe(true);
    fireEvent.change(targetInput(), { target: { value: 'reviewer' } });
    expect(launchButton().disabled).toBe(true); // required var "repo" still empty
    fireEvent.change(screen.getByLabelText('repo (required)'), { target: { value: 'gc/ds' } });
    expect(launchButton().disabled).toBe(false);
  });

  it('slings a formula-native body and links the new run on success', async () => {
    const onLaunched = vi.fn();
    renderLauncher({ onLaunched });
    fireEvent.change(targetInput(), { target: { value: 'reviewer' } });
    fireEvent.change(screen.getByLabelText('repo (required)'), { target: { value: 'gc/ds' } });
    fireEvent.click(launchButton());

    const link = await screen.findByRole('link', { name: 'wf_new' });
    expect(link.getAttribute('href')).toBe('/runs/wf_new');

    const sling = calls.find((c) => c.method === 'POST' && SLING_PATH.test(c.path));
    expect(sling?.body).toEqual({
      formula: 'code-review',
      target: 'reviewer',
      vars: { repo: 'gc/ds' },
    });
    expect(onLaunched).toHaveBeenCalledWith('wf_new');
  });

  it('renders enum vars as a select of options', () => {
    renderLauncher();
    const select = screen.getByLabelText('mode') as HTMLSelectElement;
    expect(select.tagName).toBe('SELECT');
    expect(within(select).getByRole('option', { name: 'fast' })).toBeTruthy();
  });

  it('surfaces a sling error as role=alert', async () => {
    stubFetch({ slingStatus: 503, slingBody: { error: 'supervisor down' } });
    renderLauncher();
    fireEvent.change(targetInput(), { target: { value: 'reviewer' } });
    fireEvent.change(screen.getByLabelText('repo (required)'), { target: { value: 'gc/ds' } });
    fireEvent.click(launchButton());

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/supervisor down/i);
  });

  it('compiles a target-bound steps preview once a target is chosen', async () => {
    renderLauncher();
    fireEvent.change(targetInput(), { target: { value: 'reviewer' } });

    expect(await screen.findByText('Review the change')).toBeTruthy();
    const detail = calls.find(
      (c) => c.method === 'GET' && DETAIL_PATH.test(c.path) && !SLING_PATH.test(c.path),
    );
    expect(detail).toBeTruthy();
  });
});
