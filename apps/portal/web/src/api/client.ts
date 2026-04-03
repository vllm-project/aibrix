const BASE = "/api/v1";

export interface ApiError {
  error: { code: number; message: string; reason: string };
}

export interface Pagination {
  page: number;
  pageSize: number;
  total: number;
}

export interface ListParams {
  namespace?: string;
  page?: number;
  pageSize?: number;
}

export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    headers: { "Content-Type": "application/json", ...init?.headers },
    ...init,
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: { message: res.statusText } }));
    throw new Error(body?.error?.message || res.statusText);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

export function buildQuery(params?: ListParams): string {
  if (!params) return "";
  const q = new URLSearchParams();
  if (params.namespace) q.set("namespace", params.namespace);
  if (params.page) q.set("page", String(params.page));
  if (params.pageSize) q.set("pageSize", String(params.pageSize));
  const s = q.toString();
  return s ? `?${s}` : "";
}
