import type { ModelClaim, ModelClaimEngine, ModelClaimPool } from './api';

// Client-side checks mirror apps/console/api/handler/modelclaim.go so a form
// can point at the field to fix. The backend stays authoritative.

export const MODEL_CLAIM_REFRESH_INTERVAL_MS = 5000;

// The controller keys a routing annotation on the warm Pod by the claim name,
// so the name has to be a DNS subdomain short enough for an annotation key.
export const MODEL_CLAIM_NAME_MAX_LENGTH = 63;
const modelClaimNamePattern = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;

// Flags the runtime launcher sets for every engine. It passes them after
// engineConfig.args, so a claim that sets one again would be ignored.
export const MODEL_CLAIM_LAUNCHER_FLAGS = [
  '--model',
  '--model-path',
  '--served-model-name',
  '--host',
  '--port',
  '--enable-sleep-mode',
  '--enable-memory-saver',
];

// vLLM's short names for the flags read below.
const vllmFlagAliases = new Map([
  ['-tp', '--tensor-parallel-size'],
  ['-pp', '--pipeline-parallel-size'],
  ['-dp', '--data-parallel-size'],
]);

// engineFlag mirrors ModelClaimEngineFlag in pkg/utils/modelclaim.go. It
// returns the flag's long name, with "_" read as "-", and the value written
// after "=", or null without one.
export function engineFlag(key: string): { flag: string; value: string | null } {
  const trimmed = key.trim();
  const equals = trimmed.indexOf('=');
  const name = equals < 0 ? trimmed : trimmed.slice(0, equals);
  const value = equals < 0 ? null : trimmed.slice(equals + 1);
  const long = vllmFlagAliases.get(name);
  if (long !== undefined) return { flag: long, value };
  return { flag: name.startsWith('--') ? `--${name.slice(2).replace(/_/g, '-')}` : name, value };
}

// launcherFlag is the launcher flag that flag sets, or turns off in vLLM's
// --no- form, or null for any other flag.
function launcherFlag(flag: string): string | null {
  if (MODEL_CLAIM_LAUNCHER_FLAGS.includes(flag)) return flag;
  const positive = flag.startsWith('--no-') ? `--${flag.slice('--no-'.length)}` : null;
  return positive !== null && MODEL_CLAIM_LAUNCHER_FLAGS.includes(positive) ? positive : null;
}

const PER_GPU_MAXIMUM_BYTES = 2 ** 50; // 1Pi
const PER_GPU_SMALLEST_ROUNDED_BYTES = 2 ** 20; // 1Mi

const binarySuffixes: Record<string, number> = {
  Ki: 2 ** 10,
  Mi: 2 ** 20,
  Gi: 2 ** 30,
  Ti: 2 ** 40,
  Pi: 2 ** 50,
  Ei: 2 ** 60,
};

const decimalSuffixes: Record<string, number> = {
  n: 1e-9,
  u: 1e-6,
  m: 1e-3,
  '': 1,
  k: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  E: 1e18,
};

const quantityPattern = /^([+-]?(?:\d+(?:\.\d*)?|\.\d+))(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E|[eE][+-]?\d+)?$/;

// parseQuantity reads a Kubernetes resource quantity such as 30Gi, 6.5G or
// 1e9 as a number of bytes, or returns null when it is not a quantity.
export function parseQuantity(value: string): number | null {
  const match = quantityPattern.exec(value.trim());
  if (!match) return null;
  const amount = Number(match[1]);
  const suffix = match[2] ?? '';
  const multiplier = binarySuffixes[suffix] ?? decimalSuffixes[suffix];
  if (multiplier !== undefined) return amount * multiplier;
  return amount * 10 ** Number(suffix.slice(1));
}

export function validateModelClaimName(name: string): string | null {
  const trimmed = name.trim();
  if (!trimmed) return 'Enter a name.';
  if (trimmed.length > MODEL_CLAIM_NAME_MAX_LENGTH) {
    return `Use at most ${MODEL_CLAIM_NAME_MAX_LENGTH} characters.`;
  }
  if (!modelClaimNamePattern.test(trimmed)) {
    return 'Use lowercase letters, numbers, hyphens, and dots, starting and ending with a letter or number.';
  }
  return null;
}

// The artifact schemes the runtime downloader accepts, as pkg/utils
// ValidateArtifactURL lists them.
export const MODEL_CLAIM_ARTIFACT_SCHEMES = ['huggingface://', 'hf://', 's3://', 'gcs://', 'tos://', '/'];

export function validateArtifactUrl(artifactUrl: string): string | null {
  const trimmed = artifactUrl.trim();
  if (!trimmed) return 'Enter the model artifact URL.';
  if (!MODEL_CLAIM_ARTIFACT_SCHEMES.some((scheme) => trimmed.startsWith(scheme))) {
    return 'Use huggingface://, hf://, s3://, gcs://, tos://, or an absolute mounted path.';
  }
  return null;
}

export function validateServedModelName(modelName: string): string | null {
  return /[\s\p{Cc}]/u.test(modelName.trim()) ? 'The served model name must not contain whitespace.' : null;
}

// MODEL_CLAIM_PRIORITY_MAX is the largest value spec.priority (an int32) holds.
export const MODEL_CLAIM_PRIORITY_MAX = 2147483647;

// validatePriority accepts an empty value, which leaves spec.priority at its
// default of zero.
export function validatePriority(value: string): string | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  if (!/^\d+$/.test(trimmed)) return 'Use a whole number of zero or more.';
  if (Number(trimmed) > MODEL_CLAIM_PRIORITY_MAX) return `Must be no more than ${MODEL_CLAIM_PRIORITY_MAX}.`;
  return null;
}

// validatePerGpuFigure mirrors the controller's reading of one spec.perGPU
// figure. There is deliberately no default: the figure has to be measured.
export function validatePerGpuFigure(value: string): string | null {
  const trimmed = value.trim();
  if (!trimmed) return 'Required. Measure it from a run of this model with these engine arguments.';
  const bytes = parseQuantity(trimmed);
  if (bytes === null) return 'Use a quantity such as 30Gi.';
  if (bytes <= 0) return 'Must be positive.';
  if (bytes > PER_GPU_MAXIMUM_BYTES) return 'Must be no more than 1Pi.';
  if (bytes < PER_GPU_SMALLEST_ROUNDED_BYTES && !Number.isInteger(bytes)) {
    return 'Below 1Mi it must be a whole number of bytes. Did you mean M rather than m?';
  }
  return null;
}

export interface EngineArgRow {
  key: string;
  value: string;
}

export interface EngineArgsValidation {
  rowErrors: Array<string | null>;
  // topologyError is about the arguments as a whole: their parallelism, or
  // its fit with the pool.
  topologyError: string | null;
  // parallelism is TP * PP for vLLM, and null when it cannot be read or the
  // engine is not held to the pool topology.
  parallelism: number | null;
  args: Record<string, string>;
}

function positiveEngineArg(flags: Map<string, string>, name: string): number | string {
  const raw = flags.get(name);
  if (raw === undefined) return 1;
  if (!/^\d+$/.test(raw.trim()) || Number(raw.trim()) < 1) return `${name} must be a positive integer.`;
  return Number(raw.trim());
}

export function validateEngineArgs(
  rows: EngineArgRow[],
  engine: ModelClaimEngine,
  pool: ModelClaimPool | null,
): EngineArgsValidation {
  const args: Record<string, string> = {};
  // flags holds each flag under the name vLLM reads it as, with its value.
  const flags = new Map<string, string>();
  const rowErrors = rows.map((row) => {
    const key = row.key.trim();
    const value = row.value.trim();
    if (!key && !value) return null;
    const { flag, value: valueAfterEquals } = engineFlag(key);
    if (!key.startsWith('--') || flag.length <= 2) return 'Use an engine flag such as --max-model-len.';
    if (/\s/.test(key)) return 'A flag must not contain whitespace.';
    if (valueAfterEquals !== null && value) return `${key} already gives a value after "="; leave the value empty.`;
    const owned = launcherFlag(flag);
    if (owned !== null) return `The runtime sets ${owned} for every engine.`;
    if (flags.has(flag)) return `${flag} is set more than once.`;
    if (engine === 'vllm' && flag === '--gpu-memory-utilization') {
      return 'kvcached owns KV memory; remove --gpu-memory-utilization.';
    }
    args[key] = value;
    flags.set(flag, valueAfterEquals ?? value);
    return null;
  });

  if (engine !== 'vllm') {
    return { rowErrors, topologyError: null, parallelism: null, args };
  }
  const tensor = positiveEngineArg(flags, '--tensor-parallel-size');
  const pipeline = positiveEngineArg(flags, '--pipeline-parallel-size');
  const data = positiveEngineArg(flags, '--data-parallel-size');
  for (const value of [tensor, pipeline, data]) {
    if (typeof value === 'string') return { rowErrors, topologyError: value, parallelism: null, args };
  }
  if (data !== 1) {
    return {
      rowErrors,
      topologyError: 'Data parallelism is not supported by fixed-topology pools; keep --data-parallel-size at 1.',
      parallelism: null,
      args,
    };
  }
  const parallelism = (tensor as number) * (pipeline as number);
  const poolGpus = poolGpusPerPod(pool);
  if (poolGpus !== null && poolGpus !== parallelism) {
    return {
      rowErrors,
      topologyError: `TP × PP is ${parallelism}, but Pods in pool ${pool?.name} have ${poolGpus} GPU${poolGpus === 1 ? '' : 's'}. Pick a pool whose Pods have ${parallelism}.`,
      parallelism,
      args,
    };
  }
  return { rowErrors, topologyError: null, parallelism, args };
}

// poolGpusPerPod is the one GPU count a pool holds its Pods to, or null when
// it has none, several, or Pods without nvidia.com/gpu requests.
export function poolGpusPerPod(pool: ModelClaimPool | null): number | null {
  const count = pool?.gpuCounts.length === 1 ? pool.gpuCounts[0] : undefined;
  return count !== undefined && count > 0 ? count : null;
}

export function formatPoolGpus(pool: ModelClaimPool): string {
  if (pool.gpuCounts.length === 0) return '-';
  return pool.gpuCounts.map((count) => (count > 0 ? String(count) : 'no')).join(' / ');
}

export function formatBytes(bytes: number | undefined): string {
  if (!bytes || bytes <= 0) return '-';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${unit === 0 ? value : value.toFixed(value >= 10 ? 0 : 1)} ${units[unit]}`;
}

export interface ModelClaimDraft {
  name: string;
  modelName: string;
  artifactUrl: string;
  engine: ModelClaimEngine;
  poolName: string;
  maximumFootprint: string;
  kvFloor: string;
  priority: string;
  args: Record<string, string>;
}

// buildModelClaimYaml renders the claim the console submits, so it can be
// reviewed or copied into GitOps. User input is quoted as JSON strings, which
// YAML reads as double-quoted scalars.
export function buildModelClaimYaml(draft: ModelClaimDraft, namespace?: string): string {
  const quote = (value: string) => JSON.stringify(value);
  const lines = [
    'apiVersion: model.aibrix.ai/v1alpha1',
    'kind: ModelClaim',
    'metadata:',
    `  name: ${quote(draft.name.trim() || '<name>')}`,
  ];
  if (namespace) lines.push(`  namespace: ${quote(namespace)}`);
  lines.push(
    '  labels:',
    '    app.kubernetes.io/managed-by: aibrix-console',
    'spec:',
  );
  if (draft.modelName.trim()) lines.push(`  modelName: ${quote(draft.modelName.trim())}`);
  lines.push(
    '  podSelector:',
    '    matchLabels:',
    `      claim.model.aibrix.ai/pool: ${quote(draft.poolName || '<pool>')}`,
    `  artifactURL: ${quote(draft.artifactUrl.trim() || '<artifact URL>')}`,
    `  engine: ${draft.engine}`,
    '  perGPU:',
    `    maximumFootprint: ${quote(draft.maximumFootprint.trim() || '<measured>')}`,
    `    kvFloor: ${quote(draft.kvFloor.trim() || '<measured>')}`,
  );
  const priority = draft.priority.trim();
  if (priority && Number(priority) !== 0) lines.push(`  priority: ${priority}`);
  const args = Object.entries(draft.args);
  if (args.length > 0) {
    lines.push('  engineConfig:', '    args:');
    for (const [key, value] of args) {
      lines.push(`      ${quote(key)}: ${quote(value)}`);
    }
  }
  return `${lines.join('\n')}\n`;
}

const conditionOrder = ['Initialized', 'Scheduled', 'Loaded', 'Ready'];

// modelClaimAttention is the first condition in lifecycle order that does
// not hold, which says why a claim that is not Active stands where it does.
export function modelClaimAttention(claim: ModelClaim) {
  if (claim.phase === 'Active') return null;
  const sorted = [...claim.conditions].sort(
    (a, b) => rank(a.type) - rank(b.type),
  );
  return sorted.find((condition) => condition.status !== 'True') ?? null;
}

function rank(type: string): number {
  const index = conditionOrder.indexOf(type);
  return index === -1 ? conditionOrder.length : index;
}

export type ModelClaimPhaseTone = 'ready' | 'sleeping' | 'failed' | 'progress' | 'unknown';

export function modelClaimPhaseTone(phase: string): ModelClaimPhaseTone {
  switch (phase) {
    case 'Active':
      return 'ready';
    case 'Sleeping':
      return 'sleeping';
    case 'Failed':
      return 'failed';
    case 'Unknown':
      return 'unknown';
    default:
      return 'progress';
  }
}
