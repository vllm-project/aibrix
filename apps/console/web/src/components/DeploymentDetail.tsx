import { useState, useEffect } from 'react';
import { ChevronLeft, Copy, ExternalLink } from 'lucide-react';
import { getDeployment } from '../utils/api';
import type { Deployment } from '../data/mockData';

interface DeploymentDetailProps {
  deploymentId: string | null;
  onBack: () => void;
}

export function DeploymentDetail({ deploymentId, onBack }: DeploymentDetailProps) {
  const [deployment, setDeployment] = useState<Deployment | null>(null);
  const [loading, setLoading] = useState(true);
  const [selectedLanguage, setSelectedLanguage] = useState<'python' | 'typescript' | 'java' | 'go' | 'shell' | 'chat' | 'completion'>('python');

  useEffect(() => {
    if (!deploymentId) {
      setLoading(false);
      return;
    }
    setLoading(true);
    getDeployment(deploymentId)
      .then(d => setDeployment(d))
      .catch(err => {
        console.error('Failed to fetch deployment:', err);
        setDeployment(null);
      })
      .finally(() => setLoading(false));
  }, [deploymentId]);

  if (loading) {
    return (
      <div className="p-8">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" />
          Back
        </button>
        <p className="text-sm text-gray-400">Loading deployment...</p>
      </div>
    );
  }

  if (!deployment) {
    return (
      <div className="p-8">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" />
          Back
        </button>
        <p>Deployment not found</p>
      </div>
    );
  }

  const pythonCode = `import requests
import json

url = "https://api.aibrix.ai/inference/v1/chat/completions"

payload = {
    "model": "accounts/seedjeffwan-2hvzrk1/deployments/euxdnr5z",
}`;

  return (
    <div className="p-8">
      <div className="mb-6">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" />
          Accounts / <span className="text-gray-400">seedjeffwan-2hvzrk1</span> / <span className="text-gray-400">DeployedModels</span> / <span className="text-gray-400">{deployment.baseModel.toLowerCase().replace(/\s+/g, '-')}-or1ddd6b</span>
        </button>

        <div className="flex items-center justify-between mb-4">
          <div>
            <h1 className="text-2xl mb-2">{deployment.baseModel.toLowerCase().replace(/\s+/g, '-')}-or1...</h1>
            <div className="flex items-center gap-3">
              <code className="text-sm text-gray-600 bg-gray-100 px-2 py-1 rounded-md">
                accounts/seedjeffwan-2hvzrk1/deployedModels/{deployment.baseModel.toLowerCase().replace(/\s+/g, '-')}-8b-or1ddd6b
              </code>
              <button className="text-gray-400 hover:text-gray-600">
                <Copy className="w-4 h-4" />
              </button>
              <span className="inline-flex px-2.5 py-1 text-xs rounded-full bg-emerald-50 text-emerald-700 border border-emerald-200">
                Deployed
              </span>
            </div>
          </div>

          <button className="px-4 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50 flex items-center gap-2">
            Open in Playground
            <ExternalLink className="w-4 h-4" />
          </button>
        </div>

        <p className="text-sm text-gray-500">
          Llama 8B distilled with reasoning from Deepseek R1
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        <div className="lg:col-span-2 space-y-6">
          {/* About Deployed Models */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg mb-4">About Deployed Models</h2>
            <p className="text-sm text-gray-500 mb-4">
              Deployed models are models that are deployed to a deployment and are ready to be used for inference. Creating a dedicated deployment will automatically create a deployed model for the base model used. Low-rank adaptation (LoRA) models may also be deployed to dedicated deployments.{' '}
              <a href="#" className="text-teal-600 hover:underline">Docs ↗</a>
            </p>

            <h3 className="mb-3">API Examples</h3>

            <div className="mb-4">
              <div className="flex items-center gap-2 overflow-x-auto pb-2">
                {['Python', 'Typescript', 'Java', 'Go', 'Shell', 'Chat', 'Completion'].map((lang) => (
                  <button
                    key={lang}
                    onClick={() => setSelectedLanguage(lang.toLowerCase() as any)}
                    className={`px-3 py-1.5 text-sm rounded-lg whitespace-nowrap transition-colors ${
                      selectedLanguage === lang.toLowerCase()
                        ? 'bg-slate-800 text-white'
                        : 'text-gray-500 hover:bg-gray-100'
                    }`}
                  >
                    {lang}
                  </button>
                ))}
              </div>
            </div>

            <div className="relative bg-slate-900 rounded-xl p-4">
              <button className="absolute top-4 right-4 text-gray-400 hover:text-white">
                <Copy className="w-4 h-4" />
              </button>
              <pre className="text-sm text-gray-300 overflow-x-auto">
                <code>{pythonCode}</code>
              </pre>
            </div>

            <div className="mt-4 text-center">
              <button className="text-sm text-teal-600 hover:underline">
                View More
              </button>
            </div>
          </div>

          {/* Active Deployments */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg mb-4">Active Deployments</h2>
            <p className="text-sm text-gray-500 mb-4">
              Your deployment of <strong>{deployment.baseModel.toLowerCase()}</strong> are listed below.
            </p>

            <div className="bg-gray-50 rounded-xl p-4">
              <h3 className="mb-1">Active Deployments</h3>
              <div className="text-sm text-gray-500 mb-1">{deployment.deploymentId}</div>
              <div className="text-xs text-gray-400">
                {deployment.baseModel.toLowerCase().replace(/\s+/g, '-')}-8b-or1ddd6b • Private
              </div>
            </div>
          </div>
        </div>

        {/* Right Sidebar */}
        <div className="space-y-6">
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg mb-4">Deployed Model Details</h2>

            <div className="space-y-3 text-sm">
              <div>
                <div className="text-gray-500 mb-1">Status</div>
                <span className="inline-flex px-2.5 py-1 text-xs rounded-full bg-emerald-50 text-emerald-700 border border-emerald-200">
                  Deployed
                </span>
              </div>

              <div>
                <div className="text-gray-500 mb-1">Deployed by</div>
                <div className="text-gray-900">{deployment.createdBy}</div>
              </div>

              <div>
                <div className="text-gray-500 mb-1">Deployment time</div>
                <div className="text-gray-900">Sunday, January 18, 2026 at 11:53:06 PM UTC</div>
              </div>

              <div>
                <div className="text-gray-500 mb-1">Model name</div>
                <div className="flex items-center gap-2">
                  <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">
                    accounts/seedjeffwan-2hvzrk1/deployedModels/deepseek-r1-distil-llama-8b-or1ddd6b
                  </code>
                </div>
              </div>

              <div>
                <div className="text-gray-500 mb-1">Deployment</div>
                <div className="flex items-center gap-2">
                  <code className="text-sm bg-gray-100 px-2 py-1 rounded-md">
                    accounts/seedjeffwan-2hvzrk1/deployments/euxdnr5z
                  </code>
                </div>
              </div>
            </div>
          </div>

          <div className="bg-teal-50 rounded-xl p-4 border border-teal-100">
            <div className="text-sm mb-2 text-teal-800">Documentation</div>
            <p className="text-sm text-gray-600">
              Learn more about deployments and how to use them in our{' '}
              <a href="#" className="text-teal-600 hover:underline">documentation</a>.
            </p>
          </div>
        </div>
      </div>
    </div>
  );
}
