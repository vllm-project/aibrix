import { useEffect, useState } from 'react';
import { ChevronLeft, Copy, ExternalLink } from 'lucide-react';
import type { Deployment } from '../data/mockData';
import { getDeployment } from '../utils/api';
import { copyToClipboard } from '../utils/clipboard';
import {
  canOpenInPlayground,
  deploymentCodeExample,
  formatDeploymentCreatedAt,
  type DeploymentExampleLanguage,
} from '../utils/deploymentDetail';
import { deploymentStatusClass, normalizeDeploymentStatus } from '../utils/deploymentStatus';

interface DeploymentDetailProps {
  deploymentId: string | null;
  onBack: () => void;
  onOpenPlayground: (deployment: Deployment) => void;
}

export function DeploymentDetail({
  deploymentId,
  onBack,
  onOpenPlayground,
}: DeploymentDetailProps) {
  const [deployment, setDeployment] = useState<Deployment | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selectedLanguage, setSelectedLanguage] = useState<DeploymentExampleLanguage>('python');
  const [copied, setCopied] = useState<string | null>(null);

  useEffect(() => {
    if (!deploymentId) {
      setLoading(false);
      return;
    }

    let active = true;

    setLoading(true);
    setError(null);
    getDeployment(deploymentId)
      .then((item) => {
        if (active) setDeployment(item);
      })
      .catch((err: unknown) => {
        if (!active) return;

        setDeployment(null);
        setError(err instanceof Error ? err.message : 'Failed to load deployment');
      })
      .finally(() => {
        if (active) setLoading(false);
      });

    return () => {
      active = false;
    };
  }, [deploymentId]);

  const copy = (value: string, label: string) => {
    copyToClipboard(value).then(() => {
      setCopied(label);
      window.setTimeout(() => setCopied(null), 1500);
    }).catch(() => setCopied(null));
  };

  if (loading) {
    return (
      <div className="p-8">
        <BackButton onBack={onBack} />
        <p className="text-sm text-gray-500">Loading deployment...</p>
      </div>
    );
  }

  if (!deployment) {
    return (
      <div className="p-8">
        <BackButton onBack={onBack} />
        <p className="text-gray-900">Deployment not found</p>
        {error && <p className="text-sm text-red-600 mt-2">{error}</p>}
      </div>
    );
  }

  const deploymentStatus = normalizeDeploymentStatus(deployment.status);
  const playgroundReady = canOpenInPlayground(deployment);
  const code = deploymentCodeExample(deployment, selectedLanguage);

  return (
    <div className="p-8">
      <div className="mb-6">
        <BackButton onBack={onBack} />
        <div className="text-xs text-gray-400 mb-4">Deployments / {deployment.name}</div>

        <div className="flex items-start justify-between gap-6 mb-4">
          <div className="min-w-0">
            <h1 className="text-2xl text-gray-900 mb-2">{deployment.name}</h1>
            <div className="flex flex-wrap items-center gap-3">
              <code className="text-sm text-gray-700 bg-gray-100 px-2 py-1 rounded-md">
                {deployment.servingName || 'Serving name unavailable'}
              </code>
              {deployment.servingName && (
                <button
                  onClick={() => copy(deployment.servingName, 'serving-name')}
                  className="text-gray-400 hover:text-gray-700"
                  title="Copy serving name"
                  aria-label="Copy serving name"
                >
                  <Copy className="w-4 h-4" />
                </button>
              )}
              {copied === 'serving-name' && <span className="text-xs text-teal-600">Copied</span>}
              <span className={`inline-flex px-2.5 py-1 text-xs rounded-full ${deploymentStatusClass(deployment.status)}`}>
                {deploymentStatus}
              </span>
            </div>
          </div>

          <button
            onClick={() => onOpenPlayground(deployment)}
            disabled={!playgroundReady}
            title={playgroundReady ? 'Open this deployment in Playground' : 'The deployment must be Ready and have a serving name'}
            className="px-4 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50 flex items-center gap-2 disabled:cursor-not-allowed disabled:opacity-50"
          >
            Open in Playground
            <ExternalLink className="w-4 h-4" />
          </button>
        </div>

        <p className="text-sm text-gray-500">
          {deployment.baseModel} served through the AIBrix data plane.
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        <div className="lg:col-span-2 space-y-6">
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg text-gray-900 mb-2">Call this deployment</h2>
            <p className="text-sm text-gray-500 mb-5">
              Send an OpenAI-compatible chat completion request to the shared AIBrix Gateway.
            </p>

            <div className="flex items-center gap-2 mb-4">
              {(['python', 'shell'] as const).map((language) => (
                <button
                  key={language}
                  onClick={() => setSelectedLanguage(language)}
                  className={`px-3 py-1.5 text-sm rounded-lg capitalize transition-colors ${
                    selectedLanguage === language
                      ? 'bg-slate-800 text-white'
                      : 'text-gray-500 hover:bg-gray-100'
                  }`}
                >
                  {language}
                </button>
              ))}
            </div>

            <div className="relative bg-slate-900 rounded-xl p-4">
              <button
                onClick={() => copy(code, 'example')}
                className="absolute top-4 right-4 text-gray-400 hover:text-white"
                title="Copy example"
                aria-label="Copy API example"
              >
                <Copy className="w-4 h-4" />
              </button>
              {copied === 'example' && (
                <span className="absolute top-4 right-10 text-xs text-teal-300">Copied</span>
              )}
              <pre className="text-sm text-gray-300 overflow-x-auto pr-10">
                <code>{code}</code>
              </pre>
            </div>
          </div>

          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg text-gray-900 mb-4">Runtime deployment</h2>
            <div className="bg-gray-50 rounded-xl p-4">
              <div className="font-medium text-gray-900 mb-1">{deployment.name}</div>
              <div className="text-sm text-gray-500 mb-1">{deployment.deploymentId}</div>
              <div className="text-xs text-gray-400">
                {deployment.replicas || 'Unknown replicas'} · {deployment.gpuType || 'Accelerator not specified'}
              </div>
            </div>
          </div>
        </div>

        <div className="space-y-6">
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h2 className="text-lg text-gray-900 mb-4">Deployment details</h2>
            <dl className="space-y-4 text-sm">
              <Detail label="Status">
                <span className={`inline-flex px-2.5 py-1 text-xs rounded-full ${deploymentStatusClass(deployment.status)}`}>
                  {deploymentStatus}
                </span>
              </Detail>
              <Detail label="Base model">{deployment.baseModel || 'Not available'}</Detail>
              <Detail label="Serving name">
                <code className="break-all">{deployment.servingName || 'Not available'}</code>
              </Detail>
              <Detail label="Created by">{deployment.createdBy || 'Not available'}</Detail>
              <Detail label="Created at">{formatDeploymentCreatedAt(deployment.createdAt)}</Detail>
              <Detail label="Template">
                {[deployment.templateId, deployment.templateVersion].filter(Boolean).join(' · ') || 'Not available'}
              </Detail>
              <Detail label="Implementation">{deployment.implementationKind || 'Not available'}</Detail>
              <Detail label="Runtime resource">
                <code className="break-all">{deployment.deploymentId || 'Not available'}</code>
              </Detail>
            </dl>
          </div>
        </div>
      </div>
    </div>
  );
}

function BackButton({ onBack }: { onBack: () => void }) {
  return (
    <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
      <ChevronLeft className="w-4 h-4" />
      Back to deployments
    </button>
  );
}

function Detail({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-gray-500 mb-1">{label}</dt>
      <dd className="text-gray-900">{children}</dd>
    </div>
  );
}
