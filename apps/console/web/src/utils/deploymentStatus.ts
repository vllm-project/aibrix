export function normalizeDeploymentStatus(status?: string): string {
  const value = (status || '').trim();
  if (!value) return 'Unknown';

  const normalized = value.toLowerCase();
  switch (normalized) {
    case 'ready':
      return 'Ready';
    case 'deploying':
      return 'Deploying';
    case 'scaling':
      return 'Scaling';
    case 'failed':
      return 'Failed';
    case 'degraded':
      return 'Degraded';
    case 'deleted':
      return 'Deleted';
    default:
      return value;
  }
}

export function deploymentStatusClass(status?: string): string {
  switch (normalizeDeploymentStatus(status)) {
    case 'Ready':
      return 'bg-emerald-50 text-emerald-700 border border-emerald-200';
    case 'Deploying':
    case 'Scaling':
      return 'bg-amber-50 text-amber-700 border border-amber-200';
    case 'Failed':
      return 'bg-red-50 text-red-700 border border-red-200';
    case 'Degraded':
      return 'bg-orange-50 text-orange-700 border border-orange-200';
    case 'Deleted':
      return 'bg-gray-100 text-gray-600 border border-gray-200';
    default:
      return 'bg-slate-100 text-slate-700 border border-slate-200';
  }
}
