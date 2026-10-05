import { getProvisionDetailRenderer } from './registry';

export function ProvisionDetailCard({
  provider,
  rawJson,
}: {
  provider: string;
  rawJson?: string;
}) {
  const Renderer = getProvisionDetailRenderer(provider);
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
  return <Renderer data={data} />;
}
