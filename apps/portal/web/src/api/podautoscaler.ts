import { apiFetch, buildQuery, type ListParams, type Pagination } from "./client";

export interface PodAutoscalerListItem {
  name: string;
  namespace: string;
  scalingStrategy: string;
  minReplicas?: number;
  maxReplicas: number;
  desiredScale: number;
  actualScale: number;
  createdAt: string;
}

export interface PodAutoscalerListResponse {
  items: PodAutoscalerListItem[];
  pagination: Pagination;
}

export interface PodAutoscalerCreateRequest {
  name: string;
  namespace: string;
  scaleTargetRef: { apiVersion: string; kind: string; name: string; namespace?: string };
  minReplicas?: number;
  maxReplicas: number;
  scalingStrategy: string;
}

export interface PodAutoscalerDetail {
  name: string;
  namespace: string;
  status: { desiredScale: number; actualScale: number };
  createdAt: string;
}

export const podAutoscalerApi = {
  list: (params?: ListParams) =>
    apiFetch<PodAutoscalerListResponse>(`/podautoscalers${buildQuery(params)}`),
  get: (ns: string, name: string) =>
    apiFetch<PodAutoscalerDetail>(`/podautoscalers/${ns}/${name}`),
  create: (data: PodAutoscalerCreateRequest) =>
    apiFetch<PodAutoscalerDetail>("/podautoscalers", { method: "POST", body: JSON.stringify(data) }),
  delete: (ns: string, name: string) =>
    apiFetch<void>(`/podautoscalers/${ns}/${name}`, { method: "DELETE" }),
};
