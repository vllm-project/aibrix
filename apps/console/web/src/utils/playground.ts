import type { Deployment } from '../data/mockData';
import { normalizeDeploymentStatus } from './deploymentStatus';

export interface PlaygroundChatMessage {
  role: 'system' | 'user' | 'assistant';
  content: string;
}

export interface PlaygroundChatRequest {
  model: string;
  messages: PlaygroundChatMessage[];
  temperature?: number;
  max_tokens?: number;
  top_p?: number;
  top_k?: number;
  presence_penalty?: number;
  frequency_penalty?: number;
  stop?: string[];
}

type Fetcher = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

export function callableDeployments(deployments: Deployment[]): Deployment[] {
  return deployments.filter(
    (deployment) => normalizeDeploymentStatus(deployment.status) === 'Ready'
      && deployment.servingName.trim() !== '',
  );
}

export function preferredDeployment(
  deployments: Deployment[],
  requestedDeployment?: string,
): Deployment | null {
  if (!requestedDeployment) return deployments[0] ?? null;
  return deployments.find(
    (deployment) => deployment.id === requestedDeployment
      || deployment.servingName === requestedDeployment,
  ) ?? deployments[0] ?? null;
}

function eventContent(event: string): string {
  const data = event
    .split(/\r?\n/)
    .filter((line) => line.startsWith('data:'))
    .map((line) => line.slice(5).trimStart())
    .join('\n');
  if (!data || data === '[DONE]') return '';

  const parsed = JSON.parse(data) as {
    choices?: Array<{
      delta?: { content?: string };
      message?: { content?: string };
      text?: string;
    }>;
  };
  const choice = parsed.choices?.[0];
  return choice?.delta?.content ?? choice?.message?.content ?? choice?.text ?? '';
}

export async function streamPlaygroundChat(
  request: PlaygroundChatRequest,
  onDelta: (delta: string) => void,
  fetcher: Fetcher = fetch,
): Promise<string> {
  const response = await fetcher('/api/v1/playground/chat/completions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(request),
  });
  if (!response.ok) {
    const detail = (await response.text()).trim();
    throw new Error(detail || `Playground request failed with status ${response.status}`);
  }
  if (!response.body) {
    throw new Error('Playground response did not include a stream');
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let content = '';

  const consume = (event: string) => {
    const delta = eventContent(event);
    if (!delta) return;
    content += delta;
    onDelta(delta);
  };

  while (true) {
    const { done, value } = await reader.read();
    buffer += decoder.decode(value, { stream: !done });
    const events = buffer.split(/\r?\n\r?\n/);
    buffer = events.pop() ?? '';
    events.forEach(consume);
    if (done) break;
  }
  if (buffer.trim()) consume(buffer);
  return content;
}
