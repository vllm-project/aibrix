import { describe, expect, it, vi } from 'vitest';
import type { Deployment } from '../data/mockData';
import { callableDeployments, streamPlaygroundChat } from './playground';

function deployment(overrides: Partial<Deployment>): Deployment {
  return {
    id: 'deployment-1',
    name: 'demo',
    deploymentId: 'runtime-demo',
    baseModel: 'Mock Model',
    baseModelId: 'model-1',
    servingName: '/models/mock',
    createdAt: '2026-07-26T08:30:00Z',
    replicas: '1',
    gpusPerReplica: 0,
    gpuType: 'CPU',
    region: '',
    createdBy: 'owner@example.com',
    status: 'Ready',
    ...overrides,
  };
}

describe('Playground helpers', () => {
  it('lists only ready deployments with a serving name', () => {
    expect(callableDeployments([
      deployment({ id: 'ready' }),
      deployment({ id: 'starting', status: 'Deploying' }),
      deployment({ id: 'unnamed', servingName: '' }),
    ]).map((item) => item.id)).toEqual(['ready']);
  });

  it('streams fragmented OpenAI chat completion events', async () => {
    const encoder = new TextEncoder();
    const response = new Response(new ReadableStream({
      start(controller) {
        controller.enqueue(encoder.encode('data: {"choices":[{"delta":{"content":"Hel'));
        controller.enqueue(encoder.encode('lo"}}]}\n\ndata: [DONE]\n\n'));
        controller.close();
      },
    }), { status: 200, headers: { 'content-type': 'text/event-stream' } });
    const fetcher = vi.fn(async () => response);
    const deltas: string[] = [];

    const content = await streamPlaygroundChat({
      model: '/models/mock',
      messages: [{ role: 'user', content: 'Hi' }],
    }, (delta) => deltas.push(delta), fetcher);

    expect(content).toBe('Hello');
    expect(deltas).toEqual(['Hello']);
    expect(fetcher).toHaveBeenCalledWith('/api/v1/playground/chat/completions', expect.objectContaining({
      method: 'POST',
    }));
  });

  it('skips malformed SSE data and continues streaming later events', async () => {
    const encoder = new TextEncoder();
    const response = new Response(new ReadableStream({
      start(controller) {
        controller.enqueue(encoder.encode('data: not-json\n\n'));
        controller.enqueue(encoder.encode('data: {"choices":[{"delta":{"content":"Hello"}}]}\n\n'));
        controller.close();
      },
    }), { status: 200, headers: { 'content-type': 'text/event-stream' } });
    const deltas: string[] = [];

    const content = await streamPlaygroundChat({
      model: '/models/mock',
      messages: [{ role: 'user', content: 'Hi' }],
    }, (delta) => deltas.push(delta), async () => response);

    expect(content).toBe('Hello');
    expect(deltas).toEqual(['Hello']);
  });

  it('surfaces an upstream error instead of returning an empty answer', async () => {
    const fetcher = vi.fn(async () => new Response('model unavailable', { status: 503 }));
    await expect(streamPlaygroundChat({
      model: '/models/mock',
      messages: [{ role: 'user', content: 'Hi' }],
    }, () => {}, fetcher)).rejects.toThrow('model unavailable');
  });
});
