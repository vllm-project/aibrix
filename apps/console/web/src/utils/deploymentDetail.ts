import type { Deployment } from '../data/mockData';
import { normalizeDeploymentStatus } from './deploymentStatus';

export type DeploymentExampleLanguage = 'python' | 'shell';

export function canOpenInPlayground(deployment: Deployment): boolean {
  return normalizeDeploymentStatus(deployment.status) === 'Ready' && deployment.servingName.trim() !== '';
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
  const servingName = deployment.servingName || '<serving-name>';
  if (language === 'shell') {
    return `curl "$AIBRIX_GATEWAY_URL/v1/chat/completions" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${servingName}",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'`;
  }

  return `import os
import requests

response = requests.post(
    f"{os.environ['AIBRIX_GATEWAY_URL']}/v1/chat/completions",
    json={
        "model": "${servingName}",
        "messages": [{"role": "user", "content": "Hello!"}],
    },
)
response.raise_for_status()
print(response.json())`;
}
