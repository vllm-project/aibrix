import { useState } from 'react';
import { X, Copy } from 'lucide-react';
import { mockDeployments } from '../data/mockData';

interface DeleteDeploymentModalProps {
  deploymentId: string;
  onClose: () => void;
}

export function DeleteDeploymentModal({ deploymentId, onClose }: DeleteDeploymentModalProps) {
  const deployment = mockDeployments.find(d => d.id === deploymentId);
  const [confirmText, setConfirmText] = useState('');
  const [hardDelete, setHardDelete] = useState(false);
  const [ignoreChecks, setIgnoreChecks] = useState(false);

  if (!deployment) return null;

  const canConfirm = confirmText === deployment.deploymentId && (hardDelete || ignoreChecks);

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50">
      <div className="bg-white rounded-xl max-w-lg w-full mx-4 shadow-2xl">
        <div className="flex items-center justify-between p-6 border-b border-gray-100">
          <h2 className="text-xl">Delete Deployment</h2>
          <button onClick={onClose} className="text-gray-400 hover:text-gray-600">
            <X className="w-5 h-5" />
          </button>
        </div>

        <div className="p-6">
          <div className="mb-4">
            <div className="text-lg mb-2">{deployment.deploymentId}</div>
            <div className="inline-flex px-2.5 py-1 text-xs rounded-full bg-amber-50 text-amber-700 border border-amber-200 uppercase">
              {deployment.baseModel}
            </div>
          </div>

          <div className="mb-6">
            <p className="text-sm text-gray-600 mb-4">
              Enter the deployment ID:{' '}
              <span className="inline-flex items-center gap-1 bg-gray-100 px-2 py-0.5 rounded-md text-sm font-mono">
                {deployment.deploymentId}
                <button className="text-gray-400 hover:text-gray-600">
                  <Copy className="w-3 h-3" />
                </button>
              </span>{' '}
              in the input below and click to confirm deletion.
            </p>

            <div className="space-y-3 mb-4">
              <label className="flex items-start gap-2">
                <input
                  type="checkbox"
                  checked={hardDelete}
                  onChange={(e) => setHardDelete(e.target.checked)}
                  className="mt-1 w-4 h-4 text-teal-600 rounded focus:ring-teal-500 border-gray-300"
                />
                <div className="text-sm">
                  <div>Hard Delete</div>
                  <div className="text-gray-500">
                    Hard Delete permanently removes the deployment and all associated resources without the possibility of recovery.
                  </div>
                </div>
              </label>

              <label className="flex items-start gap-2">
                <input
                  type="checkbox"
                  checked={ignoreChecks}
                  onChange={(e) => setIgnoreChecks(e.target.checked)}
                  className="mt-1 w-4 h-4 text-teal-600 rounded focus:ring-teal-500 border-gray-300"
                />
                <div className="text-sm">
                  <div>Ignore Checks</div>
                  <div className="text-gray-500">
                    Ignore Checks bypasses safety validations that would normally prevent deletion if the deployment is in use (or has been in the past 1 hour) or has dependencies.
                  </div>
                </div>
              </label>
            </div>

            <input
              type="text"
              value={confirmText}
              onChange={(e) => setConfirmText(e.target.value)}
              placeholder={deployment.deploymentId}
              className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
            />
          </div>

          <div className="flex items-center gap-3">
            <button
              onClick={onClose}
              className="px-4 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50"
            >
              Back
            </button>
            <button
              disabled={!canConfirm}
              className={`flex-1 px-4 py-2 rounded-lg text-sm ${
                canConfirm
                  ? 'bg-red-600 text-white hover:bg-red-700'
                  : 'bg-gray-100 text-gray-400 cursor-not-allowed'
              }`}
            >
              Confirm Deletion
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
