import { getTimelineEventDetailRenderer } from './registry';

export function TimelineEventDetailCard({
  provider,
  eventType,
  jobStatus,
  rawJson,
}: {
  provider: string;
  eventType: string;
  jobStatus: string;
  rawJson?: string;
}) {
  const Renderer = getTimelineEventDetailRenderer(provider, eventType);
  if (!Renderer || !rawJson) {
    return null;
  }
  let data: unknown;
  try {
    const parsed = JSON.parse(rawJson);
    data = parsed[provider];
    if (!data || typeof data !== 'object') {
      return null;
    }
  } catch {
    return null;
  }
  return <Renderer data={data} jobStatus={jobStatus} />;
}
