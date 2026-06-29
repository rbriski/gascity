import { useState } from 'react';
import { Link } from 'react-router-dom';
import type { FormulaVarDefResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { getActiveCity } from '../../api/cityBase';
import { Button } from '../Button';
import { Field } from '../Field';
import {
  READ_ONLY_CONTROL_TITLE,
  ReadOnlyBadge,
  useReadOnly,
} from '../../contexts/ReadOnlyContext';
import { useCachedData } from '../../hooks/useCachedData';
import { listSupervisorAgents } from '../../supervisor/agentReads';
import { getSupervisorFormulaSteps } from '../../supervisor/formulaReads';
import { slingFormula } from '../../supervisor/formulaWrites';
import { FormulaSteps } from './FormulaSteps';

const CONTROL =
  'w-full rounded-sm border border-rule bg-transparent px-3 py-2 text-body text-fg focus-mark';

interface FormulaLauncherProps {
  name: string;
  varDefs: ReadonlyArray<FormulaVarDefResponse>;
  /** Called with the new run id after a successful launch (parent refreshes runs). */
  onLaunched?: (workflowId: string) => void;
}

// The inline launcher: pick a target, fill the vars, see the target-compiled
// steps preview, launch. NOT a modal (DESIGN.md: modals are last-resort). The
// Launch control is gated by the read-only posture (disabled + title + a shared
// ReadOnlyBadge, never hidden); the supervisor proxy's 405 is the real lock.
export function FormulaLauncher({ name, varDefs, onLaunched }: FormulaLauncherProps) {
  const cityName = getActiveCity();
  const readOnly = useReadOnly();
  const { data: agents } = useCachedData(`agents:${cityName ?? ''}`, () => listSupervisorAgents());
  const agentNames = (agents?.items ?? [])
    .map((a) => a.name)
    .filter((n): n is string => Boolean(n));

  const [target, setTarget] = useState('');
  const [vars, setVars] = useState<Record<string, string>>(() => initialVars(varDefs));
  const [launching, setLaunching] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [launchedId, setLaunchedId] = useState<string | null>(null);

  const trimmedTarget = target.trim();
  const missingRequired = varDefs.some((v) => v.required && (vars[v.name] ?? '').trim() === '');
  const canLaunch = !readOnly && trimmedTarget !== '' && !missingRequired && !launching;

  function setVar(varName: string, value: string): void {
    setVars((prev) => ({ ...prev, [varName]: value }));
    setLaunchedId(null);
  }

  async function onLaunch(): Promise<void> {
    // read-only is already folded into canLaunch; this guard makes the intent
    // explicit so a programmatic submit can't slip past the affordance.
    if (readOnly || !canLaunch) return;
    setLaunching(true);
    setError(null);
    try {
      const res = await slingFormula({ formula: name, target: trimmedTarget, vars });
      const workflowId = res.workflow_id ?? '';
      if (workflowId) {
        setLaunchedId(workflowId);
        onLaunched?.(workflowId);
      } else {
        setError('The run was accepted but returned no workflow id.');
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLaunching(false);
    }
  }

  return (
    <section aria-label="Run this formula" className="space-y-4">
      <h2 className="text-label uppercase tracking-wider text-fg-faint border-b border-rule pb-2">
        Run this formula
      </h2>

      <div className="max-w-xl space-y-4">
        <Field label="target" variant="form">
          <input
            list="formula-launcher-agents"
            value={target}
            onChange={(e) => {
              setTarget(e.target.value);
              setLaunchedId(null);
            }}
            placeholder="agent or pool"
            className={CONTROL}
          />
          <datalist id="formula-launcher-agents">
            {agentNames.map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
        </Field>

        {varDefs.map((v) => (
          <Field key={v.name} label={varFieldLabel(v)} variant="form">
            {v.enum && v.enum.length > 0 ? (
              <select
                className={CONTROL}
                value={vars[v.name] ?? ''}
                onChange={(e) => setVar(v.name, e.target.value)}
              >
                <option value="" />
                {v.enum.map((opt) => (
                  <option key={opt} value={opt}>
                    {opt}
                  </option>
                ))}
              </select>
            ) : (
              <input
                className={CONTROL}
                value={vars[v.name] ?? ''}
                onChange={(e) => setVar(v.name, e.target.value)}
              />
            )}
          </Field>
        ))}

        {trimmedTarget !== '' && <StepsPreview name={name} target={trimmedTarget} />}

        <div className="flex flex-wrap items-center gap-4 pt-1">
          <Button
            tone="accent"
            size="md"
            onClick={() => void onLaunch()}
            disabled={!canLaunch}
            title={readOnly ? READ_ONLY_CONTROL_TITLE : undefined}
          >
            {launching ? 'Launching' : 'Launch run'}
          </Button>
          {readOnly && <ReadOnlyBadge />}
          {launchedId && (
            <span className="text-body text-ok">
              Launched:{' '}
              <Link
                to={`/runs/${encodeURIComponent(launchedId)}`}
                className="font-mono text-accent focus-mark"
              >
                {launchedId}
              </Link>
            </span>
          )}
          {error && (
            <span className="text-body text-accent" role="alert">
              {error}
            </span>
          )}
        </div>
      </div>
    </section>
  );
}

function StepsPreview({ name, target }: { name: string; target: string }) {
  const cityName = getActiveCity();
  const { data: steps, error } = useCachedData(
    `formula:steps:${cityName ?? ''}:${name}:${target}`,
    () => getSupervisorFormulaSteps(name, target),
  );
  return (
    <div>
      <h3 className="text-label uppercase tracking-wider text-fg-faint mb-2">
        Compiled steps (target: {target})
      </h3>
      {steps === undefined ? (
        error !== null ? (
          <p className="text-body text-accent" role="alert">
            Could not compile preview: {error}
          </p>
        ) : (
          <p className="text-body text-fg-muted italic">Compiling preview.</p>
        )
      ) : (
        <FormulaSteps steps={steps} />
      )}
    </div>
  );
}

function initialVars(varDefs: ReadonlyArray<FormulaVarDefResponse>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const v of varDefs) {
    out[v.name] = v.default !== undefined && v.default !== null ? String(v.default) : '';
  }
  return out;
}

function varFieldLabel(v: FormulaVarDefResponse): string {
  return v.required ? `${v.name} (required)` : v.name;
}
