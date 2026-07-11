import { describe, expect, it } from 'vitest';

import { agoStr, fmtCompact, fmtCost, fmtRate } from './format';

describe('fmtCompact', () => {
  it('keeps small numbers whole and compacts K/M/B with one decimal', () => {
    expect(fmtCompact(137)).toBe('137');
    expect(fmtCompact(1234)).toBe('1.2K');
    expect(fmtCompact(389_100)).toBe('389.1K');
    expect(fmtCompact(46_640_000)).toBe('46.6M');
    expect(fmtCompact(2_100_000_000)).toBe('2.1B');
  });
});

describe('fmtCost', () => {
  it('renders two-decimal USD', () => {
    expect(fmtCost(497.614)).toBe('$497.61');
    expect(fmtCost(0)).toBe('$0.00');
  });
});

describe('fmtRate', () => {
  it('renders a compact per-second rate', () => {
    expect(fmtRate(33_200)).toBe('33.2K/s');
  });
});

describe('agoStr', () => {
  it('steps now → seconds → minutes → hours → days', () => {
    expect(agoStr(500)).toBe('now');
    expect(agoStr(5_000)).toBe('5s');
    expect(agoStr(3 * 60_000)).toBe('3m');
    expect(agoStr(2 * 3_600_000)).toBe('2h');
    expect(agoStr(30 * 3_600_000)).toBe('1d');
  });
});
