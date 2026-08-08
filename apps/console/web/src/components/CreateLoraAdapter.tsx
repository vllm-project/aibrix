import { useEffect, useMemo, useState } from 'react';
import type { FormEvent } from 'react';
import {
  Check,
  ChevronLeft,
  LoaderCircle,
  Plus,
} from 'lucide-react';
import {
  createModelAdapter,
  listModelAdapterTargets,
} from '../utils/api';
import type {
  ModelAdapter,
  ModelAdapterPlacement,
  ModelAdapterTarget,
} from '../utils/api';

interface CreateLoraAdapterProps {
  onBack: () => void;
  onCreated: (name: string) => void;
}

const modelAdapterNamePattern = /^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$/;
const modelAdapterNameMaxLength = 63;

export function CreateLoraAdapter({ onBack, onCreated }: CreateLoraAdapterProps) {
  const [name, setName] = useState('');
  const [artifactUrl, setArtifactUrl] = useState('');
  const [targets, setTargets] = useState<ModelAdapterTarget[]>([]);
  const [selectedTargetName, setSelectedTargetName] = useState('');
  const [placement, setPlacement] = useState<ModelAdapterPlacement>('all');
  const [loadingTargets, setLoadingTargets] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [nameError, setNameError] = useState<string | null>(null);
  const [artifactError, setArtifactError] = useState<string | null>(null);
  const [createdAdapter, setCreatedAdapter] = useState<ModelAdapter | null>(null);

  useEffect(() => {
    let cancelled = false;
    listModelAdapterTargets()
      .then((items) => {
        if (cancelled) return;
        setTargets(items);
        setSelectedTargetName(items[0]?.name ?? '');
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : 'Failed to discover Kubernetes deployments.');
        }
      })
      .finally(() => {
        if (!cancelled) setLoadingTargets(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const selectedTarget = useMemo(
    () => targets.find((target) => target.name === selectedTargetName) ?? null,
    [selectedTargetName, targets],
  );

  const targetPods = selectedTarget
    ? placement === 'all'
      ? selectedTarget.readyReplicas
      : Math.min(1, selectedTarget.readyReplicas)
    : 0;

  const handleSubmit = async (event: FormEvent) => {
    event.preventDefault();
    if (submitting) return;

    const normalizedName = name.trim();
    const normalizedArtifactUrl = artifactUrl.trim();
    const nextNameError = !normalizedName ||
      normalizedName.length > modelAdapterNameMaxLength ||
      !modelAdapterNamePattern.test(normalizedName)
      ? 'Use lowercase letters, numbers, dots, and hyphens.'
      : null;
    const nextArtifactError = normalizedArtifactUrl
      ? null
      : 'Enter a supported LoRA artifact URL.';
    setNameError(nextNameError);
    setArtifactError(nextArtifactError);
    if (nextNameError || nextArtifactError || !selectedTarget) return;

    setSubmitting(true);
    setError(null);
    try {
      const adapter = await createModelAdapter({
        name: normalizedName,
        artifactUrl: normalizedArtifactUrl,
        deploymentName: selectedTarget.name,
        placement,
      });
      setCreatedAdapter(adapter);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create ModelAdapter.');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <>
      <div className="mx-auto w-full max-w-6xl p-8">
        <button
          type="button"
          onClick={onBack}
          className="mb-4 inline-flex items-center gap-1 text-sm text-gray-500 hover:text-gray-900"
        >
          <ChevronLeft className="h-4 w-4" />
          LoRA deployments
        </button>

        <div className="mb-6">
          <div className="mb-1 text-xs font-semibold uppercase tracking-widest text-teal-700">
            Create ModelAdapter
          </div>
          <h1 className="mb-1 text-3xl font-semibold tracking-tight text-gray-900">
            Deploy a LoRA adapter
          </h1>
          <p className="text-sm text-gray-500">
            Provide the adapter artifact and select the existing Kubernetes deployment that will load it.
          </p>
        </div>

        <form onSubmit={handleSubmit} className="grid grid-cols-1 items-start gap-6 xl:grid-cols-3">
          <section className="rounded-xl border border-gray-200 bg-white p-6 shadow-sm xl:col-span-2">
            {error && (
              <div className="mb-5 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
                {error}
              </div>
            )}

            <div className="border-b border-gray-100 pb-6">
              <div className="mb-5">
                <h2 className="mb-1 text-lg font-semibold text-gray-900">Adapter</h2>
                <p className="text-sm text-gray-500">
                  Only the public model ID and artifact URL are required.
                </p>
              </div>
              <div className="mb-5">
                <label className="mb-2 block text-sm font-medium text-gray-700">
                  LoRA model ID <span className="text-red-600">*</span>
                </label>
                <input
                  value={name}
                  maxLength={modelAdapterNameMaxLength}
                  onChange={(event) => {
                    setName(event.target.value);
                    setNameError(null);
                  }}
                  placeholder="sql-assistant"
                  className={`w-full rounded-lg border px-4 py-2.5 text-sm focus:outline-none focus:ring-2 ${
                    nameError
                      ? 'border-red-300 focus:ring-red-200'
                      : 'border-gray-200 focus:border-teal-500 focus:ring-teal-500/20'
                  }`}
                />
                <p className="mt-2 text-xs text-gray-400">
                  This becomes the ModelAdapter name and inference model ID.
                </p>
                {nameError && <p className="mt-1 text-xs text-red-600">{nameError}</p>}
              </div>
              <div>
                <label className="mb-2 block text-sm font-medium text-gray-700">
                  LoRA artifact URL <span className="text-red-600">*</span>
                </label>
                <input
                  value={artifactUrl}
                  onChange={(event) => {
                    setArtifactUrl(event.target.value);
                    setArtifactError(null);
                  }}
                  placeholder="huggingface://organization/adapter"
                  className={`w-full rounded-lg border px-4 py-2.5 text-sm focus:outline-none focus:ring-2 ${
                    artifactError
                      ? 'border-red-300 focus:ring-red-200'
                      : 'border-gray-200 focus:border-teal-500 focus:ring-teal-500/20'
                  }`}
                />
                <p className="mt-2 text-xs text-gray-400">
                  Use a controller-supported URL such as huggingface://, s3://, gcs://, tos://, or an absolute mounted path.
                </p>
                {artifactError && <p className="mt-1 text-xs text-red-600">{artifactError}</p>}
              </div>
            </div>

            <div className="border-b border-gray-100 py-6">
              <div className="mb-5">
                <h2 className="mb-1 text-lg font-semibold text-gray-900">Base model deployment</h2>
                <p className="text-sm text-gray-500">
                  Select an existing Kubernetes Deployment. AIBrix does not create the base workload in this workflow.
                </p>
              </div>

              {loadingTargets ? (
                <div className="rounded-lg border border-gray-200 px-4 py-8 text-center text-sm text-gray-400">
                  Discovering Kubernetes deployments...
                </div>
              ) : targets.length === 0 ? (
                <div className="rounded-lg border border-amber-200 bg-amber-50 px-4 py-4 text-sm text-amber-700">
                  No compatible Kubernetes Deployments were found in the configured namespace.
                </div>
              ) : (
                <div className="space-y-2">
                  {targets.map((target) => {
                    const selected = target.name === selectedTargetName;
                    return (
                      <button
                        key={target.name}
                        type="button"
                        onClick={() => setSelectedTargetName(target.name)}
                        className={`w-full rounded-lg border p-4 text-left transition-colors ${
                          selected
                            ? 'border-teal-400 bg-teal-50/60 ring-2 ring-teal-100'
                            : 'border-gray-200 hover:border-teal-200'
                        }`}
                      >
                        <div className="flex items-start gap-3">
                          <span className={`mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded-full border ${
                            selected ? 'border-teal-700' : 'border-gray-300'
                          }`}>
                            {selected && <span className="h-2 w-2 rounded-full bg-teal-700" />}
                          </span>
                          <div className="min-w-0 flex-1">
                            <div className="truncate text-sm font-semibold text-gray-900">
                              {target.name}
                            </div>
                            <div className="mt-1 truncate text-xs text-gray-500">
                              {target.baseModel} · {target.engine}
                            </div>
                          </div>
                          <span className="rounded-md border border-gray-200 bg-gray-50 px-2 py-1 text-xs font-medium text-gray-600">
                            {target.kind}
                          </span>
                        </div>
                        <div className="ml-7 mt-3 flex gap-5 border-t border-gray-100 pt-3 text-xs text-gray-500">
                          <span>{target.readyReplicas}/{target.desiredReplicas} ready</span>
                          <span>{target.apiVersion}</span>
                        </div>
                      </button>
                    );
                  })}
                </div>
              )}
            </div>

            <div className="pt-6">
              <div className="mb-5">
                <h2 className="mb-1 text-lg font-semibold text-gray-900">Placement</h2>
                <p className="text-sm text-gray-500">
                  This maps directly to ModelAdapter <code className="rounded bg-gray-100 px-1.5 py-0.5 text-xs">spec.replicas</code>.
                </p>
              </div>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <PlacementOption
                  selected={placement === 'all'}
                  title="All matching Pods"
                  description={`Omit replicas. Load onto all ${selectedTarget?.readyReplicas ?? 0} healthy Pods and follow deployment scale-out.`}
                  onClick={() => setPlacement('all')}
                />
                <PlacementOption
                  selected={placement === 'single'}
                  title="Single Pod"
                  description="Set replicas to 1 and let the ModelAdapter scheduler select one Pod."
                  onClick={() => setPlacement('single')}
                />
              </div>
            </div>

            <div className="mt-6 flex justify-end gap-3 border-t border-gray-100 pt-5">
              <button
                type="button"
                onClick={onBack}
                className="rounded-lg border border-gray-200 px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
              >
                Cancel
              </button>
              <button
                type="submit"
                disabled={submitting || !selectedTarget}
                className="inline-flex items-center gap-2 rounded-lg bg-teal-700 px-4 py-2.5 text-sm font-medium text-white hover:bg-teal-800 disabled:cursor-not-allowed disabled:opacity-50"
              >
                {submitting ? (
                  <LoaderCircle className="h-4 w-4 animate-spin" />
                ) : (
                  <Plus className="h-4 w-4" />
                )}
                Create ModelAdapter
              </button>
            </div>
          </section>

          <aside className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm">
            <h2 className="mb-4 text-base font-semibold text-gray-900">ModelAdapter preview</h2>
            <PreviewRow label="Name" value={name || '-'} />
            <PreviewRow label="Artifact URL" value={artifactUrl || '-'} />
            <PreviewRow label="Base model" value={selectedTarget?.baseModel || '-'} />
            <PreviewRow
              label="Base deployment"
              value={selectedTarget ? `Deployment / ${selectedTarget.name}` : '-'}
            />
            <PreviewRow label="Target Pods" value={String(targetPods)} />
            <div className="mt-4 rounded-lg border border-teal-200 bg-teal-50 px-3 py-3 text-xs leading-relaxed text-teal-800">
              The UI derives <strong>baseModel</strong>, <strong>podSelector</strong>, and runtime targets from the selected Kubernetes Deployment.
            </div>
          </aside>
        </form>
      </div>

      {(submitting || createdAdapter) && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-900/50 p-6 backdrop-blur-sm">
          <div className="w-full max-w-md overflow-hidden rounded-xl border border-gray-200 bg-white shadow-2xl">
            <div className="flex items-center gap-4 p-6">
              {createdAdapter ? (
                <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-green-600 text-white">
                  <Check className="h-5 w-5" />
                </span>
              ) : (
                <LoaderCircle className="h-9 w-9 shrink-0 animate-spin text-teal-700" />
              )}
              <div>
                <div className="font-semibold text-gray-900">
                  {createdAdapter ? 'ModelAdapter created' : 'Creating ModelAdapter'}
                </div>
                <div className="mt-1 text-sm text-gray-500">
                  {createdAdapter
                    ? `${createdAdapter.name} is ${createdAdapter.phase.toLowerCase()} on ${selectedTarget?.name}.`
                    : `Submitting ${name || 'the requested adapter'} to Kubernetes.`}
                </div>
              </div>
            </div>
            <div className="flex justify-end border-t border-gray-100 bg-gray-50 px-5 py-3">
              <button
                type="button"
                disabled={!createdAdapter}
                onClick={() => createdAdapter && onCreated(createdAdapter.name)}
                className="rounded-lg bg-teal-700 px-4 py-2 text-sm font-medium text-white hover:bg-teal-800 disabled:opacity-50"
              >
                View ModelAdapter
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

function PlacementOption({
  selected,
  title,
  description,
  onClick,
}: {
  selected: boolean;
  title: string;
  description: string;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`min-h-24 rounded-lg border p-4 text-left ${
        selected
          ? 'border-teal-400 bg-teal-50/60 ring-2 ring-teal-100'
          : 'border-gray-200 hover:border-teal-200'
      }`}
    >
      <div className="flex items-center gap-3">
        <span className={`flex h-4 w-4 shrink-0 items-center justify-center rounded-full border ${
          selected ? 'border-teal-700' : 'border-gray-300'
        }`}>
          {selected && <span className="h-2 w-2 rounded-full bg-teal-700" />}
        </span>
        <span className="text-sm font-semibold text-gray-900">{title}</span>
      </div>
      <p className="ml-7 mt-2 text-xs leading-relaxed text-gray-500">{description}</p>
    </button>
  );
}

function PreviewRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid grid-cols-[7rem_minmax(0,1fr)] gap-2 border-b border-gray-100 py-3 text-xs last:border-0">
      <span className="text-gray-400">{label}</span>
      <span className="truncate font-medium text-gray-700">{value}</span>
    </div>
  );
}
