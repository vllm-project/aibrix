import { getDeploymentDetailRenderer } from './registry';
import type { Job, JobDeploymentDetail } from '../../data/mockData';

export type { Job };

export function DeploymentDetailCard({
  detail,
  onRefresh,
}: {
  detail: JobDeploymentDetail;
  onRefresh?: () => Promise<Job | null>;
}) {
  const Renderer = getDeploymentDetailRenderer(detail.type);
  if (!Renderer) {
    return (
      <p className="text-sm text-gray-500">
        No renderer for deployment type: {detail.type || 'unknown'}
      </p>
    );
  }
  return <Renderer data={detail} onRefresh={onRefresh} />;
}
