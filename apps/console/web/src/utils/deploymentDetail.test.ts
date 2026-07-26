import { describe, expect, it } from 'vitest';
import type { Deployment } from '../data/mockData';
import {
  canOpenInPlayground,
  deploymentCodeExample,
  formatDeploymentCreatedAt,
} from './deploymentDetail';

const deployment: Deployment = {
  id: 'deployment-1',
  name: 'demo',
  deploymentId: 'runtime-demo',
  baseModel: 'Mock Model',
  baseModelId: 'model-1',
  servingName: '/models/mock',
  createdAt: '2026-07-26T08:30:00Z',
  replicas: '1',
  gpusPerReplica: 0,
  gpuType: 'CPU',
  region: '',
  createdBy: 'owner@example.com',
  status: 'Ready',
};

describe('deployment detail helpers', () => {
  it('allows only ready deployments with a serving name in Playground', () => {
    expect(canOpenInPlayground(deployment)).toBe(true);
    expect(canOpenInPlayground({ ...deployment, status: 'Deploying' })).toBe(false);
    expect(canOpenInPlayground({ ...deployment, servingName: '' })).toBe(false);
  });

  it('builds examples from the real serving name and shared gateway endpoint', () => {
    const shell = deploymentCodeExample(deployment, 'shell');
    expect(shell).toContain('$AIBRIX_GATEWAY_URL/v1/chat/completions');
    expect(shell).toContain('"model": "/models/mock"');
    expect(shell).not.toContain('seedjeffwan');
  });

  it('formats the API timestamp without inventing a date', () => {
    expect(formatDeploymentCreatedAt(deployment.createdAt)).toContain('2026');
    expect(formatDeploymentCreatedAt('')).toBe('Not available');
  });
});
