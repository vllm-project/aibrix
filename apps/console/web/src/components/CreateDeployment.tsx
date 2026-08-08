import { useEffect, useMemo, useState } from 'react';
import { ChevronLeft, Info, Rocket } from 'lucide-react';
import {
  createDeployment,
  listModelDeploymentTemplates,
  listModels,
} from '../utils/api';
import type { ModelDeploymentTemplate } from '../utils/api';
import type { Model } from '../data/mockData';

interface CreateDeploymentProps {
  onBack: () => void;
  onCreated?: (deploymentId: string) => void;
}

function positiveInt(value: string): number | null {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : null;
}

export function CreateDeployment({ onBack, onCreated }: CreateDeploymentProps) {
  const [deploymentName, setDeploymentName] = useState('');
  const [models, setModels] = useState<Model[]>([]);
  const [selectedModelId, setSelectedModelId] = useState('');
  const [templates, setTemplates] = useState<ModelDeploymentTemplate[]>([]);
  const [selectedTemplateId, setSelectedTemplateId] = useState('');
  const [region, setRegion] = useState('GLOBAL');
  const [enableAutoScaling, setEnableAutoScaling] = useState(false);
  const [minReplicas, setMinReplicas] = useState('1');
  const [maxReplicas, setMaxReplicas] = useState('1');
  const [loadingModels, setLoadingModels] = useState(true);
  const [loadingTemplates, setLoadingTemplates] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const selectedTemplate = useMemo(
    () => templates.find((template) => template.id === selectedTemplateId) ?? null,
    [templates, selectedTemplateId],
  );

  useEffect(() => {
    let cancelled = false;
    listModels()
      .then((items) => {
        if (cancelled) return;
        setModels(items);
        setSelectedModelId(items[0]?.id ?? '');
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : 'Failed to load models.');
        }
      })
      .finally(() => {
        if (!cancelled) setLoadingModels(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!selectedModelId) {
      setTemplates([]);
      setSelectedTemplateId('');
      return;
    }

    let cancelled = false;
    setTemplates([]);
    setSelectedTemplateId('');
    setLoadingTemplates(true);
    setError(null);
    listModelDeploymentTemplates(selectedModelId, 'active')
      .then((items) => {
        if (cancelled) return;
        setTemplates(items);
        setSelectedTemplateId(items[0]?.id ?? '');
      })
      .catch((err) => {
        if (!cancelled) {
          setTemplates([]);
          setSelectedTemplateId('');
          setError(err instanceof Error ? err.message : 'Failed to load deployment templates.');
        }
      })
      .finally(() => {
        if (!cancelled) setLoadingTemplates(false);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedModelId]);

  const handleDeploy = async () => {
    if (submitting) return;

    const name = deploymentName.trim();
    const min = positiveInt(minReplicas);
    const max = enableAutoScaling ? positiveInt(maxReplicas) : min;
    if (!name) {
      setError('Deployment name is required.');
      return;
    }
    if (!selectedModelId || !selectedTemplateId) {
      setError('Select an active deployment template.');
      return;
    }
    if (min === null || max === null) {
      setError('Replica counts must be positive integers.');
      return;
    }
    if (enableAutoScaling && max <= min) {
      setError('Max replicas must be greater than min replicas when auto scaling is enabled.');
      return;
    }

    setSubmitting(true);
    setError(null);
    try {
      const deployment = await createDeployment({
        name,
        template: {
          modelId: selectedModelId,
          templateId: selectedTemplateId,
        },
        implementation: {
          kind: 'kubernetes',
        },
        overrides: {
          region,
          minReplicas: min,
          maxReplicas: max,
          enableAutoScaling,
        },
      });
      onCreated?.(deployment.id);
      if (!onCreated) onBack();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create deployment.');
    } finally {
      setSubmitting(false);
    }
  };

  const spec = selectedTemplate?.spec;
  const accelerator = spec?.accelerator;
  const quantization = spec?.quantization;

  return (
    <div className="p-8">
      <button
        onClick={onBack}
        className="mb-4 flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900"
      >
        <ChevronLeft className="h-4 w-4" />
        Deployments / <span className="text-gray-400">Create</span>
      </button>
      <h1 className="mb-6 text-2xl">Create Deployment</h1>

      <div className="grid grid-cols-1 gap-6 xl:grid-cols-3">
        <div className="space-y-6 xl:col-span-2">
          {error && (
            <div className="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
              {error}
            </div>
          )}

          <section className="rounded-xl border border-gray-100 bg-white p-6 shadow-sm">
            <label className="mb-2 block text-sm">Deployment Name</label>
            <input
              type="text"
              value={deploymentName}
              onChange={(event) => setDeploymentName(event.target.value)}
              placeholder="my-model-deployment"
              className="w-full rounded-lg border border-gray-200 px-4 py-2 text-sm focus:border-teal-500 focus:outline-none focus:ring-2 focus:ring-teal-500/30"
            />
          </section>

          <section className="rounded-xl border border-gray-100 bg-white p-6 shadow-sm">
            <h2 className="mb-4">Deployment Template</h2>
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div>
                <label className="mb-2 flex items-center gap-2 text-sm">
                  Base Model <Info className="h-4 w-4 text-gray-400" />
                </label>
                <select
                  value={selectedModelId}
                  onChange={(event) => setSelectedModelId(event.target.value)}
                  disabled={loadingModels || models.length === 0}
                  className="w-full rounded-lg border border-gray-200 px-4 py-2 text-sm disabled:bg-gray-50"
                >
                  {loadingModels ? (
                    <option>Loading models...</option>
                  ) : models.length === 0 ? (
                    <option value="">No models available</option>
                  ) : (
                    models.map((model) => (
                      <option key={model.id} value={model.id}>{model.name}</option>
                    ))
                  )}
                </select>
              </div>

              <div>
                <label className="mb-2 flex items-center gap-2 text-sm">
                  Active Template <Info className="h-4 w-4 text-gray-400" />
                </label>
                <select
                  value={selectedTemplateId}
                  onChange={(event) => setSelectedTemplateId(event.target.value)}
                  disabled={loadingTemplates || templates.length === 0}
                  className="w-full rounded-lg border border-gray-200 px-4 py-2 text-sm disabled:bg-gray-50"
                >
                  {loadingTemplates ? (
                    <option>Loading templates...</option>
                  ) : templates.length === 0 ? (
                    <option value="">No active templates</option>
                  ) : (
                    templates.map((template) => (
                      <option key={template.id} value={template.id}>
                        {template.name} {template.version}
                      </option>
                    ))
                  )}
                </select>
              </div>
            </div>

            {selectedTemplate && (
              <div className="mt-5 grid grid-cols-2 gap-4 rounded-lg bg-gray-50 p-4 text-sm sm:grid-cols-4">
                <TemplateValue label="Engine" value={spec?.engine?.type || 'Not set'} />
                <TemplateValue
                  label="Accelerator"
                  value={accelerator?.type ? `${accelerator.type} x ${accelerator.count ?? 1}` : 'Not set'}
                />
                <TemplateValue label="Quantization" value={quantization?.weight?.toUpperCase() || 'Default'} />
                <TemplateValue label="Mode" value={spec?.deploymentMode || 'dedicated'} />
              </div>
            )}
          </section>

          <section className="rounded-xl border border-gray-100 bg-white p-6 shadow-sm">
            <h2 className="mb-4">Runtime Overrides</h2>
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div>
                <label className="mb-2 block text-sm">Region</label>
                <select
                  value={region}
                  onChange={(event) => setRegion(event.target.value)}
                  className="w-full rounded-lg border border-gray-200 px-4 py-2 text-sm"
                >
                  <option>GLOBAL</option>
                  <option>US Iowa 1</option>
                  <option>EU West</option>
                </select>
              </div>
              <label className="flex items-end gap-2 pb-2">
                <input
                  type="checkbox"
                  checked={enableAutoScaling}
                  onChange={(event) => setEnableAutoScaling(event.target.checked)}
                  className="h-4 w-4 rounded border-gray-300 text-teal-600 focus:ring-teal-500"
                />
                <span className="text-sm">Enable Auto Scaling</span>
              </label>
              <div>
                <label className="mb-2 block text-sm">Min Replicas</label>
                <input
                  type="number"
                  min={1}
                  value={minReplicas}
                  onChange={(event) => setMinReplicas(event.target.value)}
                  className="w-full rounded-lg border border-gray-200 px-4 py-2 text-sm"
                />
              </div>
              <div>
                <label className="mb-2 block text-sm">Max Replicas</label>
                <input
                  type="number"
                  min={1}
                  value={enableAutoScaling ? maxReplicas : minReplicas}
                  onChange={(event) => setMaxReplicas(event.target.value)}
                  disabled={!enableAutoScaling}
                  className="w-full rounded-lg border border-gray-200 px-4 py-2 text-sm disabled:bg-gray-50"
                />
              </div>
            </div>
          </section>

          <div className="flex justify-end gap-3">
            <button onClick={onBack} className="rounded-lg border border-gray-200 px-5 py-2.5 text-sm hover:bg-gray-50">
              Cancel
            </button>
            <button
              onClick={handleDeploy}
              disabled={submitting || loadingModels || loadingTemplates || !selectedTemplate}
              className="flex items-center gap-2 rounded-lg bg-teal-600 px-6 py-2.5 text-sm text-white hover:bg-teal-700 disabled:cursor-not-allowed disabled:bg-teal-300"
            >
              {submitting ? 'Deploying...' : 'Deploy'}
              <Rocket className="h-4 w-4" />
            </button>
          </div>
        </div>

        <aside className="space-y-6">
          <div className="rounded-xl border border-gray-100 bg-white p-6 shadow-sm">
            <h3 className="mb-3">Template-driven deployment</h3>
            <p className="text-sm leading-relaxed text-gray-500">
              Engine image, model source, accelerator, parallelism, quantization, and serving arguments come from the selected template.
            </p>
          </div>
          <div className="rounded-xl border border-teal-100 bg-teal-50 p-4">
            <div className="mb-2 text-sm text-teal-800">Implementation</div>
            <p className="text-sm text-gray-600">
              Kubernetes creates a Deployment, Service, and optional HorizontalPodAutoscaler.
            </p>
          </div>
        </aside>
      </div>
    </div>
  );
}

function TemplateValue({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="mb-1 text-xs text-gray-500">{label}</div>
      <div className="break-words text-gray-900">{value}</div>
    </div>
  );
}
