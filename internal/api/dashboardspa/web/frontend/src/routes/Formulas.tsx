import { Link } from 'react-router-dom';
import { getActiveCity } from '../api/cityBase';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge } from '../components/StatusBadge';
import { Table, type TableColumn } from '../components/Table';
import { ReadOnlyBadge, useReadOnly } from '../contexts/ReadOnlyContext';
import { useNow } from '../contexts/NowContext';
import { useCachedData } from '../hooks/useCachedData';
import { formatRelative } from '../hooks/time';
import {
  type SupervisorFormula,
  listSupervisorFormulas,
  recentRunTone,
} from '../supervisor/formulaReads';

// /formulas — the formula catalog. A typeset list of the city's formula
// definitions: how many vars and runs each has, and its last run's status.
// This view is read-only by nature (it lists definitions); launching happens on
// the per-formula detail page (/formulas/:name), so the Run control here is a
// plain navigation link that stays live even in the dashboard's read-only mode.

function formulaDetailHref(name: string): string {
  return `/formulas/${encodeURIComponent(name)}`;
}

export function FormulasPage() {
  const cityName = getActiveCity();
  const now = useNow();
  const readOnly = useReadOnly();
  const { data, loading, error, refresh } = useCachedData(`formulas:list:${cityName ?? ''}`, () =>
    listSupervisorFormulas(),
  );
  const formulas = data ?? null;

  const columns: ReadonlyArray<TableColumn<SupervisorFormula>> = [
    {
      key: 'name',
      label: 'Formula',
      render: (f) => (
        <div className="min-w-0">
          <Link to={formulaDetailHref(f.name)} className="text-fg hover:text-accent focus-mark">
            {f.name}
          </Link>
          {f.description && (
            <div className="text-label text-fg-muted normal-case">{f.description}</div>
          )}
        </div>
      ),
    },
    {
      key: 'vars',
      label: 'Vars',
      align: 'right',
      sortable: true,
      sortValue: (f) => f.var_defs?.length ?? 0,
      render: (f) => <span className="tnum text-fg-muted">{f.var_defs?.length ?? 0}</span>,
    },
    {
      key: 'runs',
      label: 'Runs',
      align: 'right',
      sortable: true,
      sortValue: (f) => f.run_count,
      render: (f) => <span className="tnum text-fg-muted">{f.run_count}</span>,
    },
    {
      key: 'lastRun',
      label: 'Last run',
      render: (f) => {
        const last = f.recent_runs?.[0];
        if (!last) {
          return <StatusBadge tone="neutral" glyph="·" label="no runs" />;
        }
        return (
          <StatusBadge
            tone={recentRunTone(last.status)}
            label={last.status || 'run'}
            trailing={formatRelative(last.started_at, now)}
          />
        );
      },
    },
    {
      key: 'action',
      label: '',
      align: 'right',
      render: (f) => (
        <Link
          to={formulaDetailHref(f.name)}
          className="text-label uppercase tracking-wider text-fg-muted hover:text-accent focus-mark rounded-sm"
        >
          Run ▸
        </Link>
      ),
    },
  ];

  return (
    <section>
      <PageHeader
        title="Formulas"
        synopsis={synopsis(formulas, readOnly)}
        meta={
          <>
            {error !== null && formulas !== null && (
              <span className="normal-case text-body text-accent" role="alert">
                {error}
              </span>
            )}
            {readOnly && <ReadOnlyBadge />}
            <Button size="sm" onClick={() => void refresh()} disabled={loading}>
              {loading ? 'Refreshing' : 'Refresh'}
            </Button>
          </>
        }
      />

      {formulas === null ? (
        error !== null ? (
          <p className="text-body text-accent" role="alert">
            Could not load formulas: {error}
          </p>
        ) : (
          <p className="text-body text-fg-muted italic">Loading formulas.</p>
        )
      ) : (
        <Table
          columns={columns}
          rows={formulas}
          rowKey={(f) => f.name}
          empty="No formulas defined in this city yet."
          initialSort={{ key: 'runs', dir: 'desc' }}
        />
      )}
    </section>
  );
}

function synopsis(formulas: SupervisorFormula[] | null, readOnly: boolean): string {
  const lead =
    formulas === null
      ? 'Formula definitions in this city.'
      : `${formulas.length} formula${formulas.length === 1 ? '' : 's'} defined in this city.`;
  const base = `${lead} Launch one to start a run; follow it in Formula Runs.`;
  return readOnly ? `${base} Read-only mode: launching is disabled.` : base;
}
