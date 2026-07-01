import { Link, useParams } from 'react-router-dom';
import { getActiveCity } from '../api/cityBase';
import { Field } from '../components/Field';
import { FormulaLauncher } from '../components/formulas/FormulaLauncher';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge } from '../components/StatusBadge';
import { useNow } from '../contexts/NowContext';
import { useCachedData } from '../hooks/useCachedData';
import { formatRelative } from '../hooks/time';
import {
  type SupervisorFormula,
  type SupervisorFormulaVarDef,
  cityScope,
  getSupervisorFormulaRuns,
  listSupervisorFormulas,
  recentRunTone,
} from '../supervisor/formulaReads';

// /formulas/:name — a single formula. The catalog summary (name, description,
// version, var defs) needs no target, so it comes from the shared formulas list
// (same cache key as the catalog). Recent runs deep-link into the existing
// FormulaRunDetail rather than redrawing run state. The compiled STEP graph is
// target-bound (the supervisor only compiles a preview once a target is chosen),
// so steps render inside the launcher (slice 4), not here.

function backLink() {
  return (
    <Link
      to="/formulas"
      className="text-label uppercase tracking-wider text-fg-muted hover:text-accent focus-mark rounded-sm"
    >
      ‹ Formulas
    </Link>
  );
}

export function FormulaDetailPage() {
  const { name = '' } = useParams<{ name: string }>();
  const cityName = getActiveCity();
  const now = useNow();

  const { data: formulas } = useCachedData(`formulas:list:${cityName ?? ''}`, () =>
    listSupervisorFormulas(cityScope(cityName)),
  );
  const {
    data: runs,
    error: runsError,
    refresh: refreshRuns,
  } = useCachedData(`formulas:runs:${cityName ?? ''}:${name}`, () =>
    getSupervisorFormulaRuns(name, cityScope(cityName)),
  );

  const formula = formulas?.find((f) => f.name === name) ?? null;

  // The list resolved but this name is not in it: an honest not-found, distinct
  // from "still loading".
  if (formulas !== undefined && formula === null) {
    return (
      <section>
        <PageHeader
          title={name}
          synopsis="This formula is not in the city's catalog."
          meta={backLink()}
        />
      </section>
    );
  }

  return (
    <section>
      <PageHeader
        title={name}
        synopsis={formula ? detailSynopsis(formula) : 'Loading formula.'}
        meta={backLink()}
      />

      <div className="grid gap-10 md:grid-cols-[minmax(0,1fr)_18rem]">
        <div className="min-w-0 space-y-10">
          <section>
            <SectionLabel>Variables</SectionLabel>
            {formula && formula.var_defs && formula.var_defs.length > 0 ? (
              <dl className="space-y-4">
                {formula.var_defs.map((v) => (
                  <Field key={v.name} label={v.name} variant="definition">
                    <span className="font-mono text-label normal-case text-fg-muted">
                      {varTypeLine(v)}
                    </span>
                    {v.description && <div className="text-fg-muted">{v.description}</div>}
                  </Field>
                ))}
              </dl>
            ) : (
              <p className="text-body text-fg-muted italic">This formula takes no variables.</p>
            )}
          </section>

          <div data-testid="formula-launcher-slot">
            {formula && (
              <FormulaLauncher
                name={name}
                varDefs={formula.var_defs ?? []}
                onLaunched={() => void refreshRuns()}
              />
            )}
          </div>
        </div>

        <aside>
          <SectionLabel>Recent runs</SectionLabel>
          {runs === undefined ? (
            runsError !== null ? (
              <p className="text-body text-accent" role="alert">
                Could not load runs: {runsError}
              </p>
            ) : (
              <p className="text-body text-fg-muted italic">Loading runs.</p>
            )
          ) : runs.length === 0 ? (
            <p className="text-body text-fg-muted italic">No runs yet.</p>
          ) : (
            <ul className="space-y-1">
              {runs.map((r) => (
                <li key={r.workflow_id}>
                  <Link
                    to={`/runs/${encodeURIComponent(r.workflow_id)}`}
                    className="flex items-baseline gap-3 py-2 border-b border-rule hover:bg-surface-tint focus-mark rounded-sm"
                  >
                    <StatusBadge tone={recentRunTone(r.status)} label={r.status || 'run'} />
                    <span className="font-mono text-label text-fg-muted truncate">
                      {r.workflow_id}
                    </span>
                    <span className="ml-auto text-label uppercase tracking-wider text-fg-faint tnum">
                      {formatRelative(r.started_at, now)}
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </aside>
      </div>
    </section>
  );
}

function SectionLabel({ children }: { children: string }) {
  return (
    <h2 className="text-label uppercase tracking-wider text-fg-faint border-b border-rule pb-2 mb-4">
      {children}
    </h2>
  );
}

function detailSynopsis(formula: SupervisorFormula): string {
  const lead = `Version ${formula.version}.`;
  return formula.description ? `${lead} ${formula.description}` : lead;
}

function varTypeLine(v: SupervisorFormulaVarDef): string {
  const parts = [v.type];
  if (v.required) parts.push('required');
  if (v.default !== undefined && v.default !== null)
    parts.push(`default ${formatDefault(v.default)}`);
  if (v.enum && v.enum.length > 0) parts.push(`one of ${v.enum.join(', ')}`);
  return parts.join(' · ');
}

function formatDefault(value: unknown): string {
  if (typeof value === 'string') return `"${value}"`;
  return String(value);
}
