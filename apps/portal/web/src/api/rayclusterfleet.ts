import { apiFetch, buildQuery, type ListParams, type Pagination } from "./client";

export interface RayClusterFleetListItem {
  name: string;
  namespace: string;
  replicas: number;
  readyReplicas: number;
  availableReplicas: number;
  createdAt: string;
}

export interface RayClusterFleetListResponse {
  items: RayClusterFleetListItem[];
  pagination: Pagination;
}

export interface RayClusterFleetCreateRequest {
  name: string;
  namespace: string;
  replicas?: number;
  rayVersion?: string;
}

export interface RayClusterFleetDetail {
  name: string;
  namespace: string;
  replicas: number;
  status: { replicas: number; readyReplicas: number; updatedReplicas: number; availableReplicas: number; unavailableReplicas: number };
  createdAt: string;
}

export const rayClusterFleetApi = {
  list: (params?: ListParams) =>
    apiFetch<RayClusterFleetListResponse>(`/rayclusterfleets${buildQuery(params)}`),
  get: (ns: string, name: string) =>
    apiFetch<RayClusterFleetDetail>(`/rayclusterfleets/${ns}/${name}`),
  create: (data: RayClusterFleetCreateRequest) =>
    apiFetch<RayClusterFleetDetail>("/rayclusterfleets", { method: "POST", body: JSON.stringify(data) }),
  delete: (ns: string, name: string) =>
    apiFetch<void>(`/rayclusterfleets/${ns}/${name}`, { method: "DELETE" }),
};
