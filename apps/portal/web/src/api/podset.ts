import { apiFetch, buildQuery, type ListParams, type Pagination } from "./client";

export interface PodSetListItem {
  name: string;
  namespace: string;
  podGroupSize: number;
  readyPods: number;
  totalPods: number;
  phase: string;
  createdAt: string;
}

export interface PodSetListResponse {
  items: PodSetListItem[];
  pagination: Pagination;
}

export interface PodSetCreateRequest {
  name: string;
  namespace: string;
  podGroupSize: number;
  stateful?: boolean;
}

export interface PodSetDetail {
  name: string;
  namespace: string;
  podGroupSize: number;
  stateful: boolean;
  status: { readyPods: number; totalPods: number; phase: string };
  createdAt: string;
}

export const podSetApi = {
  list: (params?: ListParams) =>
    apiFetch<PodSetListResponse>(`/podsets${buildQuery(params)}`),
  get: (ns: string, name: string) =>
    apiFetch<PodSetDetail>(`/podsets/${ns}/${name}`),
  create: (data: PodSetCreateRequest) =>
    apiFetch<PodSetDetail>("/podsets", { method: "POST", body: JSON.stringify(data) }),
  delete: (ns: string, name: string) =>
    apiFetch<void>(`/podsets/${ns}/${name}`, { method: "DELETE" }),
};
