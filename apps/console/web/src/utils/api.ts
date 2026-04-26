import type { Job, Deployment, Model } from '../data/mockData';

// --- Additional interfaces for API entities ---

export interface APIKey {
  id: string;
  name: string;
  secretKey: string;
  createdAt: string;
}

export interface Secret {
  id: string;
  name: string;
  createdAt: string;
}

export interface Quota {
  id: string;
  quotaId: string;
  name: string;
  currentUsage: number;
  usagePercentage: number;
  quota: number;
}

export interface FileInfo {
  id: string;
  name: string;
  purpose: string;
  size: number;
  createdAt: string;
}

export interface UserInfo {
  id: string;
  email: string;
  name: string;
}

export type JobEndpoint =
  | '/v1/chat/completions'
  | '/v1/completions'
  | '/v1/embeddings';

export interface CreateJobRequest {
  inputDataset: string;
  endpoint: JobEndpoint;
  completionWindow?: '24h';
  name: string;
  // Reserved fields — Console contract keeps them for future per-batch
  // overrides. Backend currently does not forward them.
  maxTokens?: number;
  temperature?: number;
  topP?: number;
  n?: number;
  // ModelDeploymentTemplate binding picked by the create-job wizard. The SDK
  // path may omit these and rely on metadata-service-side resolution via
  // extra_body.aibrix.model_template.
  modelTemplateName?: string;
  modelTemplateVersion?: string;
}

export interface ListJobsResponse {
  jobs: Job[];
  firstId?: string;
  lastId?: string;
  hasMore: boolean;
}

export interface CreateDeploymentRequest {
  name: string;
  baseModel: string;
  region: string;
  acceleratorType: string;
  acceleratorCount: number;
  quantization?: string;
  minReplicas: number;
  maxReplicas?: number;
  enableAutoScaling?: boolean;
  enableMultiLora?: boolean;
}

// --- Model Deployment Templates ---
//
// Mirrors apps/console/api/proto/console/v1/console.proto. The proto comment
// notes that this duplicates python/aibrix/aibrix/batch/template/schema.py;
// once both sides converge we collapse to one source.

export interface EngineSpec {
  type?: string;
  version?: string;
  image?: string;
  invocation?: string;
  serveArgs?: string[];
  healthEndpoint?: string;
  readyTimeoutSeconds?: number;
  metricsEndpoint?: string;
}

export interface ModelSourceSpec {
  type?: string;
  uri?: string;
  revision?: string;
  tokenizerPath?: string;
  chatTemplatePath?: string;
  authSecretRef?: string;
}

export interface AcceleratorSpec {
  type?: string;
  count?: number;
  interconnect?: string;
  vramGb?: number;
  skuHint?: string;
}

export interface ParallelismSpec {
  tp?: number;
  pp?: number;
  dp?: number;
  ep?: number;
  sp?: number;
  cp?: number;
}

export interface QuantizationSpec {
  weight?: string;
  kvCache?: string;
  weightsArtifactUri?: string;
}

export interface ProviderConfig {
  type?: string;
  extra?: Record<string, string>;
}

export interface ModelDeploymentTemplateSpec {
  engine?: EngineSpec;
  modelSource?: ModelSourceSpec;
  accelerator?: AcceleratorSpec;
  parallelism?: ParallelismSpec;
  // engineArgs is a free-form key/value map. Common knobs are surfaced by
  // the form as curated inputs; everything else flows through directly.
  engineArgs?: Record<string, string>;
  quantization?: QuantizationSpec;
  providerConfig?: ProviderConfig;
  supportedEndpoints?: string[];
  deploymentMode?: string;
}

export interface ModelDeploymentTemplate {
  id: string;
  name: string;
  version: string;
  status: string;
  modelId: string;
  spec?: ModelDeploymentTemplateSpec;
  createdAt?: string;
  updatedAt?: string;
}

export interface CreateModelDeploymentTemplateRequest {
  name: string;
  version?: string;
  status?: string;
  modelId: string;
  spec: ModelDeploymentTemplateSpec;
}

export interface UpdateModelDeploymentTemplateRequest {
  id: string;
  modelId: string;
  name?: string;
  version?: string;
  status?: string;
  spec?: ModelDeploymentTemplateSpec;
}

// --- Case conversion utilities ---

function snakeToCamelKey(key: string): string {
  return key.replace(/_([a-z])/g, (_, letter) => letter.toUpperCase());
}

function camelToSnakeKey(key: string): string {
  return key.replace(/[A-Z]/g, (letter) => `_${letter.toLowerCase()}`);
}

export function snakeToCamel<T>(data: unknown): T {
  if (Array.isArray(data)) {
    return data.map((item) => snakeToCamel(item)) as unknown as T;
  }
  if (data !== null && typeof data === 'object') {
    const result: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(data as Record<string, unknown>)) {
      result[snakeToCamelKey(key)] = snakeToCamel(value);
    }
    return result as T;
  }
  return data as T;
}

export function camelToSnake<T>(data: unknown): T {
  if (Array.isArray(data)) {
    return data.map((item) => camelToSnake(item)) as unknown as T;
  }
  if (data !== null && typeof data === 'object') {
    const result: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(data as Record<string, unknown>)) {
      result[camelToSnakeKey(key)] = camelToSnake(value);
    }
    return result as T;
  }
  return data as T;
}

// --- Fetch helper ---

class APIError extends Error {
  status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = 'APIError';
    this.status = status;
  }
}

async function apiFetch<T>(
  url: string,
  options?: RequestInit,
): Promise<T> {
  const response = await fetch(url, {
    ...options,
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...options?.headers,
    },
  });

  if (!response.ok) {
    const text = await response.text().catch(() => 'Unknown error');
    throw new APIError(text, response.status);
  }

  if (response.status === 204) {
    return undefined as T;
  }

  const json = await response.json();
  return snakeToCamel<T>(json);
}

function buildQuery(params: Record<string, string | undefined>): string {
  const entries = Object.entries(params).filter(
    (entry): entry is [string, string] => entry[1] !== undefined && entry[1] !== '',
  );
  if (entries.length === 0) return '';
  return '?' + new URLSearchParams(entries).toString();
}

// --- Jobs ---
//
// The Console BFF (`/api/v1/jobs`) proxies to the metadata service
// `/v1/batches` API and merges with Console-side fields persisted in the
// store. The Job shape is a superset of OpenAI Batch.

export async function listJobs(params?: { after?: string; limit?: number }): Promise<ListJobsResponse> {
  const query = buildQuery({
    after: params?.after,
    limit: params?.limit !== undefined ? String(params.limit) : undefined,
  });
  return apiFetch<ListJobsResponse>(`/api/v1/jobs${query}`);
}

export async function getJob(id: string): Promise<Job> {
  return apiFetch<Job>(`/api/v1/jobs/${encodeURIComponent(id)}`);
}

export async function createJob(req: CreateJobRequest): Promise<Job> {
  return apiFetch<Job>('/api/v1/jobs', {
    method: 'POST',
    body: JSON.stringify(camelToSnake(req)),
  });
}

export async function cancelJob(id: string): Promise<Job> {
  return apiFetch<Job>(`/api/v1/jobs/${encodeURIComponent(id)}/cancel`, {
    method: 'POST',
    body: '{}',
  });
}

// --- Deployments ---

export async function listDeployments(search?: string): Promise<Deployment[]> {
  const query = buildQuery({ search });
  const data = await apiFetch<{ deployments: Deployment[] }>(`/api/v1/deployments${query}`);
  return data.deployments || [];
}

export async function getDeployment(id: string): Promise<Deployment> {
  return apiFetch<Deployment>(`/api/v1/deployments/${encodeURIComponent(id)}`);
}

export async function createDeployment(req: CreateDeploymentRequest): Promise<Deployment> {
  return apiFetch<Deployment>('/api/v1/deployments', {
    method: 'POST',
    body: JSON.stringify(camelToSnake(req)),
  });
}

export async function deleteDeployment(id: string): Promise<void> {
  return apiFetch<void>(`/api/v1/deployments/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
}

// --- Models ---

export async function listModels(search?: string, category?: string): Promise<Model[]> {
  const query = buildQuery({ search, category });
  const data = await apiFetch<{ models: Model[] }>(`/api/v1/models${query}`);
  return data.models || [];
}

export async function getModel(id: string): Promise<Model> {
  return apiFetch<Model>(`/api/v1/models/${encodeURIComponent(id)}`);
}

// --- Model Deployment Templates ---

export async function listModelDeploymentTemplates(
  modelId: string,
  status?: string,
): Promise<ModelDeploymentTemplate[]> {
  const query = buildQuery({ status });
  const data = await apiFetch<{ templates: ModelDeploymentTemplate[] }>(
    `/api/v1/models/${encodeURIComponent(modelId)}/deployment-templates${query}`,
  );
  return data.templates || [];
}

export async function getModelDeploymentTemplate(
  modelId: string,
  id: string,
): Promise<ModelDeploymentTemplate> {
  return apiFetch<ModelDeploymentTemplate>(
    `/api/v1/models/${encodeURIComponent(modelId)}/deployment-templates/${encodeURIComponent(id)}`,
  );
}

export async function createModelDeploymentTemplate(
  req: CreateModelDeploymentTemplateRequest,
): Promise<ModelDeploymentTemplate> {
  return apiFetch<ModelDeploymentTemplate>(
    `/api/v1/models/${encodeURIComponent(req.modelId)}/deployment-templates`,
    {
      method: 'POST',
      body: JSON.stringify(camelToSnake(req)),
    },
  );
}

export async function updateModelDeploymentTemplate(
  req: UpdateModelDeploymentTemplateRequest,
): Promise<ModelDeploymentTemplate> {
  return apiFetch<ModelDeploymentTemplate>(
    `/api/v1/models/${encodeURIComponent(req.modelId)}/deployment-templates/${encodeURIComponent(req.id)}`,
    {
      method: 'PUT',
      body: JSON.stringify(camelToSnake(req)),
    },
  );
}

export async function deleteModelDeploymentTemplate(
  modelId: string,
  id: string,
): Promise<void> {
  return apiFetch<void>(
    `/api/v1/models/${encodeURIComponent(modelId)}/deployment-templates/${encodeURIComponent(id)}`,
    { method: 'DELETE' },
  );
}

// resolveModelDeploymentTemplate looks up a template by (modelId, name, version).
// version="" means "latest active". This is the same lookup that batch SDK
// callers will use when they pass model_template + model_template_version
// in extra_body.aibrix.
export async function resolveModelDeploymentTemplate(
  modelId: string,
  name: string,
  version?: string,
): Promise<ModelDeploymentTemplate> {
  const query = buildQuery({ version });
  return apiFetch<ModelDeploymentTemplate>(
    `/api/v1/models/${encodeURIComponent(modelId)}/deployment-templates/by-name/${encodeURIComponent(name)}${query}`,
  );
}

// --- API Keys ---

export async function listAPIKeys(): Promise<APIKey[]> {
  const data = await apiFetch<{ apiKeys: APIKey[] }>('/api/v1/apikeys');
  return data.apiKeys || [];
}

export async function createAPIKey(
  name: string,
): Promise<{ apiKey: APIKey; fullKey: string }> {
  return apiFetch<{ apiKey: APIKey; fullKey: string }>('/api/v1/apikeys', {
    method: 'POST',
    body: JSON.stringify({ name }),
  });
}

export async function deleteAPIKey(id: string): Promise<void> {
  return apiFetch<void>(`/api/v1/apikeys/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
}

// --- Secrets ---

export async function listSecrets(search?: string): Promise<Secret[]> {
  const query = buildQuery({ search });
  const data = await apiFetch<{ secrets: Secret[] }>(`/api/v1/secrets${query}`);
  return data.secrets || [];
}

export async function createSecret(name: string, value: string): Promise<Secret> {
  return apiFetch<Secret>('/api/v1/secrets', {
    method: 'POST',
    body: JSON.stringify({ name, value }),
  });
}

export async function deleteSecret(id: string): Promise<void> {
  return apiFetch<void>(`/api/v1/secrets/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
}

// --- Quotas ---

export async function listQuotas(search?: string): Promise<Quota[]> {
  const query = buildQuery({ search });
  const data = await apiFetch<{ quotas: Quota[] }>(`/api/v1/quotas${query}`);
  return data.quotas || [];
}

// --- Files ---

export async function uploadFile(file: File, purpose?: string): Promise<FileInfo> {
  const formData = new FormData();
  formData.append('file', file);
  if (purpose) {
    formData.append('purpose', purpose);
  }

  const response = await fetch('/api/v1/files/upload', {
    method: 'POST',
    credentials: 'include',
    body: formData,
    // Do not set Content-Type; the browser sets it with the boundary
  });

  if (!response.ok) {
    const text = await response.text().catch(() => 'Unknown error');
    throw new APIError(text, response.status);
  }

  const json = await response.json();
  return snakeToCamel<FileInfo>(json);
}

export async function listFiles(): Promise<FileInfo[]> {
  const data = await apiFetch<{ files: FileInfo[] }>('/api/v1/files');
  return data.files;
}

// --- Auth ---

export async function getAuthConfig(): Promise<{ mode: string; providerName?: string }> {
  return apiFetch<{ mode: string; providerName?: string }>('/api/v1/auth/config');
}

export async function getUserInfo(): Promise<UserInfo | null> {
  return apiFetch<UserInfo | null>('/api/v1/auth/userinfo');
}

export async function logout(): Promise<void> {
  return apiFetch<void>('/api/v1/auth/logout', {
    method: 'POST',
  });
}
