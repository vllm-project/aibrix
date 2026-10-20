import { useEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import {
  AlertTriangle,
  Check,
  ChevronLeft,
  Copy,
  LoaderCircle,
  Plus,
  Trash2,
} from 'lucide-react';
import { createModelClaim, listModelClaimPools } from '../utils/api';
import type { ModelClaim, ModelClaimEngine, ModelClaimPool } from '../utils/api';
import { copyToClipboard } from '../utils/clipboard';
import {
  buildModelClaimYaml,
  formatPoolGpus,
  MODEL_CLAIM_LAUNCHER_FLAGS,
  MODEL_CLAIM_NAME_MAX_LENGTH,
  validateArtifactUrl,
  validateEngineArgs,
  validateModelClaimName,
  validatePerGpuFigure,
  validatePriority,
  validateServedModelName,
} from '../utils/modelClaim';
import type { EngineArgRow } from '../utils/modelClaim';

interface CreateModelClaimProps {
  onBack: () => void;
  onCreated: (name: string) => void;
}

const inputClass = (invalid: boolean) => `w-full rounded-lg border px-4 py-2.5 text-sm focus:outline-none focus:ring-2 ${
  invalid
    ? 'border-red-300 focus:ring-red-200'
    : 'border-gray-200 focus:border-teal-500 focus:ring-teal-500/20'
}`;

export function CreateModelClaim({ onBack, onCreated }: CreateModelClaimProps) {
  const [name, setName] = useState('');
  const [modelName, setModelName] = useState('');
  const [artifactUrl, setArtifactUrl] = useState('');
  const [engine, setEngine] = useState<ModelClaimEngine>('vllm');
  const [pools, setPools] = useState<ModelClaimPool[]>([]);
  const [selectedPoolName, setSelectedPoolName] = useState('');
  const [maximumFootprint, setMaximumFootprint] = useState('');
  const [kvFloor, setKvFloor] = useState('');
  const [priority, setPriority] = useState('');
  // Each row keeps the id it was added with, so React keys stay stable when a
  // row above it is removed.
  const [argRows, setArgRows] = useState<Array<EngineArgRow & { id: number }>>([]);
  const nextArgRowId = useRef(0);
  const [loadingPools, setLoadingPools] = useState(true);
  const [submitted, setSubmitted] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [createdClaim, setCreatedClaim] = useState<ModelClaim | null>(null);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    let cancelled = false;
    listModelClaimPools()
      .then((items) => {
        if (cancelled) return;
        setPools(items);
        setSelectedPoolName(items.find((pool) => pool.readyPods > 0)?.name ?? items[0]?.name ?? '');
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : 'Failed to discover warm runtime pools.');
        }
      })
      .finally(() => {
        if (!cancelled) setLoadingPools(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const selectedPool = useMemo(
    () => pools.find((pool) => pool.name === selectedPoolName) ?? null,
    [pools, selectedPoolName],
  );
  const engineArgs = useMemo(
    () => validateEngineArgs(argRows, engine, selectedPool),
    [argRows, engine, selectedPool],
  );
  const fieldErrors = {
    name: validateModelClaimName(name),
    modelName: validateServedModelName(modelName),
    artifactUrl: validateArtifactUrl(artifactUrl),
    maximumFootprint: validatePerGpuFigure(maximumFootprint),
    kvFloor: validatePerGpuFigure(kvFloor),
    priority: validatePriority(priority),
  };
  const shownError = (field: keyof typeof fieldErrors) => (submitted ? fieldErrors[field] : null);
  const yaml = buildModelClaimYaml({
    name,
    modelName,
    artifactUrl,
    engine,
    poolName: selectedPoolName,
    maximumFootprint,
    kvFloor,
    priority,
    args: engineArgs.args,
  }, selectedPool?.namespace);

  const updateRow = (index: number, patch: Partial<EngineArgRow>) => {
    setArgRows((rows) => rows.map((row, i) => (i === index ? { ...row, ...patch } : row)));
  };

  const addRow = () => {
    const id = nextArgRowId.current;
    nextArgRowId.current += 1;
    setArgRows((rows) => [...rows, { id, key: '', value: '' }]);
  };

  const handleSubmit = async (event: FormEvent) => {
    event.preventDefault();
    if (submitting) return;
    setSubmitted(true);
    const invalid = Object.values(fieldErrors).some(Boolean) ||
      engineArgs.rowErrors.some(Boolean) ||
      engineArgs.topologyError !== null;
    if (invalid || !selectedPool) return;

    setSubmitting(true);
    setError(null);
    try {
      const claim = await createModelClaim({
        name: name.trim(),
        modelName: modelName.trim() || undefined,
        artifactUrl: artifactUrl.trim(),
        engine,
        poolName: selectedPool.name,
        perGpu: { maximumFootprint: maximumFootprint.trim(), kvFloor: kvFloor.trim() },
        priority: priority.trim() ? Number(priority.trim()) : undefined,
        engineArgs: engineArgs.args,
      });
      setCreatedClaim(claim);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create ModelClaim.');
    } finally {
      setSubmitting(false);
    }
  };

  const copyYaml = async () => {
    await copyToClipboard(yaml);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
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
          ModelClaims
        </button>

        <div className="mb-6">
          <div className="mb-1 text-xs font-semibold uppercase tracking-widest text-teal-700">
            Create ModelClaim
          </div>
          <h1 className="mb-1 text-3xl font-semibold tracking-tight text-gray-900">
            Attach a model to a warm GPU pool
          </h1>
          <p className="text-sm text-gray-500">
            The controller places the model on a Pod whose GPU has room for it, and the runtime starts it as its own engine.
          </p>
        </div>

        <form onSubmit={handleSubmit} className="grid grid-cols-1 items-start gap-6 xl:grid-cols-3">
          <section className="rounded-xl border border-gray-200 bg-white p-6 shadow-sm xl:col-span-2">
            {error && (
              <div className="mb-5 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
                {error}
              </div>
            )}

            <FormSection title="Model" description="Only the GPU cost below can change after the claim is created.">
              <Field
                label="ModelClaim name"
                required
                hint="The Kubernetes object name."
                error={shownError('name')}
              >
                <input
                  value={name}
                  maxLength={MODEL_CLAIM_NAME_MAX_LENGTH}
                  onChange={(event) => setName(event.target.value)}
                  placeholder="qwen3-0-6b"
                  className={inputClass(Boolean(shownError('name')))}
                />
              </Field>
              <Field
                label="Served model name"
                hint="The model value clients send to the gateway. Defaults to the ModelClaim name."
                error={shownError('modelName')}
              >
                <input
                  value={modelName}
                  onChange={(event) => setModelName(event.target.value)}
                  placeholder={name.trim() || 'qwen3-0.6b'}
                  className={inputClass(Boolean(shownError('modelName')))}
                />
              </Field>
              <Field
                label="Model artifact URL"
                required
                hint="huggingface://, hf://, s3://, gcs://, tos://, or an absolute mounted path."
                error={shownError('artifactUrl')}
              >
                <input
                  value={artifactUrl}
                  onChange={(event) => setArtifactUrl(event.target.value)}
                  placeholder="huggingface://Qwen/Qwen3-0.6B"
                  className={inputClass(Boolean(shownError('artifactUrl')))}
                />
              </Field>
              <div>
                <div className="mb-2 block text-sm font-medium text-gray-700">Engine</div>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <OptionCard
                    selected={engine === 'vllm'}
                    title="vLLM"
                    description="TP × PP must equal the GPUs of each pool Pod."
                    onClick={() => setEngine('vllm')}
                  />
                  <OptionCard
                    selected={engine === 'sglang'}
                    title="SGLang"
                    description="Requires a pool built on a compatible SGLang runtime image."
                    onClick={() => setEngine('sglang')}
                  />
                </div>
              </div>
            </FormSection>

            <FormSection
              title="Warm runtime pool"
              description="Pods labelled claim.model.aibrix.ai/pool and claim.model.aibrix.ai/enabled: &quot;true&quot; in this namespace. The pool itself is managed outside the console."
            >
              {loadingPools ? (
                <div className="rounded-lg border border-gray-200 px-4 py-8 text-center text-sm text-gray-400">
                  Discovering warm runtime pools...
                </div>
              ) : pools.length === 0 ? (
                <div className="rounded-lg border border-amber-200 bg-amber-50 px-4 py-4 text-sm text-amber-700">
                  No warm runtime pools were found. Deploy one first, for example samples/modelclaim/warm-runtime-pool.yaml.
                </div>
              ) : (
                <div className="space-y-2">
                  {pools.map((pool) => (
                    <PoolOption
                      key={pool.name}
                      pool={pool}
                      selected={pool.name === selectedPoolName}
                      onClick={() => setSelectedPoolName(pool.name)}
                    />
                  ))}
                </div>
              )}
              <div className="mt-4 max-w-xs">
                <Field
                  label="Placement priority"
                  hint="Among claims waiting for a place, a higher value is placed first. It never moves a running model, and it cannot change after creation."
                  error={shownError('priority')}
                >
                  <input
                    value={priority}
                    inputMode="numeric"
                    onChange={(event) => setPriority(event.target.value)}
                    placeholder="0"
                    className={inputClass(Boolean(shownError('priority')))}
                  />
                </Field>
              </div>
            </FormSection>

            <FormSection
              title="GPU cost per instance"
              description="spec.perGPU: what one instance holds on a single GPU. Placement only puts the model on a card with room for both figures, so a claim without them is never placed."
            >
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <Field
                  label="Maximum footprint"
                  required
                  hint="Largest non-KV memory on one GPU: weights, CUDA graphs, workspaces, allocator retention."
                  error={shownError('maximumFootprint')}
                >
                  <input
                    value={maximumFootprint}
                    onChange={(event) => setMaximumFootprint(event.target.value)}
                    placeholder="e.g. 30Gi"
                    className={inputClass(Boolean(shownError('maximumFootprint')))}
                  />
                </Field>
                <Field
                  label="KV floor"
                  required
                  hint="KV cache for one request at the engine's maximum model length."
                  error={shownError('kvFloor')}
                >
                  <input
                    value={kvFloor}
                    onChange={(event) => setKvFloor(event.target.value)}
                    placeholder="e.g. 2Gi"
                    className={inputClass(Boolean(shownError('kvFloor')))}
                  />
                </Field>
              </div>
              <p className="mt-3 rounded-lg border border-gray-200 bg-gray-50 px-3 py-2 text-xs leading-relaxed text-gray-600">
                The artifact size does not predict these figures. Take them from a run of this model with these engine arguments on
                the pool&apos;s GPU. With tensor or pipeline parallelism, declare the heaviest GPU. Declaring more than an instance
                needs wastes room; declaring less is unsafe.
              </p>
            </FormSection>

            <div className="pt-6">
              <div className="mb-4 flex items-start justify-between gap-4">
                <div>
                  <h2 className="mb-1 text-lg font-semibold text-gray-900">Engine arguments</h2>
                  <p className="text-sm text-gray-500">
                    Engine CLI flags. Leave the value empty for a boolean flag. {MODEL_CLAIM_LAUNCHER_FLAGS.join(', ')} are set by
                    the runtime.
                  </p>
                </div>
                <button
                  type="button"
                  onClick={addRow}
                  className="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-gray-200 px-3 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50"
                >
                  <Plus className="h-4 w-4" />
                  Add argument
                </button>
              </div>
              {argRows.length === 0 ? (
                <div className="rounded-lg border border-dashed border-gray-200 px-4 py-4 text-sm text-gray-400">
                  No engine arguments. For example, add --max-model-len to bound the KV floor.
                </div>
              ) : (
                <div className="space-y-2">
                  {argRows.map((row, index) => {
                    const rowError = engineArgs.rowErrors[index];
                    return (
                      <div key={row.id}>
                        <div className="flex gap-2">
                          <input
                            value={row.key}
                            onChange={(event) => updateRow(index, { key: event.target.value })}
                            placeholder="--max-model-len"
                            aria-label="Engine flag"
                            className={`${inputClass(Boolean(rowError))} font-mono`}
                          />
                          <input
                            value={row.value}
                            onChange={(event) => updateRow(index, { value: event.target.value })}
                            placeholder="value"
                            aria-label="Engine flag value"
                            className={`${inputClass(false)} font-mono`}
                          />
                          <button
                            type="button"
                            aria-label="Remove argument"
                            onClick={() => setArgRows((rows) => rows.filter((_, i) => i !== index))}
                            className="shrink-0 rounded-lg border border-gray-200 px-3 text-gray-500 hover:bg-gray-50 hover:text-red-600"
                          >
                            <Trash2 className="h-4 w-4" />
                          </button>
                        </div>
                        {rowError && <p className="mt-1 text-xs text-red-600">{rowError}</p>}
                      </div>
                    );
                  })}
                </div>
              )}
              {engineArgs.topologyError && (
                <div className="mt-3 flex items-start gap-2 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-700">
                  <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
                  {engineArgs.topologyError}
                </div>
              )}
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
                disabled={submitting || !selectedPool}
                className="inline-flex items-center gap-2 rounded-lg bg-teal-700 px-4 py-2.5 text-sm font-medium text-white hover:bg-teal-800 disabled:cursor-not-allowed disabled:opacity-50"
              >
                {submitting ? <LoaderCircle className="h-4 w-4 animate-spin" /> : <Plus className="h-4 w-4" />}
                Create ModelClaim
              </button>
            </div>
          </section>

          <aside className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm xl:sticky xl:top-6">
            <div className="mb-3 flex items-center justify-between">
              <h2 className="text-base font-semibold text-gray-900">ModelClaim preview</h2>
              <button
                type="button"
                onClick={copyYaml}
                className="inline-flex items-center gap-1.5 rounded-md border border-gray-200 px-2 py-1 text-xs text-gray-600 hover:bg-gray-50"
              >
                {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                {copied ? 'Copied' : 'Copy YAML'}
              </button>
            </div>
            <pre className="whitespace-pre-wrap break-all rounded-lg bg-slate-900 p-3 font-mono text-xs leading-relaxed text-slate-100">
              {yaml}
            </pre>
            <div className="mt-4 rounded-lg border border-teal-200 bg-teal-50 px-3 py-3 text-xs leading-relaxed text-teal-800">
              The same object can be applied with <code>kubectl apply -f</code>. The console lists claims created either way.
            </div>
          </aside>
        </form>
      </div>

      {(submitting || createdClaim) && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-900/50 p-6 backdrop-blur-sm">
          <div className="w-full max-w-md overflow-hidden rounded-xl border border-gray-200 bg-white shadow-2xl">
            <div className="flex items-center gap-4 p-6">
              {createdClaim ? (
                <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-green-600 text-white">
                  <Check className="h-5 w-5" />
                </span>
              ) : (
                <LoaderCircle className="h-9 w-9 shrink-0 animate-spin text-teal-700" />
              )}
              <div>
                <div className="font-semibold text-gray-900">
                  {createdClaim ? 'ModelClaim created' : 'Creating ModelClaim'}
                </div>
                <div className="mt-1 text-sm text-gray-500">
                  {createdClaim
                    ? `${createdClaim.name} is ${createdClaim.phase.toLowerCase()}. Placement and engine start continue in the cluster.`
                    : `Submitting ${name || 'the requested claim'} to Kubernetes.`}
                </div>
              </div>
            </div>
            <div className="flex justify-end border-t border-gray-100 bg-gray-50 px-5 py-3">
              <button
                type="button"
                disabled={!createdClaim}
                onClick={() => createdClaim && onCreated(createdClaim.name)}
                className="rounded-lg bg-teal-700 px-4 py-2 text-sm font-medium text-white hover:bg-teal-800 disabled:opacity-50"
              >
                View ModelClaim
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

function FormSection({ title, description, children }: { title: string; description: string; children: ReactNode }) {
  return (
    <div className="space-y-5 border-b border-gray-100 py-6 first:pt-0">
      <div>
        <h2 className="mb-1 text-lg font-semibold text-gray-900">{title}</h2>
        <p className="text-sm text-gray-500">{description}</p>
      </div>
      {children}
    </div>
  );
}

function Field({
  label,
  required = false,
  hint,
  error,
  children,
}: {
  label: string;
  required?: boolean;
  hint?: string;
  error?: string | null;
  children: ReactNode;
}) {
  return (
    <label className="block">
      <span className="mb-2 block text-sm font-medium text-gray-700">
        {label} {required && <span className="text-red-600">*</span>}
      </span>
      {children}
      {hint && <span className="mt-2 block text-xs text-gray-400">{hint}</span>}
      {error && <span className="mt-1 block text-xs text-red-600">{error}</span>}
    </label>
  );
}

function OptionCard({
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
      className={`rounded-lg border p-4 text-left ${
        selected ? 'border-teal-400 bg-teal-50/60 ring-2 ring-teal-100' : 'border-gray-200 hover:border-teal-200'
      }`}
    >
      <div className="flex items-center gap-3">
        <Radio selected={selected} />
        <span className="text-sm font-semibold text-gray-900">{title}</span>
      </div>
      <p className="ml-7 mt-2 text-xs leading-relaxed text-gray-500">{description}</p>
    </button>
  );
}

function PoolOption({ pool, selected, onClick }: { pool: ModelClaimPool; selected: boolean; onClick: () => void }) {
  const warning = pool.enabledPods === 0
    ? 'No Pod carries claim.model.aibrix.ai/enabled: "true", so a claim waits for one.'
    : pool.readyPods === 0
      ? 'No Pod is Ready yet, so a claim waits for one.'
      : pool.gpuCounts.length > 1
        ? 'Pods have different GPU counts. Keep one topology per pool.'
        : null;
  return (
    <button
      type="button"
      onClick={onClick}
      className={`w-full rounded-lg border p-4 text-left transition-colors ${
        selected ? 'border-teal-400 bg-teal-50/60 ring-2 ring-teal-100' : 'border-gray-200 hover:border-teal-200'
      }`}
    >
      <div className="flex items-start gap-3">
        <span className="mt-0.5">
          <Radio selected={selected} />
        </span>
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold text-gray-900">{pool.name}</div>
          <div className="mt-1 truncate text-xs text-gray-500">
            {pool.workloads.length > 0 ? pool.workloads.join(', ') : 'No Deployment found'}
            {pool.images.length > 0 ? ` · ${pool.images.join(', ')}` : ''}
          </div>
        </div>
        <span className="rounded-md border border-gray-200 bg-gray-50 px-2 py-1 text-xs font-medium text-gray-600">
          {formatPoolGpus(pool)} GPU / Pod
        </span>
      </div>
      <div className="ml-7 mt-3 flex flex-wrap gap-x-5 gap-y-1 border-t border-gray-100 pt-3 text-xs text-gray-500">
        <span>{pool.readyPods}/{pool.enabledPods} enabled Pods ready</span>
        <span>{pool.claims} ModelClaims</span>
        {pool.policy && <span className="truncate font-mono" title={pool.policy}>policy {pool.policy}</span>}
      </div>
      {warning && (
        <div className="ml-7 mt-2 flex items-start gap-1.5 text-xs text-amber-700">
          <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
          {warning}
        </div>
      )}
    </button>
  );
}

function Radio({ selected }: { selected: boolean }) {
  return (
    <span
      className={`flex h-4 w-4 shrink-0 items-center justify-center rounded-full border ${
        selected ? 'border-teal-700' : 'border-gray-300'
      }`}
    >
      {selected && <span className="h-2 w-2 rounded-full bg-teal-700" />}
    </span>
  );
}
