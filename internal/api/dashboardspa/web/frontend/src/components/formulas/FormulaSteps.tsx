import type { SupervisorFormulaStep } from '../../supervisor/formulaReads';

// The compiled step graph, as a typeset ordered list (not a diagram — the run
// diagram lives in FormulaRunDetail). Pure: the launcher fetches the
// target-bound preview and hands the steps in.

export function FormulaSteps({ steps }: { steps: ReadonlyArray<SupervisorFormulaStep> }) {
  if (steps.length === 0) {
    return <p className="text-body text-fg-muted italic">No steps in this formula.</p>;
  }
  return (
    <ol className="m-0 p-0">
      {steps.map((step, index) => (
        <li key={step.id} className="flex gap-3 py-2 border-b border-rule last:border-b-0">
          <span className="text-label text-fg-faint tnum w-5 shrink-0 pt-0.5">{index + 1}</span>
          <div className="min-w-0">
            <div className="text-body text-fg">{step.title || step.id}</div>
            <div className="flex flex-wrap gap-x-3 font-mono text-label normal-case text-fg-faint">
              <span>{step.kind}</span>
              <span>{step.id}</span>
              {step.assignee && <span className="text-accent">▸ {step.assignee}</span>}
              {step.labels && step.labels.length > 0 && <span>{step.labels.join(' ')}</span>}
            </div>
          </div>
        </li>
      ))}
    </ol>
  );
}
