import { describe, expect, it } from 'vitest';
import type { ModelClaim, ModelClaimPool } from './api';
import {
  buildModelClaimYaml,
  engineFlag,
  formatBytes,
  modelClaimAttention,
  parseQuantity,
  validateArtifactUrl,
  validateEngineArgs,
  validateModelClaimName,
  validatePerGpuFigure,
  validatePriority,
} from './modelClaim';

const pool: ModelClaimPool = {
  name: 'b300-pool-a',
  namespace: 'default',
  workloads: ['warm-runtime-pool-b300'],
  pods: 1,
  enabledPods: 1,
  readyPods: 1,
  gpuCounts: [1],
  images: ['aibrix/kvcached-runtime:dev'],
  claims: 0,
};

describe('model claim helpers', () => {
  it('reads Kubernetes quantities as bytes', () => {
    expect(parseQuantity('6Gi')).toBe(6 * 2 ** 30);
    expect(parseQuantity('1.5Mi')).toBe(1.5 * 2 ** 20);
    expect(parseQuantity('2G')).toBe(2e9);
    expect(parseQuantity('1e3')).toBe(1000);
    expect(parseQuantity('30m')).toBeCloseTo(0.03);
    expect(parseQuantity('6 GB')).toBeNull();
    expect(parseQuantity('Gi')).toBeNull();
  });

  it('refuses the perGPU figures the controller refuses', () => {
    expect(validatePerGpuFigure('30Gi')).toBeNull();
    expect(validatePerGpuFigure('')).toMatch(/Required/);
    expect(validatePerGpuFigure('6 GB')).toMatch(/quantity/);
    expect(validatePerGpuFigure('0')).toMatch(/positive/);
    expect(validatePerGpuFigure('2Pi')).toMatch(/1Pi/);
    expect(validatePerGpuFigure('30m')).toMatch(/whole number of bytes/);
  });

  it('accepts only the artifact schemes the runtime downloads', () => {
    expect(validateArtifactUrl('huggingface://Qwen/Qwen3-0.6B')).toBeNull();
    expect(validateArtifactUrl('/models/qwen3')).toBeNull();
    expect(validateArtifactUrl('')).toMatch(/Enter/);
    expect(validateArtifactUrl('hdfs://models/qwen3')).toMatch(/huggingface/);
  });

  it('accepts names that fit a Pod annotation key', () => {
    expect(validateModelClaimName('qwen3-0-6b')).toBeNull();
    expect(validateModelClaimName('qwen3.0-6b')).toBeNull();
    expect(validateModelClaimName('Qwen3')).not.toBeNull();
    expect(validateModelClaimName('-qwen')).not.toBeNull();
    expect(validateModelClaimName('a'.repeat(64))).toMatch(/63/);
  });

  it('checks engine flags and the pool topology', () => {
    const ok = validateEngineArgs(
      [{ key: ' --max-model-len ', value: ' 2048 ' }, { key: '', value: '' }],
      'vllm',
      pool,
    );
    expect(ok.rowErrors).toEqual([null, null]);
    expect(ok.topologyError).toBeNull();
    expect(ok.parallelism).toBe(1);
    expect(ok.args).toEqual({ '--max-model-len': '2048' });

    const rows = validateEngineArgs(
      [
        { key: 'max-model-len', value: '2048' },
        { key: '--port', value: '9000' },
        { key: '--gpu-memory-utilization', value: '0.9' },
        { key: '--enforce-eager', value: '' },
        { key: '--enforce-eager', value: '' },
      ],
      'vllm',
      pool,
    );
    expect(rows.rowErrors[0]).toMatch(/engine flag/);
    expect(rows.rowErrors[1]).toMatch(/runtime sets --port/);
    expect(rows.rowErrors[2]).toMatch(/kvcached/);
    expect(rows.rowErrors[3]).toBeNull();
    expect(rows.rowErrors[4]).toMatch(/more than once/);

    const mismatch = validateEngineArgs([{ key: '--tensor-parallel-size', value: '2' }], 'vllm', pool);
    expect(mismatch.topologyError).toMatch(/TP × PP is 2, but Pods in pool b300-pool-a have 1 GPU\./);

    const dataParallel = validateEngineArgs([{ key: '--data-parallel-size', value: '2' }], 'vllm', pool);
    expect(dataParallel.topologyError).toMatch(/Data parallelism/);

    const unknownPool = validateEngineArgs([{ key: '--tensor-parallel-size', value: '2' }], 'vllm', {
      ...pool,
      gpuCounts: [1, 2],
    });
    expect(unknownPool.topologyError).toBeNull();

    const sglang = validateEngineArgs([{ key: '--tensor-parallel-size', value: '2' }], 'sglang', pool);
    expect(sglang.topologyError).toBeNull();
    expect(sglang.parallelism).toBeNull();
  });

  it('reads engine flags the way vLLM does', () => {
    expect(engineFlag(' --max_model_len ')).toEqual({ flag: '--max-model-len', value: null });
    expect(engineFlag('--port=9000')).toEqual({ flag: '--port', value: '9000' });
    expect(engineFlag('--enforce-eager=')).toEqual({ flag: '--enforce-eager', value: '' });
    expect(engineFlag('-tp')).toEqual({ flag: '--tensor-parallel-size', value: null });
    expect(engineFlag('-q')).toEqual({ flag: '-q', value: null });

    const rows = validateEngineArgs(
      [
        { key: '--port=9000', value: '' },
        { key: '--served_model_name', value: 'x' },
        { key: '--no-enable-sleep-mode', value: '' },
        { key: '--max-num-seqs=8', value: '16' },
        { key: '--max-model-len', value: '2048' },
        { key: '--max_model_len', value: '4096' },
        { key: '--gpu_memory_utilization', value: '0.9' },
      ],
      'vllm',
      pool,
    );
    expect(rows.rowErrors).toEqual([
      'The runtime sets --port for every engine.',
      'The runtime sets --served-model-name for every engine.',
      'The runtime sets --enable-sleep-mode for every engine.',
      '--max-num-seqs=8 already gives a value after "="; leave the value empty.',
      null,
      '--max-model-len is set more than once.',
      'kvcached owns KV memory; remove --gpu-memory-utilization.',
    ]);

    for (const row of [
      { key: '--tensor-parallel-size=2', value: '' },
      { key: '--tensor_parallel_size', value: '2' },
    ]) {
      expect(validateEngineArgs([row], 'vllm', pool).topologyError).toMatch(/TP × PP is 2/);
    }
  });

  it('renders the submitted claim as YAML', () => {
    expect(buildModelClaimYaml({
      name: 'qwen3-0-6b',
      modelName: 'qwen3-0.6b',
      artifactUrl: 'huggingface://Qwen/Qwen3-0.6B',
      engine: 'vllm',
      poolName: 'b300-pool-a',
      maximumFootprint: '6Gi',
      kvFloor: '1Gi',
      priority: '10',
      args: { '--max-model-len': '2048' },
    }, 'default')).toBe(`apiVersion: model.aibrix.ai/v1alpha1
kind: ModelClaim
metadata:
  name: "qwen3-0-6b"
  namespace: "default"
  labels:
    app.kubernetes.io/managed-by: aibrix-console
spec:
  modelName: "qwen3-0.6b"
  podSelector:
    matchLabels:
      claim.model.aibrix.ai/pool: "b300-pool-a"
  artifactURL: "huggingface://Qwen/Qwen3-0.6B"
  engine: vllm
  perGPU:
    maximumFootprint: "6Gi"
    kvFloor: "1Gi"
  priority: 10
  engineConfig:
    args:
      "--max-model-len": "2048"
`);
  });

  it('leaves priority out of the YAML at its default', () => {
    const yaml = buildModelClaimYaml({
      name: 'a',
      modelName: '',
      artifactUrl: 'hf://a',
      engine: 'vllm',
      poolName: 'p',
      maximumFootprint: '6Gi',
      kvFloor: '1Gi',
      priority: '0',
      args: {},
    });
    expect(yaml).not.toContain('priority');
  });

  it('accepts a priority of zero or more that fits an int32', () => {
    expect(validatePriority('')).toBeNull();
    expect(validatePriority('0')).toBeNull();
    expect(validatePriority(' 100 ')).toBeNull();
    expect(validatePriority('2147483647')).toBeNull();
    expect(validatePriority('-1')).toContain('whole number');
    expect(validatePriority('1.5')).toContain('whole number');
    expect(validatePriority('2147483648')).toContain('no more than');
  });

  it('explains a claim that is not Active by its first failing condition', () => {
    const claim = {
      phase: 'Pending',
      conditions: [
        { type: 'Ready', status: 'False', reason: 'NotPlaced', message: '', lastTransitionTime: '' },
        { type: 'Scheduled', status: 'False', reason: 'InvalidPerGPU', message: 'spec.perGPU is missing', lastTransitionTime: '' },
        { type: 'Initialized', status: 'True', reason: 'ModelClaimInitialized', message: '', lastTransitionTime: '' },
      ],
    } as unknown as ModelClaim;
    expect(modelClaimAttention(claim)?.reason).toBe('InvalidPerGPU');
    expect(modelClaimAttention({ ...claim, phase: 'Active' })).toBeNull();
  });

  it('formats byte counts', () => {
    expect(formatBytes(2 ** 30)).toBe('1.0 GiB');
    expect(formatBytes(12 * 2 ** 30)).toBe('12 GiB');
    expect(formatBytes(0)).toBe('-');
  });
});
