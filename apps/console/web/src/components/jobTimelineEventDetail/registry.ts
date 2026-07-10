import { ComponentType } from 'react';

export interface TimelineEventDetailProps {
  data: unknown;
  jobStatus?: string;
}

const registry = new Map<string, ComponentType<TimelineEventDetailProps>>();

function makeKey(provider: string, type: string): string {
  return `${provider}:${type}`;
}

export function registerTimelineEventDetailRenderer(
  provider: string,
  type: string,
  component: ComponentType<TimelineEventDetailProps>,
) {
  registry.set(makeKey(provider, type), component);
}

export function getTimelineEventDetailRenderer(
  provider: string,
  type: string,
): ComponentType<TimelineEventDetailProps> | undefined {
  return registry.get(makeKey(provider, type));
}
