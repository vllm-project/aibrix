import type { Job, JobStatus } from '../data/mockData';
import type { ModelDeploymentTemplate } from './api';

const TERMINAL_STATUSES = new Set<JobStatus>([
  'completed',
  'failed',
  'expired',
  'cancelled',
  'resource_failed',
  'submit_failed',
]);

const ATTENTION_STATUSES = new Set<JobStatus>([
  'failed',
  'expired',
  'resource_failed',
  'submit_failed',
]);

export type BatchStatusFilter = '' | JobStatus | 'active' | 'attention';

export interface CreateJobReadinessInput {
  selectedModel: string;
  selectedTemplate: ModelDeploymentTemplate | null;
  displayName: string;
  hasParamErrors: boolean;
  submitting: boolean;
  hasValidUploadedFile: boolean;
  selectedExistingFileId: string;
  hasInferenceOverrides?: boolean;
}

export interface CreateJobReadiness {
  canSubmit: boolean;
  reason: string;
}

export interface CreateJobEndpointInput {
  validationEndpoints: string[];
  selectedEndpoint: string;
  selectedTemplate: ModelDeploymentTemplate | null;
}

export interface BatchDatasetRow {
  key: 'input' | 'output' | 'error';
  label: string;
  fileId: string;
  unavailableReason: string;
}

export interface BatchJobSummary {
  active: number;
  completed: number;
  attention: number;
  totalRequests: number;
}

export type CompletionWindowOption = '1h' | '2h' | '6h' | '12h' | '24h';

export const DEFAULT_COMPLETION_WINDOW: CompletionWindowOption = '24h';

export const COMPLETION_WINDOW_OPTIONS: Array<{
  value: CompletionWindowOption;
  label: string;
}> = [
  { value: '1h', label: '1 hr' },
  { value: '2h', label: '2 hr' },
  { value: '6h', label: '6 hr' },
  { value: '12h', label: '12 hr' },
  { value: '24h', label: '24 hr' },
];

export interface BatchTiming {
  deadlineAt: number | null;
  terminalAt: number | null;
  remainingSeconds: number | null;
  elapsedSeconds: number | null;
}

export function formatModelSelectionLabel(name: string, servingName?: string): string {
  const displayName = name.trim();
  const inferenceName = (servingName || '').trim();

  if (!displayName) return inferenceName;
  if (!inferenceName || inferenceName === displayName) return displayName;
  return `${displayName} (${inferenceName})`;
}

export function parseCompletionWindowSeconds(value?: string): number | null {
  const normalized = (value || '').trim();
  if (!COMPLETION_WINDOW_OPTIONS.some(option => option.value === normalized)) return null;
  return Number(normalized.replace(/h$/, '')) * 60 * 60;
}

function positiveUnix(value?: number): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : null;
}

export function getBatchTiming(
  job: Pick<
    Job,
    | 'status'
    | 'createdAt'
    | 'completionWindow'
    | 'expiresAt'
    | 'completedAt'
    | 'failedAt'
    | 'expiredAt'
    | 'cancelledAt'
    | 'resourceFailedAt'
    | 'submitFailedAt'
  >,
  nowSec: number = Math.floor(Date.now() / 1000),
): BatchTiming {
  const createdAt = positiveUnix(job.createdAt);
  const windowSeconds = parseCompletionWindowSeconds(job.completionWindow);
  const deadlineAt =
    positiveUnix(job.expiresAt) ??
    (createdAt !== null && windowSeconds !== null ? createdAt + windowSeconds : null);
  const terminalAt =
    job.status === 'completed'
      ? positiveUnix(job.completedAt)
      : job.status === 'failed'
        ? positiveUnix(job.failedAt)
        : job.status === 'resource_failed'
          ? positiveUnix(job.resourceFailedAt) ?? positiveUnix(job.failedAt)
          : job.status === 'submit_failed'
            ? positiveUnix(job.submitFailedAt) ?? positiveUnix(job.failedAt)
        : job.status === 'expired'
          ? positiveUnix(job.expiredAt)
          : job.status === 'cancelled'
            ? positiveUnix(job.cancelledAt)
            : null;
  const remainingSeconds =
    terminalAt === null && deadlineAt !== null ? Math.max(0, deadlineAt - nowSec) : null;
  const elapsedSeconds =
    createdAt !== null ? Math.max(0, (terminalAt ?? nowSec) - createdAt) : null;

  return {
    deadlineAt,
    terminalAt,
    remainingSeconds,
    elapsedSeconds,
  };
}

export function formatDuration(seconds: number | null | undefined): string {
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) return '—';
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

export function getCreateJobReadiness(input: CreateJobReadinessInput): CreateJobReadiness {
  if (input.submitting) return { canSubmit: false, reason: 'Creating job' };
  if (!input.selectedModel) return { canSubmit: false, reason: 'Select a model' };
  if (!input.selectedTemplate) return { canSubmit: false, reason: 'Select a deployment template' };
  if (input.displayName.trim() === '') return { canSubmit: false, reason: 'Enter a display name' };
  if (input.hasParamErrors) return { canSubmit: false, reason: 'Fix inference parameters' };
  if (!input.hasValidUploadedFile && input.selectedExistingFileId.trim() === '') {
    return { canSubmit: false, reason: 'Select or upload a dataset' };
  }
  if (input.hasInferenceOverrides && !input.hasValidUploadedFile) {
    return { canSubmit: false, reason: 'Upload a dataset file to apply inference overrides' };
  }
  return { canSubmit: true, reason: '' };
}

export function canCancelBatchJob(status: JobStatus): boolean {
  return !TERMINAL_STATUSES.has(status) && status !== 'cancelling';
}

export function isBatchJobInStatusFilter(status: JobStatus, filter: BatchStatusFilter): boolean {
  if (!filter) return true;
  if (filter === 'active') return !TERMINAL_STATUSES.has(status);
  if (filter === 'attention') return ATTENTION_STATUSES.has(status);
  return status === filter;
}

export function getBatchJobSummary(
  jobs: Pick<Job, 'status' | 'requestCounts'>[],
): BatchJobSummary {
  return jobs.reduce<BatchJobSummary>(
    (summary, job) => {
      if (!TERMINAL_STATUSES.has(job.status)) summary.active += 1;
      if (job.status === 'completed') summary.completed += 1;
      if (ATTENTION_STATUSES.has(job.status)) summary.attention += 1;
      summary.totalRequests += job.requestCounts?.total ?? 0;
      return summary;
    },
    { active: 0, completed: 0, attention: 0, totalRequests: 0 },
  );
}

export function getBatchDatasetRows(job: Pick<Job, 'status' | 'inputDataset' | 'outputDataset' | 'errorDataset'>): BatchDatasetRow[] {
  const rows: BatchDatasetRow[] = [];
  if (job.inputDataset) {
    rows.push({
      key: 'input',
      label: 'Input Dataset',
      fileId: job.inputDataset,
      unavailableReason: '',
    });
  }
  if (job.outputDataset || job.status === 'completed') {
    rows.push({
      key: 'output',
      label: 'Output Dataset',
      fileId: job.outputDataset || '',
      unavailableReason: job.outputDataset ? '' : 'No output file returned yet',
    });
  }
  if (
    job.errorDataset ||
    job.status === 'failed' ||
    job.status === 'expired' ||
    job.status === 'cancelled' ||
    job.status === 'resource_failed' ||
    job.status === 'submit_failed'
  ) {
    rows.push({
      key: 'error',
      label: 'Error Dataset',
      fileId: job.errorDataset || '',
      unavailableReason: job.errorDataset ? '' : 'No error file returned yet',
    });
  }
  return rows;
}

export function getBatchExampleEndpoint(template: ModelDeploymentTemplate | null): string {
  return template?.spec?.supportedEndpoints?.[0] || '/v1/chat/completions';
}

export function getCreateJobEndpoint(input: CreateJobEndpointInput): string {
  return (
    input.validationEndpoints[0] ||
    input.selectedEndpoint ||
    input.selectedTemplate?.spec?.supportedEndpoints?.[0] ||
    '/v1/chat/completions'
  );
}

export function getBatchExampleJsonlLine(template: ModelDeploymentTemplate | null, model: string): string {
  const endpoint = getBatchExampleEndpoint(template);
  const modelName = model || 'your-model';
  let body: Record<string, unknown>;
  switch (endpoint) {
    case '/v1/completions':
      body = { model: modelName, prompt: 'Hello' };
      break;
    case '/v1/embeddings':
      body = { model: modelName, input: 'Hello' };
      break;
    case '/v1/rerank':
      body = {
        model: modelName,
        query: 'What is AIBrix?',
        documents: ['AIBrix is an open source AI inference platform.'],
      };
      break;
    case '/v1/chat/completions':
    default:
      body = {
        model: modelName,
        messages: [{ role: 'user', content: 'Hello' }],
      };
      break;
  }

  return JSON.stringify({
    custom_id: 'req-001',
    method: 'POST',
    url: endpoint,
    body,
  });
}

export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex++;
  }
  const compact = Number.isInteger(value) ? String(value) : value.toFixed(1);
  return `${compact} ${units[unitIndex]}`;
}

export function formatFileCreatedAt(value: string | number | undefined | null): string {
  if (value === undefined || value === null || value === '') return '—';
  let date: Date;
  if (typeof value === 'number') {
    const millis = value > 9999999999 ? value : value * 1000;
    date = new Date(millis);
  } else {
    date = new Date(value);
  }
  if (Number.isNaN(date.getTime())) return '—';
  return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

export async function copyBatchIdentifier(
  jobId: string,
  copy: (value: string) => Promise<void>,
): Promise<'Copied' | 'Copy failed'> {
  try {
    await copy(jobId);
    return 'Copied';
  } catch {
    return 'Copy failed';
  }
}
