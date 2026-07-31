import { ComponentType } from 'react';

export interface ProvisionDetailProps {
  data: unknown;
}

const registry = new Map<string, ComponentType<ProvisionDetailProps>>();

export function registerProvisionDetailRenderer(
  provider: string,
  component: ComponentType<ProvisionDetailProps>,
) {
  registry.set(provider, component);
}

export function getProvisionDetailRenderer(
  provider: string,
): ComponentType<ProvisionDetailProps> | undefined {
  return registry.get(provider);
}
