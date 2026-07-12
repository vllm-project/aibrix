import { ComponentType } from 'react';
import type { Job } from '../../data/mockData';

export interface DeploymentDetailProps {
  data: unknown;
  onRefresh?: () => Promise<Job | null>;
}

const registry = new Map<string, ComponentType<DeploymentDetailProps>>();

export function registerDeploymentDetailRenderer(
  type: string,
  component: ComponentType<DeploymentDetailProps>,
) {
  registry.set(type, component);
}

export function getDeploymentDetailRenderer(
  type: string,
): ComponentType<DeploymentDetailProps> | undefined {
  return registry.get(type);
}
