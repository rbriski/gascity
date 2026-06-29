import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import type { SupervisorFormulaStep } from '../../supervisor/formulaReads';
import { FormulaSteps } from './FormulaSteps';

afterEach(cleanup);

describe('FormulaSteps', () => {
  it('renders steps in order with kind, id, assignee, and labels', () => {
    const steps: SupervisorFormulaStep[] = [
      {
        id: 'review',
        kind: 'agent',
        title: 'Review the change',
        assignee: 'reviewer',
        labels: ['ci'],
      },
      { id: 'publish', kind: 'agent', title: 'Publish' },
    ];
    render(<FormulaSteps steps={steps} />);

    expect(screen.getByText('Review the change')).toBeTruthy();
    expect(screen.getByText('▸ reviewer')).toBeTruthy();
    expect(screen.getByText('ci')).toBeTruthy();
    expect(screen.getByText('Publish')).toBeTruthy();
    // ordered: two list items, numbered 1 and 2
    const items = screen.getAllByRole('listitem');
    expect(items).toHaveLength(2);
    expect(items[0]?.textContent).toContain('1');
    expect(items[1]?.textContent).toContain('2');
  });

  it('renders an empty message when there are no steps', () => {
    render(<FormulaSteps steps={[]} />);
    expect(screen.getByText('No steps in this formula.')).toBeTruthy();
  });

  it('renders cleanly when optional fields are missing', () => {
    render(<FormulaSteps steps={[{ id: 'only', kind: 'task', title: '' }]} />);
    // falls back to the id when title is empty; no crash on missing assignee/labels
    expect(screen.getByText('task')).toBeTruthy();
    expect(screen.getAllByText('only').length).toBeGreaterThanOrEqual(1);
  });
});
