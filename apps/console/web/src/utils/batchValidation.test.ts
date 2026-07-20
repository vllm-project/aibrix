import { describe, expect, it } from 'vitest';
import {
  getFileValidationTimeoutError,
  parseJsonl,
  parseJsonlFile,
  validateBatchFile,
  validateBatchLines,
} from './batchValidation';

function jsonlLine(url: string, model = 'qwen-serving', customId = 'req-1'): string {
  return JSON.stringify({
    custom_id: customId,
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

  it('validates a file through chunked parsing', async () => {
    const file = new File([
      [
        jsonlLine('/v1/chat/completions', 'qwen-serving', 'req-1'),
        jsonlLine('/v1/chat/completions', 'qwen-serving', 'req-2'),
      ].join('\n'),
    ], 'requests.jsonl', {
      type: 'application/jsonl',
    });

    const parsed = await parseJsonlFile(file, {
      retainFullRecords: true,
      readChunkSizeBytes: 7,
    });
    const result = validateBatchLines(parsed, {
      expectedModel: 'qwen-serving',
      supportedEndpoints: ['/v1/chat/completions'],
    });

    expect(result.valid).toBe(true);
    expect(result.totalLines).toBe(2);
    expect(result.errors).toEqual([]);
  });

  it('uses an explicit client-side timeout message', () => {
    expect(getFileValidationTimeoutError(60_000)).toBe(
      'Client-side validation timed out after 60s. ' +
      'The file is too large to validate in the browser; split it into smaller batches or select an already uploaded dataset.',
    );
  });
});
