import { describe, expect, it } from 'vitest';
import { normalizeFilesResponse } from './api';

describe('api helpers', () => {
  it('normalizes OpenAI file list responses for the batch file picker', () => {
    const files = normalizeFilesResponse({
      object: 'list',
      data: [
        {
          id: 'file-1',
          filename: 'requests.jsonl',
          bytes: 1536,
          created_at: 1737230220,
          purpose: 'batch',
        },
      ],
      hasMore: false,
    });

    expect(files).toEqual([
      {
        id: 'file-1',
        name: 'requests.jsonl',
        purpose: 'batch',
        size: 1536,
        createdAt: 1737230220,
      },
    ]);
  });

  it('normalizes raw array file list responses for the batch file picker', () => {
    const files = normalizeFilesResponse([
      {
        id: 'file-2',
        filename: 'uploaded.jsonl',
        bytes: 2048,
        created_at: 1737230221,
        purpose: 'batch',
      },
    ]);

    expect(files).toEqual([
      {
        id: 'file-2',
        name: 'uploaded.jsonl',
        purpose: 'batch',
        size: 2048,
        createdAt: 1737230221,
      },
    ]);
  });
});
