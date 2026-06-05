import { describe, expect, it } from 'vitest';
import { parseJsonl, validateBatchFile, validateBatchLines } from './batchValidation';

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

  it('rejects a json file even when the content is valid jsonl', async () => {
    const file = new File([jsonlLine('/v1/chat/completions')], 'requests.json', {
      type: 'application/json',
    });

    const result = await validateBatchFile(file, {
      expectedModel: 'qwen-serving',
      supportedEndpoints: ['/v1/chat/completions'],
    });

    expect(result.valid).toBe(false);
    expect(result.errors).toContain('File must use the .jsonl extension.');
  });
});
