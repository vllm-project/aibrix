import type { Deployment } from '../data/mockData';
import type { ModelClaim } from './api';
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

// A model served by a ModelClaim has no HTTPRoute, so the gateway only routes
// a request for it when the request names a routing strategy. ModelClaim
// serves one replica, so any strategy picks the same engine.
export const MODEL_CLAIM_ROUTING_STRATEGY = 'random';

// PlaygroundTarget is something the Playground can chat with: a console
// Deployment, or the served model of a ModelClaim.
export interface PlaygroundTarget {
  key: string;
  kind: 'deployment' | 'modelclaim';
  // The Deployment id or the ModelClaim name.
  id: string;
  name: string;
  servingName: string;
  // What the avatar initials are taken from.
  label: string;
  // Shown under the target when it is not simply ready, e.g. a sleeping claim.
  note?: string;
  routingStrategy?: string;
  // The engine's --max-model-len, when it sets one. vLLM refuses a request
  // whose max_tokens is larger.
  maxModelLen?: number;
}

function servingNameOf(deployment: Deployment): string {
  return deployment.servingName?.trim() ?? '';
}

export function callableDeployments(deployments: Deployment[]): Deployment[] {
  return deployments.filter(
    (deployment) => normalizeDeploymentStatus(deployment.status) === 'Ready'
      && servingNameOf(deployment) !== '',
  );
}

export function deploymentTargets(deployments: Deployment[]): PlaygroundTarget[] {
  return callableDeployments(deployments).map((deployment) => ({
    key: `deployment:${deployment.id}`,
    kind: 'deployment',
    id: deployment.id,
    name: deployment.name,
    servingName: servingNameOf(deployment),
    label: deployment.baseModel || deployment.name,
  }));
}

function maxModelLenOf(claim: ModelClaim): number | undefined {
  const args = claim.engineArgs ?? {};
  // Number() ignores surrounding whitespace and reads a value that is not a string.
  const value = Number(args['--max-model-len'] ?? args['--max_model_len']);
  return Number.isInteger(value) && value > 0 ? value : undefined;
}

// maxTokensFor keeps max_tokens within the target's context, leaving half of
// it for the prompt.
export function maxTokensFor(target: PlaygroundTarget | null, current: number): number {
  if (!target?.maxModelLen) return current;
  return Math.max(1, Math.min(current, Math.floor(target.maxModelLen / 2)));
}

// modelClaimTargets lists the claims a request can reach: Active ones, and
// Sleeping ones, which the first request wakes.
export function modelClaimTargets(claims: ModelClaim[]): PlaygroundTarget[] {
  return claims
    .filter((claim) => !claim.deleting
      && claim.modelName?.trim()
      && (claim.phase === 'Active' || claim.phase === 'Sleeping'))
    .map((claim) => ({
      key: `modelclaim:${claim.name}`,
      kind: 'modelclaim',
      id: claim.name,
      name: claim.name,
      servingName: claim.modelName.trim(),
      label: claim.modelName.trim(),
      note: claim.phase === 'Sleeping' ? 'Sleeping: the first request wakes it' : undefined,
      routingStrategy: MODEL_CLAIM_ROUTING_STRATEGY,
      maxModelLen: maxModelLenOf(claim),
    }));
}

export function preferredTarget(
  targets: PlaygroundTarget[],
  requested: { deployment?: string; modelClaim?: string } = {},
): PlaygroundTarget | null {
  const match = requested.modelClaim
    ? targets.find((target) => target.kind === 'modelclaim' && target.id === requested.modelClaim)
    : requested.deployment
      ? targets.find((target) => target.kind === 'deployment'
        && (target.id === requested.deployment || target.servingName === requested.deployment))
      : undefined;
  return match ?? targets[0] ?? null;
}

function eventContent(event: string): string {
  const data = event
    .split(/\r?\n/)
    .filter((line) => line.startsWith('data:'))
    .map((line) => line.slice(5).trimStart())
    .join('\n');
  if (!data || data === '[DONE]') return '';

  try {
    const parsed = JSON.parse(data) as {
      choices?: Array<{
        delta?: { content?: string };
        message?: { content?: string };
        text?: string;
      }>;
    };
    const choice = parsed.choices?.[0];
    return choice?.delta?.content ?? choice?.message?.content ?? choice?.text ?? '';
  } catch {
    return '';
  }
}

// playgroundErrorMessage reads the message of a gateway error,
// {"error":{"message":"..."}}, or of a console error, {"error":"..."}.
export function playgroundErrorMessage(body: string, status: number): string {
  const text = body.trim();
  try {
    const parsed = JSON.parse(text) as { error?: string | { message?: string } };
    const message = typeof parsed.error === 'string' ? parsed.error : parsed.error?.message;
    if (typeof message === 'string' && message.trim()) return message.trim();
  } catch {
    // Not JSON: show the body as it is.
  }
  return text || `Playground request failed with status ${status}`;
}

export interface StreamPlaygroundOptions {
  routingStrategy?: string;
  // Called before each wait, while the gateway answers 503 with Retry-After
  // (a ModelClaim that is waking up).
  onWaiting?: (message: string, retryAfterSeconds: number) => void;
  // How long to keep retrying such a 503 before giving up.
  maxWaitMs?: number;
  sleep?: (ms: number) => Promise<void>;
}

const DEFAULT_MAX_WAIT_MS = 120_000;
const defaultSleep = (ms: number) => new Promise<void>((resolve) => { setTimeout(resolve, ms); });

export async function streamPlaygroundChat(
  request: PlaygroundChatRequest,
  onDelta: (delta: string) => void,
  fetcher: Fetcher = fetch,
  options: StreamPlaygroundOptions = {},
): Promise<string> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (options.routingStrategy) headers['routing-strategy'] = options.routingStrategy;
  const sleep = options.sleep ?? defaultSleep;
  const deadline = Date.now() + (options.maxWaitMs ?? DEFAULT_MAX_WAIT_MS);

  let response: Response;
  for (;;) {
    response = await fetcher('/api/v1/playground/chat/completions', {
      method: 'POST',
      headers,
      body: JSON.stringify(request),
    });
    const retryAfter = Number(response.headers.get('retry-after'));
    if (response.status !== 503 || !(retryAfter > 0) || Date.now() + retryAfter * 1000 > deadline) break;
    options.onWaiting?.(playgroundErrorMessage(await response.text(), response.status), retryAfter);
    await sleep(retryAfter * 1000);
  }
  if (!response.ok) {
    throw new Error(playgroundErrorMessage(await response.text(), response.status));
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
