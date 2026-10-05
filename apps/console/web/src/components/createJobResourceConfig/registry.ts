import type { ComponentType } from 'react';
import type { CompletionWindowOption } from '../../utils/batchProduct';

export type CreateJobResourceConfig = Record<string, unknown>;

export interface CreateJobResourceFieldsProps {
  completionWindow: CompletionWindowOption;
  value: CreateJobResourceConfig;
  onChange: (value: CreateJobResourceConfig) => void;
}

export interface CreateJobResourcePlugin {
  Fields: ComponentType<CreateJobResourceFieldsProps>;
  normalize?: (
    value: CreateJobResourceConfig,
    completionWindow: CompletionWindowOption,
  ) => CreateJobResourceConfig;
  toProviderConfig?: (
    value: CreateJobResourceConfig,
    completionWindow: CompletionWindowOption,
  ) => CreateJobResourceConfig;
}

const registry = new Map<string, CreateJobResourcePlugin>();

export function registerCreateJobResourcePlugin(
  provider: string,
  plugin: CreateJobResourcePlugin,
) {
  registry.set(provider.toLowerCase(), plugin);
}

export function getCreateJobResourcePlugin(
  provider: string,
): CreateJobResourcePlugin | undefined {
  return registry.get(provider.toLowerCase());
}
