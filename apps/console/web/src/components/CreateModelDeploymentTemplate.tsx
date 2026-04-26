import { useEffect, useState } from 'react';
import { ChevronLeft, Save, Trash2 } from 'lucide-react';
import {
  createModelDeploymentTemplate,
  getModel,
  getModelDeploymentTemplate,
  updateModelDeploymentTemplate,
} from '../utils/api';
import type {
  ModelDeploymentTemplate,
  ModelDeploymentTemplateSpec,
} from '../utils/api';
import type { Model } from '../data/mockData';

interface CreateModelDeploymentTemplateProps {
  modelId: string;
  templateId?: string; // when set, the form is in edit mode
  onBack: () => void;
  onSaved: () => void;
}

const ENGINE_TYPES = ['vllm', 'sglang', 'trtllm', 'lmdeploy', 'mock'];
const MODEL_SOURCE_TYPES = ['huggingface', 's3', 'local', 'registry'];
const INTERCONNECT_OPTIONS = ['', 'nvlink', 'pcie', 'ib'];
const WEIGHT_QUANT_OPTIONS = ['', 'fp8', 'awq', 'gptq', 'int8', 'bf16', 'fp16'];
const KV_QUANT_OPTIONS = ['', 'auto', 'fp8', 'fp8_e4m3', 'fp8_e5m2', 'int8'];
const PROVIDER_TYPES = ['k8s', 'runpod', 'lambda_labs', 'ec2', 'gcp', 'external'];
const DEPLOYMENT_MODES = ['dedicated', 'shared', 'external'];
const STATUS_OPTIONS = ['active', 'draft', 'deprecated'];

const COMMON_ENDPOINTS = [
  '/v1/chat/completions',
  '/v1/completions',
  '/v1/embeddings',
  '/v1/audio/transcriptions',
  '/v1/audio/translations',
  '/v1/images/generations',
];

// Curated list of well-known engine_args knobs surfaced as labeled inputs.
// The data model is still a flat string map; this is purely a UI affordance.
// Inputs not in this list are presented as a free-form key/value editor.
type KnobKind = 'int' | 'float' | 'bool' | 'string';
interface KnownKnob {
  key: string;
  label: string;
  kind: KnobKind;
}
const KNOWN_ENGINE_ARGS: KnownKnob[] = [
  { key: 'max_num_batched_tokens', label: 'max_num_batched_tokens', kind: 'int' },
  { key: 'max_num_seqs', label: 'max_num_seqs', kind: 'int' },
  { key: 'max_model_len', label: 'max_model_len', kind: 'int' },
  { key: 'gpu_memory_utilization', label: 'gpu_memory_utilization', kind: 'float' },
  { key: 'block_size', label: 'block_size', kind: 'int' },
  { key: 'swap_space', label: 'swap_space (GB)', kind: 'int' },
  { key: 'enable_prefix_caching', label: 'enable_prefix_caching', kind: 'bool' },
  { key: 'enable_chunked_prefill', label: 'enable_chunked_prefill', kind: 'bool' },
  { key: 'speculative_model', label: 'speculative_model', kind: 'string' },
  { key: 'num_speculative_tokens', label: 'num_speculative_tokens', kind: 'int' },
];
const KNOWN_KEYS = new Set(KNOWN_ENGINE_ARGS.map((k) => k.key));

function emptySpec(): ModelDeploymentTemplateSpec {
  return {
    engine: { type: 'vllm', version: '', image: '', invocation: 'http_server', healthEndpoint: '/health' },
    modelSource: { type: 'huggingface', uri: '' },
    accelerator: { type: '', count: 1 },
    parallelism: { tp: 1, pp: 1, dp: 1 },
    engineArgs: {},
    quantization: {},
    providerConfig: { type: 'k8s', extra: {} },
    supportedEndpoints: ['/v1/chat/completions'],
    deploymentMode: 'dedicated',
  };
}

export function CreateModelDeploymentTemplate({
  modelId,
  templateId,
  onBack,
  onSaved,
}: CreateModelDeploymentTemplateProps) {
  const isEdit = !!templateId;

  const [model, setModel] = useState<Model | null>(null);
  const [name, setName] = useState('');
  const [version, setVersion] = useState('v1.0.0');
  const [statusValue, setStatusValue] = useState('active');
  const [spec, setSpec] = useState<ModelDeploymentTemplateSpec>(emptySpec());
  const [serveArgsRaw, setServeArgsRaw] = useState('');
  const [providerExtraRaw, setProviderExtraRaw] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    getModel(modelId).then(setModel).catch(() => setModel(null));
  }, [modelId]);

  useEffect(() => {
    if (!templateId) return;
    getModelDeploymentTemplate(modelId, templateId)
      .then((t: ModelDeploymentTemplate) => {
        setName(t.name);
        setVersion(t.version);
        setStatusValue(t.status);
        const s = t.spec ?? emptySpec();
        setSpec(s);
        setServeArgsRaw((s.engine?.serveArgs ?? []).join('\n'));
        setProviderExtraRaw(
          Object.entries(s.providerConfig?.extra ?? {})
            .map(([k, v]) => `${k}=${v}`)
            .join('\n'),
        );
      })
      .catch(err => setError(`Failed to load template: ${err}`));
  }, [templateId, modelId]);

  const updateSpec = <K extends keyof ModelDeploymentTemplateSpec>(
    key: K,
    patch: Partial<NonNullable<ModelDeploymentTemplateSpec[K]>>,
  ) => {
    setSpec((prev) => ({
      ...prev,
      [key]: { ...(prev[key] ?? {}), ...patch } as ModelDeploymentTemplateSpec[K],
    }));
  };

  const toggleEndpoint = (ep: string) => {
    const current = spec.supportedEndpoints ?? [];
    setSpec((prev) => ({
      ...prev,
      supportedEndpoints: current.includes(ep)
        ? current.filter((x) => x !== ep)
        : [...current, ep],
    }));
  };

  const parseProviderExtra = (raw: string): Record<string, string> => {
    const out: Record<string, string> = {};
    for (const line of raw.split('\n')) {
      const trimmed = line.trim();
      if (!trimmed || !trimmed.includes('=')) continue;
      const [k, ...rest] = trimmed.split('=');
      out[k.trim()] = rest.join('=').trim();
    }
    return out;
  };

  const handleSave = async () => {
    setError(null);
    if (!name.trim()) {
      setError('Template name is required');
      return;
    }
    if (!spec.accelerator?.type) {
      setError('Accelerator type is required');
      return;
    }
    const ws =
      (spec.parallelism?.tp ?? 1) *
      (spec.parallelism?.pp ?? 1) *
      (spec.parallelism?.dp ?? 1) *
      (spec.parallelism?.ep ?? 1);
    if (ws !== (spec.accelerator?.count ?? 1)) {
      setError(`Parallelism (tp*pp*dp*ep=${ws}) must equal accelerator.count (${spec.accelerator?.count})`);
      return;
    }
    if (!spec.supportedEndpoints || spec.supportedEndpoints.length === 0) {
      setError('Pick at least one supported endpoint');
      return;
    }

    const serveArgs = serveArgsRaw
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean);
    const providerExtra = parseProviderExtra(providerExtraRaw);

    const finalSpec: ModelDeploymentTemplateSpec = {
      ...spec,
      engine: { ...(spec.engine ?? {}), serveArgs },
      providerConfig: { ...(spec.providerConfig ?? { type: 'k8s' }), extra: providerExtra },
    };

    setSaving(true);
    try {
      if (isEdit && templateId) {
        await updateModelDeploymentTemplate({
          id: templateId,
          modelId,
          name,
          version,
          status: statusValue,
          spec: finalSpec,
        });
      } else {
        await createModelDeploymentTemplate({
          name,
          version,
          status: statusValue,
          modelId,
          spec: finalSpec,
        });
      }
      onSaved();
    } catch (err) {
      setError(`Save failed: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="p-8 max-w-5xl">
      <button
        onClick={onBack}
        className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4"
      >
        <ChevronLeft className="w-4 h-4" />
        {model ? `${model.name} / ` : ''}
        <span className="text-gray-400">
          {isEdit ? 'Edit Template' : 'Create Template'}
        </span>
      </button>

      <h1 className="text-2xl mb-1">
        {isEdit ? 'Edit Deployment Template' : 'Create Deployment Template'}
      </h1>
      <p className="text-sm text-gray-500 mb-6">
        Capture engine, accelerator, parallelism, and tuning settings for{' '}
        <span className="font-medium">{model?.name ?? modelId}</span>. Templates are
        reusable across online deployments and batch jobs.
      </p>

      {error && (
        <div className="mb-4 p-3 border border-red-200 bg-red-50 text-sm text-red-700 rounded-lg">
          {error}
        </div>
      )}

      <div className="space-y-6">
        {/* Identity */}
        <Section title="Identity">
          <Field label="Name">
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. llama3-70b-prod"
              className={inputCls}
            />
          </Field>
          <Field label="Version">
            <input
              type="text"
              value={version}
              onChange={(e) => setVersion(e.target.value)}
              className={inputCls}
            />
          </Field>
          <Field label="Status">
            <select value={statusValue} onChange={(e) => setStatusValue(e.target.value)} className={inputCls}>
              {STATUS_OPTIONS.map((s) => (
                <option key={s} value={s}>{s}</option>
              ))}
            </select>
          </Field>
        </Section>

        {/* Engine */}
        <Section title="Engine">
          <Field label="Type">
            <select
              value={spec.engine?.type ?? 'vllm'}
              onChange={(e) => updateSpec('engine', { type: e.target.value })}
              className={inputCls}
            >
              {ENGINE_TYPES.map((t) => (
                <option key={t} value={t}>{t}</option>
              ))}
            </select>
          </Field>
          <Field label="Version">
            <input
              type="text"
              value={spec.engine?.version ?? ''}
              onChange={(e) => updateSpec('engine', { version: e.target.value })}
              placeholder="0.6.3"
              className={inputCls}
            />
          </Field>
          <Field label="Image" wide>
            <input
              type="text"
              value={spec.engine?.image ?? ''}
              onChange={(e) => updateSpec('engine', { image: e.target.value })}
              placeholder="vllm/vllm-openai:v0.6.3"
              className={inputCls}
            />
          </Field>
          <Field label="Health endpoint">
            <input
              type="text"
              value={spec.engine?.healthEndpoint ?? ''}
              onChange={(e) => updateSpec('engine', { healthEndpoint: e.target.value })}
              className={inputCls}
            />
          </Field>
          <Field label="Ready timeout (s)">
            <input
              type="number"
              value={spec.engine?.readyTimeoutSeconds ?? 600}
              onChange={(e) => updateSpec('engine', { readyTimeoutSeconds: Number(e.target.value) })}
              className={inputCls}
            />
          </Field>
          <Field label="Serve args (one per line)" wide>
            <textarea
              value={serveArgsRaw}
              onChange={(e) => setServeArgsRaw(e.target.value)}
              placeholder={'--port=8000\n--enable-prefix-caching'}
              rows={3}
              className={`${inputCls} font-mono`}
            />
          </Field>
        </Section>

        {/* Model Source */}
        <Section title="Model Source">
          <Field label="Type">
            <select
              value={spec.modelSource?.type ?? 'huggingface'}
              onChange={(e) => updateSpec('modelSource', { type: e.target.value })}
              className={inputCls}
            >
              {MODEL_SOURCE_TYPES.map((t) => (
                <option key={t} value={t}>{t}</option>
              ))}
            </select>
          </Field>
          <Field label="URI" wide>
            <input
              type="text"
              value={spec.modelSource?.uri ?? ''}
              onChange={(e) => updateSpec('modelSource', { uri: e.target.value })}
              placeholder="meta-llama/Llama-3.3-70B-Instruct or s3://bucket/path/"
              className={inputCls}
            />
          </Field>
          <Field label="Revision">
            <input
              type="text"
              value={spec.modelSource?.revision ?? ''}
              onChange={(e) => updateSpec('modelSource', { revision: e.target.value })}
              placeholder="main"
              className={inputCls}
            />
          </Field>
          <Field label="Auth secret ref">
            <input
              type="text"
              value={spec.modelSource?.authSecretRef ?? ''}
              onChange={(e) => updateSpec('modelSource', { authSecretRef: e.target.value })}
              placeholder="aibrix-hf-token"
              className={inputCls}
            />
          </Field>
          <Field label="Tokenizer path">
            <input
              type="text"
              value={spec.modelSource?.tokenizerPath ?? ''}
              onChange={(e) => updateSpec('modelSource', { tokenizerPath: e.target.value })}
              className={inputCls}
            />
          </Field>
          <Field label="Chat template path">
            <input
              type="text"
              value={spec.modelSource?.chatTemplatePath ?? ''}
              onChange={(e) => updateSpec('modelSource', { chatTemplatePath: e.target.value })}
              className={inputCls}
            />
          </Field>
        </Section>

        {/* Accelerator */}
        <Section title="Accelerator">
          <Field label="GPU type">
            <input
              type="text"
              value={spec.accelerator?.type ?? ''}
              onChange={(e) => updateSpec('accelerator', { type: e.target.value })}
              placeholder="H100-SXM"
              className={inputCls}
            />
          </Field>
          <Field label="Count">
            <input
              type="number"
              min={1}
              value={spec.accelerator?.count ?? 1}
              onChange={(e) => updateSpec('accelerator', { count: Number(e.target.value) })}
              className={inputCls}
            />
          </Field>
          <Field label="Interconnect">
            <select
              value={spec.accelerator?.interconnect ?? ''}
              onChange={(e) => updateSpec('accelerator', { interconnect: e.target.value })}
              className={inputCls}
            >
              {INTERCONNECT_OPTIONS.map((o) => (
                <option key={o || 'none'} value={o}>{o || '—'}</option>
              ))}
            </select>
          </Field>
          <Field label="VRAM (GB)">
            <input
              type="number"
              min={0}
              value={spec.accelerator?.vramGb ?? ''}
              onChange={(e) => updateSpec('accelerator', { vramGb: e.target.value === '' ? undefined : Number(e.target.value) })}
              className={inputCls}
            />
          </Field>
          <Field label="SKU hint" wide>
            <input
              type="text"
              value={spec.accelerator?.skuHint ?? ''}
              onChange={(e) => updateSpec('accelerator', { skuHint: e.target.value })}
              placeholder="aws/p5.48xlarge"
              className={inputCls}
            />
          </Field>
        </Section>

        {/* Parallelism */}
        <Section title="Parallelism">
          {(['tp', 'pp', 'dp', 'ep', 'sp', 'cp'] as const).map((k) => (
            <Field key={k} label={k.toUpperCase()}>
              <input
                type="number"
                min={1}
                value={spec.parallelism?.[k] ?? 1}
                onChange={(e) => updateSpec('parallelism', { [k]: Number(e.target.value) })}
                className={inputCls}
              />
            </Field>
          ))}
        </Section>

        {/* Engine args (free-form key/value, with curated knobs surfaced) */}
        <EngineArgsSection
          args={spec.engineArgs ?? {}}
          onChange={(next) => setSpec((prev) => ({ ...prev, engineArgs: next }))}
        />


        {/* Quantization */}
        <Section title="Quantization">
          <Field label="Weight">
            <select
              value={spec.quantization?.weight ?? ''}
              onChange={(e) => updateSpec('quantization', { weight: e.target.value })}
              className={inputCls}
            >
              {WEIGHT_QUANT_OPTIONS.map((w) => (
                <option key={w || 'none'} value={w}>{w || '—'}</option>
              ))}
            </select>
          </Field>
          <Field label="KV cache">
            <select
              value={spec.quantization?.kvCache ?? ''}
              onChange={(e) => updateSpec('quantization', { kvCache: e.target.value })}
              className={inputCls}
            >
              {KV_QUANT_OPTIONS.map((q) => (
                <option key={q || 'none'} value={q}>{q || '—'}</option>
              ))}
            </select>
          </Field>
          <Field label="Pre-quantized weights URI" wide>
            <input
              type="text"
              value={spec.quantization?.weightsArtifactUri ?? ''}
              onChange={(e) => updateSpec('quantization', { weightsArtifactUri: e.target.value })}
              className={inputCls}
            />
          </Field>
        </Section>

        {/* Provider */}
        <Section title="Provider">
          <Field label="Type">
            <select
              value={spec.providerConfig?.type ?? 'k8s'}
              onChange={(e) => updateSpec('providerConfig', { type: e.target.value })}
              className={inputCls}
            >
              {PROVIDER_TYPES.map((p) => (
                <option key={p} value={p}>{p}</option>
              ))}
            </select>
          </Field>
          <Field label="Deployment mode">
            <select
              value={spec.deploymentMode ?? 'dedicated'}
              onChange={(e) => setSpec((prev) => ({ ...prev, deploymentMode: e.target.value }))}
              className={inputCls}
            >
              {DEPLOYMENT_MODES.map((m) => (
                <option key={m} value={m}>{m}</option>
              ))}
            </select>
          </Field>
          <Field label="Provider extras (key=value, one per line)" wide>
            <textarea
              value={providerExtraRaw}
              onChange={(e) => setProviderExtraRaw(e.target.value)}
              placeholder={'namespace=aibrix-inference\nservice_account=aibrix-engine'}
              rows={3}
              className={`${inputCls} font-mono`}
            />
          </Field>
        </Section>

        {/* Endpoints */}
        <Section title="Supported endpoints">
          <div className="col-span-full flex flex-wrap gap-2">
            {COMMON_ENDPOINTS.map((ep) => {
              const active = (spec.supportedEndpoints ?? []).includes(ep);
              return (
                <button
                  key={ep}
                  type="button"
                  onClick={() => toggleEndpoint(ep)}
                  className={`px-3 py-1.5 text-xs rounded-full border transition-colors ${
                    active
                      ? 'bg-slate-800 text-white border-slate-800'
                      : 'bg-white text-gray-600 border-gray-200 hover:bg-gray-50'
                  }`}
                >
                  {ep}
                </button>
              );
            })}
          </div>
        </Section>
      </div>

      <div className="flex items-center gap-3 mt-6">
        <button
          onClick={handleSave}
          disabled={saving}
          className="inline-flex items-center gap-2 px-4 py-2 bg-slate-800 text-white text-sm rounded-lg hover:bg-slate-700 disabled:opacity-50"
        >
          {isEdit ? <Save className="w-4 h-4" /> : <Trash2 className="w-4 h-4 hidden" />}
          {saving ? 'Saving…' : isEdit ? 'Save Changes' : 'Create Template'}
        </button>
        <button
          onClick={onBack}
          className="px-4 py-2 text-sm text-gray-600 border border-gray-200 rounded-lg hover:bg-gray-50"
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

const inputCls =
  'w-full px-3 py-2 bg-white border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500';

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
      <h3 className="text-sm mb-4">{title}</h3>
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">{children}</div>
    </div>
  );
}

function Field({
  label,
  wide,
  children,
}: {
  label: string;
  wide?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className={wide ? 'sm:col-span-2 lg:col-span-3' : ''}>
      <label className="block text-xs text-gray-600 mb-1">{label}</label>
      {children}
    </div>
  );
}

function EngineArgsSection({
  args,
  onChange,
}: {
  args: Record<string, string>;
  onChange: (next: Record<string, string>) => void;
}) {
  const setKey = (key: string, value: string | undefined) => {
    const next = { ...args };
    if (value === undefined || value === '') {
      delete next[key];
    } else {
      next[key] = value;
    }
    onChange(next);
  };

  const renameKey = (oldKey: string, newKey: string) => {
    if (newKey === oldKey) return;
    const next = { ...args };
    if (oldKey in next) {
      const v = next[oldKey];
      delete next[oldKey];
      if (newKey) next[newKey] = v;
    }
    onChange(next);
  };

  const customEntries = Object.entries(args).filter(([k]) => !KNOWN_KEYS.has(k));

  return (
    <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
      <div className="mb-4">
        <h3 className="text-sm">Engine args</h3>
        <p className="text-xs text-gray-500 mt-1">
          Key/value flags forwarded to the engine. Common knobs are listed below; use Custom for engine-specific flags.
        </p>
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 mb-6">
        {KNOWN_ENGINE_ARGS.map((knob) => {
          const raw = args[knob.key];
          if (knob.kind === 'bool') {
            const value = raw === undefined ? '' : raw === 'true' ? 'true' : 'false';
            return (
              <Field key={knob.key} label={knob.label}>
                <select
                  value={value}
                  onChange={(e) => setKey(knob.key, e.target.value === '' ? undefined : e.target.value)}
                  className={inputCls}
                >
                  <option value="">—</option>
                  <option value="true">true</option>
                  <option value="false">false</option>
                </select>
              </Field>
            );
          }
          const inputType = knob.kind === 'int' || knob.kind === 'float' ? 'number' : 'text';
          const step = knob.kind === 'float' ? '0.01' : undefined;
          return (
            <Field key={knob.key} label={knob.label}>
              <input
                type={inputType}
                step={step}
                value={raw ?? ''}
                onChange={(e) => setKey(knob.key, e.target.value === '' ? undefined : e.target.value)}
                className={inputCls}
              />
            </Field>
          );
        })}
      </div>

      <div className="border-t border-gray-100 pt-4">
        <div className="flex items-center justify-between mb-2">
          <h4 className="text-xs text-gray-700">Custom flags</h4>
          <button
            type="button"
            onClick={() => onChange({ ...args, '': '' })}
            className="text-xs text-teal-600 hover:text-teal-700"
          >
            + Add
          </button>
        </div>
        {customEntries.length === 0 ? (
          <p className="text-xs text-gray-400">No custom flags set.</p>
        ) : (
          <div className="space-y-2">
            {customEntries.map(([k, v], idx) => (
              <div key={`${k}-${idx}`} className="flex items-center gap-2">
                <input
                  type="text"
                  value={k}
                  placeholder="flag_name"
                  onChange={(e) => renameKey(k, e.target.value)}
                  className={`${inputCls} flex-1 font-mono`}
                />
                <input
                  type="text"
                  value={v}
                  placeholder="value"
                  onChange={(e) => setKey(k, e.target.value)}
                  className={`${inputCls} flex-1 font-mono`}
                />
                <button
                  type="button"
                  onClick={() => setKey(k, undefined)}
                  className="px-2 py-1 text-xs text-gray-400 hover:text-red-600"
                  aria-label="Remove"
                >
                  ×
                </button>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
