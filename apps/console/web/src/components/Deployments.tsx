import { useState } from 'react';
import { Search, MoreVertical } from 'lucide-react';
import { mockDeployments } from '../data/mockData';
import { DeleteDeploymentModal } from './DeleteDeploymentModal';

interface DeploymentsProps {
  onSelectDeployment: (id: string) => void;
  onCreateDeployment: () => void;
}

export function Deployments({ onSelectDeployment, onCreateDeployment }: DeploymentsProps) {
  const [selectedTab, setSelectedTab] = useState<'on-demand' | 'lora'>('on-demand');
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);
  const [deploymentToDelete, setDeploymentToDelete] = useState<string | null>(null);

  const handleDelete = (deploymentId: string) => {
    setDeploymentToDelete(deploymentId);
    setDeleteModalOpen(true);
  };

  return (
    <div className="p-8">
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl mb-2">Deployments</h1>
          <p className="text-sm text-gray-500">
            Manage your on-demand deployments and serverless deployed models.{' '}
            <a href="#" className="text-teal-600 hover:underline">Learn more</a>
          </p>
        </div>
        <button
          onClick={onCreateDeployment}
          className="px-4 py-2 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors"
        >
          Create Deployment
        </button>
      </div>

      <div className="mb-6 flex items-center gap-4">
        <div className="flex-1 relative">
          <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
          <input
            type="text"
            placeholder="Search by name, model, or created by"
            className="w-full pl-10 pr-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 bg-white"
          />
        </div>
        
        <div className="flex items-center bg-gray-100 rounded-lg p-0.5">
          <button
            onClick={() => setSelectedTab('on-demand')}
            className={`px-4 py-1.5 rounded-md text-sm transition-colors ${
              selectedTab === 'on-demand'
                ? 'bg-white text-gray-900 shadow-sm'
                : 'text-gray-500 hover:text-gray-700'
            }`}
          >
            On-demand
          </button>
          <button
            onClick={() => setSelectedTab('lora')}
            className={`px-4 py-1.5 rounded-md text-sm transition-colors ${
              selectedTab === 'lora'
                ? 'bg-white text-gray-900 shadow-sm'
                : 'text-gray-500 hover:text-gray-700'
            }`}
          >
            LoRA Addons
          </button>
        </div>
      </div>

      <div className="bg-white rounded-xl shadow-sm border border-gray-100 overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full">
            <thead className="bg-gray-50/80 border-b border-gray-100">
              <tr>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Name</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Base model</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider"># Replicas</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">GPUs/Replica</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">GPU Type</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Region</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Created by</th>
                <th className="px-6 py-3 text-left text-xs text-gray-500 uppercase tracking-wider">Status</th>
                <th className="px-6 py-3"></th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-50">
              {mockDeployments.map((deployment) => (
                <tr
                  key={deployment.id}
                  className="hover:bg-gray-50/50 cursor-pointer transition-colors"
                  onClick={() => onSelectDeployment(deployment.id)}
                >
                  <td className="px-6 py-4">
                    <div className="flex items-center gap-2">
                      <div className="w-2 h-2 rounded-full bg-emerald-500"></div>
                      <div>
                        <div className="text-sm text-gray-900">{deployment.name}</div>
                        <div className="text-xs text-gray-400">ID: {deployment.deploymentId}</div>
                      </div>
                    </div>
                  </td>
                  <td className="px-6 py-4">
                    <div className="text-sm text-gray-900">{deployment.baseModel}</div>
                    <div className="text-xs text-gray-400">ID: {deployment.baseModelId}</div>
                  </td>
                  <td className="px-6 py-4 text-sm text-gray-500">{deployment.replicas}</td>
                  <td className="px-6 py-4 text-sm text-gray-500">{deployment.gpusPerReplica}</td>
                  <td className="px-6 py-4 text-sm text-gray-500">{deployment.gpuType}</td>
                  <td className="px-6 py-4 text-sm text-gray-500">{deployment.region}</td>
                  <td className="px-6 py-4 text-sm text-gray-500">{deployment.createdBy}</td>
                  <td className="px-6 py-4">
                    <span className="inline-flex px-2.5 py-1 text-xs rounded-full bg-emerald-50 text-emerald-700 border border-emerald-200">
                      {deployment.status}
                    </span>
                  </td>
                  <td className="px-6 py-4">
                    <button
                      onClick={(e) => {
                        e.stopPropagation();
                        handleDelete(deployment.id);
                      }}
                      className="text-gray-300 hover:text-gray-500"
                    >
                      <MoreVertical className="w-5 h-5" />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {deleteModalOpen && deploymentToDelete && (
        <DeleteDeploymentModal
          deploymentId={deploymentToDelete}
          onClose={() => {
            setDeleteModalOpen(false);
            setDeploymentToDelete(null);
          }}
        />
      )}
    </div>
  );
}
