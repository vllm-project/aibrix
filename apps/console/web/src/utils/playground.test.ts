import { describe, expect, it, vi } from 'vitest';
import type { Deployment } from '../data/mockData';
import type { ModelClaim } from './api';
import {
  callableDeployments,
  deploymentTargets,
  maxTokensFor,
  modelClaimTargets,
  playgroundErrorMessage,
  preferredTarget,
  streamPlaygroundChat,
} from './playground';

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
    const items = [
      deployment({ id: 'ready' }),
      deployment({ id: 'starting', status: 'Deploying' }),
      deployment({ id: 'unnamed', servingName: '' }),
    ];
    const callable = callableDeployments(items);
    expect(callable.map((item) => item.id)).toEqual(['ready']);
    const targets = deploymentTargets(items);
    expect(preferredTarget(targets, { deployment: 'ready' })?.id).toBe('ready');
    expect(preferredTarget(targets, { deployment: '/models/mock' })?.id).toBe('ready');
    expect(targets[0]?.routingStrategy).toBeUndefined();
  });

  it('lists Active and Sleeping ModelClaims with a routing strategy', () => {
    const claim = (overrides: Partial<ModelClaim>) => ({
      name: 'qwen3-0-6b',
      modelName: 'qwen3-0.6b',
      phase: 'Active',
      deleting: false,
      ...overrides,
    }) as ModelClaim;
    const targets = modelClaimTargets([
      claim({}),
      claim({ name: 'asleep', modelName: 'asleep-model', phase: 'Sleeping' }),
      claim({ name: 'pending', modelName: 'pending-model', phase: 'Pending' }),
      claim({ name: 'going', modelName: 'going-model', deleting: true }),
    ]);
    expect(targets.map((target) => target.id)).toEqual(['qwen3-0-6b', 'asleep']);
    expect(targets[0]).toMatchObject({ kind: 'modelclaim', servingName: 'qwen3-0.6b', routingStrategy: 'random' });
    expect(targets[0]?.note).toBeUndefined();
    expect(targets[1]?.note).toContain('wakes');

    const all = [...deploymentTargets([deployment({ id: 'ready' })]), ...targets];
    expect(preferredTarget(all, { modelClaim: 'asleep' })?.key).toBe('modelclaim:asleep');
    expect(preferredTarget(all, { modelClaim: 'missing' })?.key).toBe('deployment:ready');
    expect(preferredTarget(all)?.key).toBe('deployment:ready');
  });

  it('keeps max tokens within a ModelClaim engine context', () => {
    const [withLen, withoutLen] = modelClaimTargets([
      { name: 'a', modelName: 'a', phase: 'Active', deleting: false, engineArgs: { '--max-model-len': '2048' } },
      { name: 'b', modelName: 'b', phase: 'Active', deleting: false, engineArgs: {} },
    ] as unknown as ModelClaim[]);
    expect(withLen?.maxModelLen).toBe(2048);
    expect(withoutLen?.maxModelLen).toBeUndefined();
    const [padded, numeric, unparsable] = modelClaimTargets([
      { name: 'c', modelName: 'c', phase: 'Active', deleting: false, engineArgs: { '--max-model-len': ' 4096 ' } },
      { name: 'd', modelName: 'd', phase: 'Active', deleting: false, engineArgs: { '--max_model_len': 1024 } },
      { name: 'e', modelName: 'e', phase: 'Active', deleting: false, engineArgs: { '--max-model-len': 'auto' } },
    ] as unknown as ModelClaim[]);
    expect(padded?.maxModelLen).toBe(4096);
    expect(numeric?.maxModelLen).toBe(1024);
    expect(unparsable?.maxModelLen).toBeUndefined();
    expect(maxTokensFor(withLen ?? null, 4096)).toBe(1024);
    expect(maxTokensFor(withLen ?? null, 256)).toBe(256);
    expect(maxTokensFor(withoutLen ?? null, 4096)).toBe(4096);
    expect(maxTokensFor(null, 4096)).toBe(4096);

    const [sglang, sglangUnderscore] = modelClaimTargets([
      { name: 'f', modelName: 'f', phase: 'Active', deleting: false, engineArgs: { '--context-length': '2048' } },
      { name: 'g', modelName: 'g', phase: 'Active', deleting: false, engineArgs: { '--context_length': '3072' } },
    ] as unknown as ModelClaim[]);
    expect(sglang?.maxModelLen).toBe(2048);
    expect(maxTokensFor(sglang ?? null, 4096)).toBe(1024);
    expect(sglangUnderscore?.maxModelLen).toBe(3072);
    expect(maxTokensFor(sglangUnderscore ?? null, 4096)).toBe(1536);
  });

  it('sends the routing strategy and waits out a waking model', async () => {
    const encoder = new TextEncoder();
    const fetcher = vi.fn()
      .mockResolvedValueOnce(new Response(
        '{"error":{"message":"model qwen3-0.6b is sleeping; retry shortly"}}',
        { status: 503, headers: { 'retry-after': '3' } },
      ))
      .mockResolvedValueOnce(new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode('data: {"choices":[{"delta":{"content":"Paris"}}]}\n\n'));
          controller.close();
        },
      }), { status: 200 }));
    const sleep = vi.fn(async () => {});
    const waits: Array<[string, number]> = [];

    const content = await streamPlaygroundChat({
      model: 'qwen3-0.6b',
      messages: [{ role: 'user', content: 'Capital of France?' }],
    }, () => {}, fetcher, {
      routingStrategy: 'random',
      sleep,
      onWaiting: (message, seconds) => waits.push([message, seconds]),
    });

    expect(content).toBe('Paris');
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(fetcher.mock.calls[0]?.[1]?.headers).toMatchObject({ 'routing-strategy': 'random' });
    expect(sleep).toHaveBeenCalledWith(3000);
    expect(waits).toEqual([['model qwen3-0.6b is sleeping; retry shortly', 3]]);
  });

  it('stops waiting once Retry-After would pass the wait limit', async () => {
    const fetcher = vi.fn(async () => new Response(
      '{"error":{"message":"model qwen3-0.6b is sleeping; retry shortly"}}',
      { status: 503, headers: { 'retry-after': '30' } },
    ));
    await expect(streamPlaygroundChat({
      model: 'qwen3-0.6b',
      messages: [{ role: 'user', content: 'Hi' }],
    }, () => {}, fetcher, { maxWaitMs: 10_000, sleep: async () => {} })).rejects.toThrow('is sleeping; retry shortly');
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it('reads gateway and console error messages', () => {
    expect(playgroundErrorMessage('{"error":{"message":"model x does not exist"}}', 400)).toBe('model x does not exist');
    expect(playgroundErrorMessage('{"error":"invalid request body"}', 400)).toBe('invalid request body');
    expect(playgroundErrorMessage('upstream broke', 502)).toBe('upstream broke');
    expect(playgroundErrorMessage('{"error":{"message":{"detail":"x"}}}', 500)).toBe('{"error":{"message":{"detail":"x"}}}');
    expect(playgroundErrorMessage('null', 500)).toBe('null');
    expect(playgroundErrorMessage('', 500)).toBe('Playground request failed with status 500');
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
