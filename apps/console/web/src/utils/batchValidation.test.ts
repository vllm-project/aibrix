import { describe, expect, it } from 'vitest';
import { parseJsonl, validateBatchLines } from './batchValidation';

function jsonlLine(url: string, model = 'qwen-serving'): string {
  return JSON.stringify({
    custom_id: 'req-1',
    method: 'POST',
    url,
    body: {
      model,
      input: 'hello',
    },
  });
}

describe('batch validation', () => {
  it('accepts a single endpoint when it is supported by the selected template', () => {
    const parsed = parseJsonl(jsonlLine('/v1/embeddings'));

    const result = validateBatchLines(parsed, {
      expectedModel: 'qwen-serving',
      supportedEndpoints: ['/v1/chat/completions', '/v1/embeddings'],
    });

    expect(result.valid).toBe(true);
    expect(result.errors).toEqual([]);
    expect(result.endpoints).toEqual(['/v1/embeddings']);
  });
});
