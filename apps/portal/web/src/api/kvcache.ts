import { apiFetch, buildQuery, type ListParams, type Pagination } from "./client";

export interface KVCacheListItem {
  name: string;
  namespace: string;
  mode: string;
  readyReplicas: number;
  createdAt: string;
}

export interface KVCacheListResponse {
  items: KVCacheListItem[];
  pagination: Pagination;
}

export interface KVCacheCreateRequest {
  name: string;
  namespace: string;
  mode?: string;
}

export interface KVCacheDetail {
  name: string;
  namespace: string;
  mode: string;
  status: { readyReplicas: number };
  createdAt: string;
}

export const kvCacheApi = {
  list: (params?: ListParams) =>
    apiFetch<KVCacheListResponse>(`/kvcaches${buildQuery(params)}`),
  get: (ns: string, name: string) =>
    apiFetch<KVCacheDetail>(`/kvcaches/${ns}/${name}`),
  create: (data: KVCacheCreateRequest) =>
    apiFetch<KVCacheDetail>("/kvcaches", { method: "POST", body: JSON.stringify(data) }),
  delete: (ns: string, name: string) =>
    apiFetch<void>(`/kvcaches/${ns}/${name}`, { method: "DELETE" }),
};
