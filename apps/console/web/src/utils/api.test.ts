import { afterEach, describe, expect, it, vi } from 'vitest';
import { camelToSnake, listAllJobs, normalizeFilesResponse } from './api';

afterEach(() => {
  vi.unstubAllGlobals();
});

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

  it('serializes template-driven deployment requests to the API contract', () => {
    expect(camelToSnake({
      name: 'test-deployment',
      template: {
        modelId: 'model-1',
        templateId: 'template-1',
      },
      implementation: {
        kind: 'kubernetes',
      },
      overrides: {
        minReplicas: 1,
        maxReplicas: 3,
        enableAutoScaling: true,
        engineArgs: {
          max_num_seqs: '64',
        },
      },
    })).toEqual({
      name: 'test-deployment',
      template: {
        model_id: 'model-1',
        template_id: 'template-1',
      },
      implementation: {
        kind: 'kubernetes',
      },
      overrides: {
        min_replicas: 1,
        max_replicas: 3,
        enable_auto_scaling: true,
        engine_args: {
          max_num_seqs: '64',
        },
      },
    });
  });

  it('serializes ModelAdapter creation without exposing the Kubernetes CRD', () => {
    expect(camelToSnake({
      name: 'sql-assistant',
      artifactUrl: 'huggingface://example/sql-assistant',
      deploymentName: 'qwen-serving',
      placement: 'all',
    })).toEqual({
      name: 'sql-assistant',
      artifact_url: 'huggingface://example/sql-assistant',
      deployment_name: 'qwen-serving',
      placement: 'all',
    });
  });

  it('serializes job resource duration to the API contract', () => {
    expect(camelToSnake({
      inputDataset: 'file-input',
      endpoint: '/v1/chat/completions',
      completionWindow: '6h',
      name: 'flex-window',
      resourceRequest: {
        replicas: 2,
        providerConfig: {
          duration: '1h',
        },
      },
    })).toEqual({
      input_dataset: 'file-input',
      endpoint: '/v1/chat/completions',
      completion_window: '6h',
      name: 'flex-window',
      resource_request: {
        replicas: 2,
        provider_config: {
          duration: '1h',
        },
      },
    });
  });

  it('serializes adaptive AIMD settings to the job client contract', () => {
    expect(camelToSnake({
      client: {
        adaptiveConcurrency: true,
        adaptiveMaxFactor: 16,
        adaptiveHealthyWindow: 8,
        adaptiveAdditiveIncrease: 2,
      },
    })).toEqual({
      client: {
        adaptive_concurrency: true,
        adaptive_max_factor: 16,
        adaptive_healthy_window: 8,
        adaptive_additive_increase: 2,
      },
    });
  });

  it('publishes the first jobs page while older pages are still loading', async () => {
    let resolveSecondPage!: (response: Response) => void;
    const secondPage = new Promise<Response>((resolve) => {
      resolveSecondPage = resolve;
    });
    const jsonResponse = (body: unknown) => ({
      ok: true,
      status: 200,
      json: async () => body,
    }) as Response;
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({
        jobs: [{ id: 'job-new' }],
        has_more: true,
      }))
      .mockReturnValueOnce(secondPage);
    vi.stubGlobal('fetch', fetchMock);

    const published: string[][] = [];
    const result = listAllJobs({
      pageLimit: 1,
      onPage: jobs => published.push(jobs.map(job => job.id)),
    });

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(published).toEqual([['job-new']]);
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      '/api/v1/jobs?after=job-new&limit=1',
      expect.any(Object),
    );

    resolveSecondPage(jsonResponse({
      jobs: [{ id: 'job-old' }],
      has_more: false,
    }));

    await expect(result).resolves.toEqual([
      expect.objectContaining({ id: 'job-new' }),
      expect.objectContaining({ id: 'job-old' }),
    ]);
    expect(published).toEqual([
      ['job-new'],
      ['job-new', 'job-old'],
    ]);
  });
});
