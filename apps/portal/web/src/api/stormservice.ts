import { apiFetch, buildQuery, type ListParams, type Pagination } from "./client";

export interface StormServiceListItem {
  name: string;
  namespace: string;
  replicas: number;
  readyReplicas: number;
  rolesCount: number;
  stateful: boolean;
  createdAt: string;
}

export interface StormServiceListResponse {
  items: StormServiceListItem[];
  pagination: Pagination;
}

export interface StormRoleSpec {
  name: string;
  replicas: number;
  image: string;
  cpu?: string;
  memory?: string;
  gpu?: number;
}

export interface StormServiceCreateRequest {
  name: string;
  namespace: string;
  replicas?: number;
  stateful?: boolean;
  roles: StormRoleSpec[];
}

export interface StormServiceDetail {
  name: string;
  namespace: string;
  stateful: boolean;
  status: { replicas: number; readyReplicas: number };
  createdAt: string;
}

export const stormServiceApi = {
  list: (params?: ListParams) =>
    apiFetch<StormServiceListResponse>(`/stormservices${buildQuery(params)}`),
  get: (ns: string, name: string) =>
    apiFetch<StormServiceDetail>(`/stormservices/${ns}/${name}`),
  create: (data: StormServiceCreateRequest) =>
    apiFetch<StormServiceDetail>("/stormservices", { method: "POST", body: JSON.stringify(data) }),
  delete: (ns: string, name: string) =>
    apiFetch<void>(`/stormservices/${ns}/${name}`, { method: "DELETE" }),
};
