import { useEffect, useRef, useState } from 'react';
import { ChevronLeft, Save } from 'lucide-react';
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
  templateId?: string; // when set with mode!=='view', edit mode; with mode==='view', read-only
  cloneFromId?: string; // when set, prefill from this template but save as new (create path)
  mode?: 'view';
  onBack: () => void;
  onSaved: () => void;
}

// Bump trailing numeric component of a version string. Examples:
//   v1.2.3 -> v1.2.4   v1.2 -> v1.3   v1 -> v2   foo-7 -> foo-8   plain -> plain-2
function bumpVersion(v: string): string {
  const semver = v.match(/^(v?)(\d+)\.(\d+)\.(\d+)$/);
  if (semver) return `${semver[1]}${semver[2]}.${semver[3]}.${Number(semver[4]) + 1}`;
  const minor = v.match(/^(v?)(\d+)\.(\d+)$/);
  if (minor) return `${minor[1]}${minor[2]}.${Number(minor[3]) + 1}`;
  const major = v.match(/^(v?)(\d+)$/);
  if (major) return `${major[1]}${Number(major[2]) + 1}`;
  const tail = v.match(/^(.*?)(\d+)$/);
  if (tail) return `${tail[1]}${Number(tail[2]) + 1}`;
  return `${v}-2`;
}

const ENGINE_TYPES = ['vllm', 'sglang', 'trtllm'];
const MODEL_SOURCE_TYPES = ['huggingface', 's3', 'local'];

interface SourceAuthHint {
  show: boolean;
  placeholder: string;
  help: string;
}
function authHint(sourceType: string | undefined): SourceAuthHint {
  switch (sourceType) {
    case 'huggingface':
      return {
        show: true,
        placeholder: 'aibrix-hf-token',
        help: 'K8s Secret containing key "token" — mounted as HF_TOKEN env on the engine container.',
      };
    case 's3':
      return {
        show: true,
        placeholder: 'aibrix-s3-creds',
        help: 'K8s Secret with keys access_key_id, secret_access_key (and optional session_token, region). Wiring not yet implemented in renderer.',
      };
    case 'local':
    default:
      return { show: false, placeholder: '', help: '' };
  }
}
// Curated GPU catalog. vram_gb / interconnect derive from the SKU pick so
// the user only chooses a name; the renderer doesn't read these fields today
// but they're persisted on the spec for downstream schedulers.
interface GpuSku {
  type: string;
  label: string;
  vramGb: number;
  interconnect: 'nvlink' | 'pcie' | 'ib' | '';
}
const GPU_CATALOG: GpuSku[] = [
  { type: 'H200-SXM', label: 'NVIDIA H200 SXM (141 GB, NVLink)', vramGb: 141, interconnect: 'nvlink' },
  { type: 'H100-SXM', label: 'NVIDIA H100 SXM (80 GB, NVLink)', vramGb: 80, interconnect: 'nvlink' },
  { type: 'H100-NVL', label: 'NVIDIA H100 NVL (94 GB, NVLink)', vramGb: 94, interconnect: 'nvlink' },
  { type: 'H100-PCIe', label: 'NVIDIA H100 PCIe (80 GB, PCIe)', vramGb: 80, interconnect: 'pcie' },
  { type: 'B200', label: 'NVIDIA B200 (192 GB, NVLink)', vramGb: 192, interconnect: 'nvlink' },
  { type: 'A100-SXM-80', label: 'NVIDIA A100 SXM (80 GB, NVLink)', vramGb: 80, interconnect: 'nvlink' },
  { type: 'A100-SXM-40', label: 'NVIDIA A100 SXM (40 GB, NVLink)', vramGb: 40, interconnect: 'nvlink' },
  { type: 'A100-PCIe', label: 'NVIDIA A100 PCIe (40 GB, PCIe)', vramGb: 40, interconnect: 'pcie' },
  { type: 'L40S', label: 'NVIDIA L40S (48 GB, PCIe)', vramGb: 48, interconnect: 'pcie' },
  { type: 'L4', label: 'NVIDIA L4 (24 GB, PCIe)', vramGb: 24, interconnect: 'pcie' },
  { type: 'T4', label: 'NVIDIA T4 (16 GB, PCIe)', vramGb: 16, interconnect: 'pcie' },
  { type: 'MI300X', label: 'AMD MI300X (192 GB, IF)', vramGb: 192, interconnect: 'ib' },
  { type: 'CPU', label: 'CPU (no GPU)', vramGb: 0, interconnect: '' },
];
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
];
const KNOWN_KEYS = new Set(KNOWN_ENGINE_ARGS.map((k) => k.key));

function emptySpec(): ModelDeploymentTemplateSpec {
  return {
    engine: { type: 'vllm', version: '', image: '', invocation: 'http_server' },
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
  cloneFromId,
  mode,
  onBack,
  onSaved,
}: CreateModelDeploymentTemplateProps) {
  const isView = mode === 'view' && !!templateId;
  const isClone = !!cloneFromId && !templateId;
  const isEdit = !!templateId && !isView;
  // Source template to prefill from (edit/view loads templateId; clone loads cloneFromId).
  const sourceTemplateId = templateId || cloneFromId;

  const [model, setModel] = useState<Model | null>(null);
  const [name, setName] = useState('');
  const [version, setVersion] = useState('v1.0.0');
  const [statusValue, setStatusValue] = useState('active');
  const [spec, setSpec] = useState<ModelDeploymentTemplateSpec>(emptySpec());
  const [providerExtraRaw, setProviderExtraRaw] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    getModel(modelId).then(setModel).catch(() => setModel(null));
  }, [modelId]);

  useEffect(() => {
    if (!sourceTemplateId) return;
    getModelDeploymentTemplate(modelId, sourceTemplateId)
      .then((t: ModelDeploymentTemplate) => {
        setName(t.name);
        // For clone, auto-bump the version so user lands on a non-conflicting one.
        setVersion(isClone ? bumpVersion(t.version) : t.version);
        setStatusValue(t.status);
        const s = t.spec ?? emptySpec();
        setSpec(s);
        setProviderExtraRaw(
          Object.entries(s.providerConfig?.extra ?? {})
            .map(([k, v]) => `${k}=${v}`)
            .join('\n'),
        );
      })
      .catch(err => setError(`Failed to load template: ${err}`));
  }, [sourceTemplateId, modelId, isClone]);

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
    // Treat 0 (proto int default for unset fields) as 1, since each dim is 1-based.
    const ws =
      (spec.parallelism?.tp || 1) *
      (spec.parallelism?.pp || 1) *
      (spec.parallelism?.dp || 1) *
      (spec.parallelism?.ep || 1);
    if (ws !== (spec.accelerator?.count ?? 1)) {
      setError(`Parallelism (tp*pp*dp*ep=${ws}) must equal accelerator.count (${spec.accelerator?.count})`);
      return;
    }
    if (!spec.supportedEndpoints || spec.supportedEndpoints.length === 0) {
      setError('Pick at least one supported endpoint');
      return;
    }

    const providerExtra = parseProviderExtra(providerExtraRaw);

    const finalSpec: ModelDeploymentTemplateSpec = {
      ...spec,
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
          {isView ? 'View Template' : isEdit ? 'Edit Template' : isClone ? 'Clone Template' : 'Create Template'}
        </span>
      </button>

      <h1 className="text-2xl mb-1">
        {isView
          ? 'View Deployment Template'
          : isEdit
          ? 'Edit Deployment Template'
          : isClone
          ? 'Clone Deployment Template'
          : 'Create Deployment Template'}
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

      <fieldset disabled={isView} className="space-y-6 disabled:opacity-90">
        {/* Identity */}
        <Section title="Identity">
          <Field label="Name">
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. llama3-70b-prod"
              className={inputCls}
              disabled={isClone || isEdit}
              title={
                isClone
                  ? 'Name is fixed when cloning; only the version changes.'
                  : isEdit
                  ? 'Name is part of the template identity. To rename, clone into a new template.'
                  : undefined
              }
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

        {/* Model Source */}
        {(() => {
          const sourceType = spec.modelSource?.type ?? 'huggingface';
          const hint = authHint(sourceType);
          const isHF = sourceType === 'huggingface';
          return (
            <Section title="Model Source">
              <Field label="Type">
                <select
                  value={sourceType}
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
              {isHF && (
                <Field label="Revision">
                  <input
                    type="text"
                    value={spec.modelSource?.revision ?? ''}
                    onChange={(e) => updateSpec('modelSource', { revision: e.target.value })}
                    placeholder="main"
                    className={inputCls}
                  />
                </Field>
              )}
              {hint.show && (
                <Field label="Auth secret" wide>
                  <input
                    type="text"
                    value={spec.modelSource?.authSecretRef ?? ''}
                    onChange={(e) => updateSpec('modelSource', { authSecretRef: e.target.value })}
                    placeholder={hint.placeholder}
                    className={inputCls}
                  />
                  <p className="text-xs text-gray-500 mt-1">{hint.help}</p>
                </Field>
              )}
              <div className="sm:col-span-2 lg:col-span-3">
                <details className="group">
                  <summary className="cursor-pointer text-xs text-gray-500 hover:text-gray-700 select-none">
                    Advanced — tokenizer / chat template overrides
                  </summary>
                  <div className="mt-3 grid grid-cols-1 sm:grid-cols-2 gap-4">
                    <div>
                      <label className="block text-xs text-gray-600 mb-1">Tokenizer path</label>
                      <input
                        type="text"
                        value={spec.modelSource?.tokenizerPath ?? ''}
                        onChange={(e) => updateSpec('modelSource', { tokenizerPath: e.target.value })}
                        className={inputCls}
                      />
                    </div>
                    <div>
                      <label className="block text-xs text-gray-600 mb-1">Chat template path</label>
                      <input
                        type="text"
                        value={spec.modelSource?.chatTemplatePath ?? ''}
                        onChange={(e) => updateSpec('modelSource', { chatTemplatePath: e.target.value })}
                        className={inputCls}
                      />
                    </div>
                  </div>
                </details>
              </div>
            </Section>
          );
        })()}

        {/* Accelerator */}
        <Section title="Accelerator">
          <Field label="GPU">
            <select
              value={spec.accelerator?.type ?? ''}
              onChange={(e) => {
                const sku = GPU_CATALOG.find((g) => g.type === e.target.value);
                if (sku) {
                  updateSpec('accelerator', {
                    type: sku.type,
                    vramGb: sku.vramGb,
                    interconnect: sku.interconnect,
                  });
                } else {
                  updateSpec('accelerator', { type: '', vramGb: undefined, interconnect: '' });
                }
              }}
              className={inputCls}
            >
              <option value="">— Select GPU —</option>
              {GPU_CATALOG.map((g) => (
                <option key={g.type} value={g.type}>{g.label}</option>
              ))}
            </select>
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
          <Field label="SKU hint">
            <input
              type="text"
              value={spec.accelerator?.skuHint ?? ''}
              onChange={(e) => updateSpec('accelerator', { skuHint: e.target.value })}
              placeholder="aws/p5.48xlarge"
              className={inputCls}
            />
          </Field>
        </Section>

        {/* Engine (mega-group: engine selection + tuning) */}
        <Group title="Engine">
          <SubSection title="Engine">
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
            <Field label="Ready timeout (s)">
              <input
                type="number"
                value={spec.engine?.readyTimeoutSeconds ?? 600}
                onChange={(e) => updateSpec('engine', { readyTimeoutSeconds: Number(e.target.value) })}
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
          </SubSection>

          <EngineArgsSection
            bare
            args={spec.engineArgs ?? {}}
            onChange={(next) => setSpec((prev) => ({ ...prev, engineArgs: next }))}
          />

          <SubSection title="Parallelism">
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
          </SubSection>

          <SubSection title="Quantization">
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
          </SubSection>
        </Group>

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
      </fieldset>

      <div className="flex items-center gap-3 mt-6">
        {!isView && (
          <button
            onClick={handleSave}
            disabled={saving}
            className="inline-flex items-center gap-2 px-4 py-2 bg-slate-800 text-white text-sm rounded-lg hover:bg-slate-700 disabled:opacity-50"
          >
            {isEdit && <Save className="w-4 h-4" />}
            {saving
              ? 'Saving…'
              : isEdit
              ? 'Save Changes'
              : isClone
              ? 'Create New Version'
              : 'Create Template'}
          </button>
        )}
        <button
          onClick={onBack}
          className="px-4 py-2 text-sm text-gray-600 border border-gray-200 rounded-lg hover:bg-gray-50"
        >
          {isView ? 'Back' : 'Cancel'}
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

// Group wraps a logically related set of subsections inside one larger card.
function Group({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
      <h3 className="text-base font-medium mb-5">{title}</h3>
      <div className="divide-y divide-gray-100">{children}</div>
    </div>
  );
}

// SubSection is a Section without the card chrome — used inside Group.
function SubSection({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="py-5 first:pt-0 last:pb-0">
      <h4 className="text-xs font-medium uppercase tracking-wide text-gray-500 mb-3">{title}</h4>
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

type CustomRow = { id: string; key: string; value: string };

function newRowId(): string {
  return `row_${Math.random().toString(36).slice(2, 10)}`;
}

function EngineArgsSection({
  args,
  onChange,
  bare = false,
}: {
  args: Record<string, string>;
  onChange: (next: Record<string, string>) => void;
  bare?: boolean;
}) {
  // Custom flags live in local array state so editing key/value to empty
  // doesn't collapse the row (which the prior Record<string,string> design
  // forced — empty string can't address an entry, and two empty keys can't
  // coexist). Known-knob values still live in `args` and are synced through
  // setKey below. We re-derive customRows from props when an external load
  // happens (template fetch / clone) but skip our own emissions to avoid an
  // update loop.
  const [customRows, setCustomRows] = useState<CustomRow[]>([]);
  const lastEmittedRef = useRef<Record<string, string> | null>(null);

  useEffect(() => {
    if (lastEmittedRef.current === args) return;
    const fromProps = Object.entries(args)
      .filter(([k]) => !KNOWN_KEYS.has(k))
      .map(([k, v]) => ({ id: newRowId(), key: k, value: v }));
    setCustomRows(fromProps);
  }, [args]);

  // Compose final Record from current known-knob values plus custom rows
  // (skipping rows whose key is blank — they are placeholders the user is
  // still typing into).
  const compose = (rows: CustomRow[], knownOverride?: { key: string; value: string | undefined }) => {
    const next: Record<string, string> = {};
    for (const [k, v] of Object.entries(args)) {
      if (KNOWN_KEYS.has(k)) next[k] = v;
    }
    if (knownOverride) {
      const { key, value } = knownOverride;
      if (value === undefined || value === '') delete next[key];
      else next[key] = value;
    }
    for (const r of rows) {
      const k = r.key.trim();
      if (k) next[k] = r.value;
    }
    return next;
  };

  const emit = (rows: CustomRow[], knownOverride?: { key: string; value: string | undefined }) => {
    const next = compose(rows, knownOverride);
    lastEmittedRef.current = next;
    onChange(next);
  };

  const setKey = (key: string, value: string | undefined) => {
    emit(customRows, { key, value });
  };

  const updateRow = (id: string, patch: Partial<Pick<CustomRow, 'key' | 'value'>>) => {
    const next = customRows.map((r) => (r.id === id ? { ...r, ...patch } : r));
    setCustomRows(next);
    emit(next);
  };

  const addRow = () => {
    setCustomRows((rows) => [...rows, { id: newRowId(), key: '', value: '' }]);
    // Don't emit yet — a row with empty key contributes nothing to the wire format.
  };

  const removeRow = (id: string) => {
    const next = customRows.filter((r) => r.id !== id);
    setCustomRows(next);
    emit(next);
  };

  const wrapperClass = bare
    ? 'py-5 first:pt-0 last:pb-0'
    : 'bg-white rounded-xl shadow-sm border border-gray-100 p-6';
  const headerClass = bare
    ? 'text-xs font-medium uppercase tracking-wide text-gray-500'
    : 'text-sm';

  return (
    <div className={wrapperClass}>
      <div className="mb-3">
        <h3 className={headerClass}>Engine args</h3>
        {!bare && (
          <p className="text-xs text-gray-500 mt-1">
            Key/value flags forwarded to the engine. Common knobs are listed below; use Custom for engine-specific flags.
          </p>
        )}
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
            onClick={addRow}
            className="text-xs text-teal-600 hover:text-teal-700"
          >
            + Add
          </button>
        </div>
        {customRows.length === 0 ? (
          <p className="text-xs text-gray-400">No custom flags set.</p>
        ) : (
          <div className="space-y-2">
            {customRows.map((row) => (
              <div key={row.id} className="flex items-center gap-2">
                <input
                  type="text"
                  value={row.key}
                  placeholder="flag_name"
                  onChange={(e) => updateRow(row.id, { key: e.target.value })}
                  className={`${inputCls} flex-1 font-mono`}
                />
                <input
                  type="text"
                  value={row.value}
                  placeholder="value"
                  onChange={(e) => updateRow(row.id, { value: e.target.value })}
                  className={`${inputCls} flex-1 font-mono`}
                />
                <button
                  type="button"
                  onClick={() => removeRow(row.id)}
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
