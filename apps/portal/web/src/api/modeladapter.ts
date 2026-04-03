import { apiFetch, buildQuery, type ListParams, type Pagination } from "./client";

export interface ModelAdapterListItem {
  name: string;
  namespace: string;
  baseModel?: string;
  artifactURL: string;
  phase: string;
  readyReplicas: number;
  desiredReplicas: number;
  createdAt: string;
}

export interface ModelAdapterListResponse {
  items: ModelAdapterListItem[];
  pagination: Pagination;
}

export interface ModelAdapterCreateRequest {
  name: string;
  namespace: string;
  baseModel?: string;
  artifactURL: string;
  podSelector: Record<string, string>;
  replicas?: number;
}

export interface ModelAdapterDetail {
  name: string;
  namespace: string;
  spec: { baseModel?: string; artifactURL: string; podSelector?: Record<string, string>; replicas?: number };
  status: { phase: string; readyReplicas: number; desiredReplicas: number; instances?: string[]; conditions?: unknown[] };
  createdAt: string;
}

export const modelAdapterApi = {
  list: (params?: ListParams) =>
    apiFetch<ModelAdapterListResponse>(`/modeladapters${buildQuery(params)}`),
  get: (ns: string, name: string) =>
    apiFetch<ModelAdapterDetail>(`/modeladapters/${ns}/${name}`),
  create: (data: ModelAdapterCreateRequest) =>
    apiFetch<ModelAdapterDetail>("/modeladapters", { method: "POST", body: JSON.stringify(data) }),
  delete: (ns: string, name: string) =>
    apiFetch<void>(`/modeladapters/${ns}/${name}`, { method: "DELETE" }),
};
