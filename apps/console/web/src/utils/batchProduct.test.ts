import { describe, expect, it } from 'vitest';
import {
  formatModelSelectionLabel,
  getBatchTiming,
  parseCompletionWindowSeconds,
} from './batchProduct';

describe('batch product helpers', () => {
  it('shows the serving model name beside the display name when they differ', () => {
    expect(formatModelSelectionLabel('Qwen3.6 32B', 'Qwen/Qwen3.5-32B')).toBe(
      'Qwen3.6 32B (Qwen/Qwen3.5-32B)',
    );
  });

  it('does not duplicate the model label when the serving name is empty or identical', () => {
    expect(formatModelSelectionLabel('Qwen/Qwen3.5-32B', 'Qwen/Qwen3.5-32B')).toBe(
      'Qwen/Qwen3.5-32B',
    );
    expect(formatModelSelectionLabel('Qwen3.6 32B', '')).toBe('Qwen3.6 32B');
  });

  it('parses the supported 24h completion window', () => {
    expect(parseCompletionWindowSeconds('1h')).toBe(3600);
    expect(parseCompletionWindowSeconds('2h')).toBe(7200);
    expect(parseCompletionWindowSeconds('6h')).toBe(21600);
    expect(parseCompletionWindowSeconds('12h')).toBe(43200);
    expect(parseCompletionWindowSeconds('24h')).toBe(86400);
    expect(parseCompletionWindowSeconds('48h')).toBeNull();
    expect(parseCompletionWindowSeconds('96h')).toBeNull();
    expect(parseCompletionWindowSeconds('3h')).toBeNull();
    expect(parseCompletionWindowSeconds('best_effort')).toBeNull();
  });

  it('derives active batch deadline and remaining seconds from completion window', () => {
    const timing = getBatchTiming(
      {
        status: 'in_progress',
        createdAt: 1700000000,
        expiresAt: 0,
        completionWindow: '24h',
      },
      1700003600,
    );

    expect(timing.deadlineAt).toBe(1700086400);
    expect(timing.remainingSeconds).toBe(82800);
    expect(timing.terminalAt).toBeNull();
    expect(timing.elapsedSeconds).toBe(3600);
  });

  it('uses terminal completion time when a batch is completed', () => {
    const timing = getBatchTiming(
      {
        status: 'completed',
        createdAt: 1700000000,
        expiresAt: 1700086400,
        completedAt: 1700000900,
        completionWindow: '24h',
      },
      1700003600,
    );

    expect(timing.terminalAt).toBe(1700000900);
    expect(timing.remainingSeconds).toBeNull();
    expect(timing.elapsedSeconds).toBe(900);
  });

  it('uses planner terminal time for pre-submit failures', () => {
    const timing = getBatchTiming(
      {
        status: 'resource_failed',
        createdAt: 1700000000,
        resourceFailedAt: 1700000300,
        completionWindow: '24h',
      },
      1700003600,
    );

    expect(timing.terminalAt).toBe(1700000300);
    expect(timing.remainingSeconds).toBeNull();
    expect(timing.elapsedSeconds).toBe(300);
  });
});
