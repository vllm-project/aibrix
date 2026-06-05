import { describe, expect, it } from 'vitest';
import { formatModelSelectionLabel } from './batchProduct';

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
});
