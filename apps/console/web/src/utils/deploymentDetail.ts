import type { Deployment } from '../data/mockData';
import { normalizeDeploymentStatus } from './deploymentStatus';

export type DeploymentExampleLanguage = 'python' | 'shell';

function trimmed(value?: string): string {
  return value?.trim() ?? '';
}

export function canOpenInPlayground(deployment: Deployment): boolean {
  return normalizeDeploymentStatus(deployment.status) === 'Ready' && trimmed(deployment.servingName) !== '';
}

export function deploymentExamplesAvailable(deployment: Deployment): boolean {
  return trimmed(deployment.servingName) !== '' && trimmed(deployment.inferenceUrl) !== '';
}

export function playgroundHref(deployment: Pick<Deployment, 'id' | 'servingName'>): string {
  const selected = trimmed(deployment.id) || trimmed(deployment.servingName);
  return `/playground?deployment=${encodeURIComponent(selected)}`;
}

export function formatDeploymentCreatedAt(createdAt: string): string {
  if (!createdAt) return 'Not available';
  const date = new Date(createdAt);
  if (Number.isNaN(date.getTime())) return 'Not available';
  return new Intl.DateTimeFormat('en-US', {
    dateStyle: 'long',
    timeStyle: 'long',
    timeZone: 'UTC',
  }).format(date);
}

export function deploymentCodeExample(
  deployment: Deployment,
  language: DeploymentExampleLanguage,
): string {
  const servingName = trimmed(deployment.servingName) || '<serving-name>';
  const url = trimmed(deployment.inferenceUrl);
  if (language === 'shell') {
    const target = url || '"$AIBRIX_GATEWAY_URL/v1/chat/completions"';
    const quotedTarget = url ? `"${url}"` : target;
    return `curl ${quotedTarget} \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${servingName}",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'`;
  }

  const pythonUrl = url
    ? `"${url}"`
    : `f"{os.environ['AIBRIX_GATEWAY_URL']}/v1/chat/completions"`;
  const imports = url ? 'import requests' : `import os
import requests`;
  return `${imports}

response = requests.post(
    ${pythonUrl},
    json={
        "model": "${servingName}",
        "messages": [{"role": "user", "content": "Hello!"}],
    },
)
response.raise_for_status()
print(response.json())`;
}
