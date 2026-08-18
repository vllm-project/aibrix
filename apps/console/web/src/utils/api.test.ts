import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  camelToSnake,
  listAllJobs,
  listCatalogRegionsForAccelerators,
  normalizeFilesResponse,
} from './api';
import {
  defaultTemplateRegions,
  mergeRegionOptions,
  toggleTemplateRegion,
} from '../components/CreateModelDeploymentTemplate';

afterEach(() => {
  vi.restoreAllMocks();
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

  it('serializes regions selected in the deployment template form', () => {
    const spec = toggleTemplateRegion({
      accelerator: {
        type: 'NVIDIA-L20',
        count: 1,
      },
      regions: ['USEAST1'],
    }, 'USWEST2');

    expect(camelToSnake({
      name: 'l20-template',
      modelId: 'model-1',
      spec,
    })).toEqual({
      name: 'l20-template',
      model_id: 'model-1',
      spec: {
        accelerator: {
          type: 'NVIDIA-L20',
          count: 1,
        },
        regions: [
          'USEAST1',
          'USWEST2',
        ],
      },
    });
  });

  it('keeps stored regions visible when the catalog is empty or changes', () => {
    expect(mergeRegionOptions(
      ['USWEST2'],
      [],
    )).toEqual(['USWEST2']);

    expect(mergeRegionOptions(
      ['USWEST2'],
      [
        'USEAST1',
        'USWEST2',
      ],
    )).toEqual([
      'USEAST1',
      'USWEST2',
    ]);
  });

  it('selects all catalog regions only when the template has no region state', () => {
    expect(defaultTemplateRegions(
      undefined,
      ['USEAST1', 'USWEST2'],
    )).toEqual([
      'USEAST1',
      'USWEST2',
    ]);
    expect(defaultTemplateRegions(
      ['USWEST2'],
      ['USEAST1', 'USWEST2'],
    )).toEqual(['USWEST2']);
    expect(defaultTemplateRegions(
      [],
      ['USEAST1', 'USWEST2'],
    )).toEqual([]);
  });

  it('refreshes the complete accelerator region snapshot after cache expiry', async () => {
    const now = vi.spyOn(Date, 'now').mockReturnValue(1_000);
    const accelerators = ['NVIDIA-L20', 'NVIDIA-A30'];
    const fetcher = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        regions: {
          'NVIDIA-L20': ['USWEST2'],
          'NVIDIA-A30': ['USEAST1'],
        },
      }),
    });
    vi.stubGlobal('fetch', fetcher);

    await expect(
      listCatalogRegionsForAccelerators(accelerators),
    ).resolves.toEqual({
      'NVIDIA-L20': ['USWEST2'],
      'NVIDIA-A30': ['USEAST1'],
    });
    expect(fetcher).toHaveBeenCalledWith(
      '/api/v1/catalog/regions',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ accelerators }),
        credentials: 'include',
      }),
    );

    await listCatalogRegionsForAccelerators(accelerators);
    expect(fetcher).toHaveBeenCalledTimes(1);

    now.mockReturnValue(1_000 + 2 * 60 * 60 * 1000 + 1);
    await listCatalogRegionsForAccelerators(accelerators);
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(fetcher).toHaveBeenLastCalledWith(
      '/api/v1/catalog/regions',
      expect.objectContaining({
        body: JSON.stringify({ accelerators }),
      }),
    );
  });

  it('preloads all accelerator regions with one request', async () => {
    vi.spyOn(Date, 'now').mockReturnValue(2_000);
    const fetcher = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        regions: {
          'NVIDIA-A30': ['USEAST1'],
          'NVIDIA-L40S': ['USWEST2'],
        },
      }),
    });
    vi.stubGlobal('fetch', fetcher);

    const regions = await listCatalogRegionsForAccelerators([
      'NVIDIA-A30',
      'NVIDIA-L40S',
      'CPU',
    ]);

    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(fetcher).toHaveBeenCalledWith(
      '/api/v1/catalog/regions',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({
          accelerators: ['NVIDIA-A30', 'NVIDIA-L40S'],
        }),
      }),
    );
    expect(regions).toEqual({
      'NVIDIA-A30': ['USEAST1'],
      'NVIDIA-L40S': ['USWEST2'],
    });
  });

  it('does not cache failed catalog region requests', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce({
        ok: false,
        status: 502,
        text: async () => 'catalog unavailable',
      })
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          regions: {
            'NVIDIA A100': [],
          },
        }),
      });
    vi.stubGlobal('fetch', fetcher);

    await expect(
      listCatalogRegionsForAccelerators(['NVIDIA A100', 'NVIDIA L20']),
    ).rejects.toThrow();
    await expect(
      listCatalogRegionsForAccelerators(['NVIDIA A100', 'NVIDIA L20']),
    ).resolves.toEqual({
      'NVIDIA A100': [],
      'NVIDIA L20': [],
    });
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
});
