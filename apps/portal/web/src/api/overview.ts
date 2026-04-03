import { apiFetch } from "./client";

export interface OverviewCount {
  total: number;
  ready: number;
  notReady: number;
}

export interface OverviewResponse {
  modelAdapters: OverviewCount;
  rayClusterFleets: OverviewCount;
  stormServices: OverviewCount;
  podAutoscalers: OverviewCount;
  kvCaches: OverviewCount;
  podSets: OverviewCount;
}

export const overviewApi = {
  get: () => apiFetch<OverviewResponse>("/overview"),
};
