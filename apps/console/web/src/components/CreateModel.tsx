import { useState, useRef, useEffect } from 'react';
import { ChevronLeft, Save, ChevronDown, Check, Monitor, AudioLines, ImageIcon, Eye, Grid3X3, Link2, Video } from 'lucide-react';
import { createModel } from '../utils/api';
import type { CreateModelRequest } from '../utils/api';
import type { ModelCategory } from '../data/mockData';
import { CATEGORY_OPTIONS, CATEGORY_COLORS } from '../data/mockData';

interface CreateModelProps {
  onBack: () => void;
  onSaved: (modelId: string) => void;
}

const CATEGORY_ICONS: Record<ModelCategory, (cls: string) => React.ReactNode> = {
  LLM: (cls) => <Monitor className={cls} />,
  Audio: (cls) => <AudioLines className={cls} />,
  Image: (cls) => <ImageIcon className={cls} />,
  Video: (cls) => <Video className={cls} />,
  Vision: (cls) => <Eye className={cls} />,
  Embedding: (cls) => <Grid3X3 className={cls} />,
  Reranks: (cls) => <Link2 className={cls} />,
};

const ICON_COLOR_PALETTES = [
  { bg: 'bg-blue-100', text: 'text-blue-700' },
  { bg: 'bg-green-100', text: 'text-green-700' },
  { bg: 'bg-orange-100', text: 'text-orange-700' },
  { bg: 'bg-red-100', text: 'text-red-700' },
  { bg: 'bg-violet-100', text: 'text-violet-700' },
  { bg: 'bg-cyan-100', text: 'text-cyan-700' },
  { bg: 'bg-pink-100', text: 'text-pink-700' },
  { bg: 'bg-amber-100', text: 'text-amber-700' },
  { bg: 'bg-emerald-100', text: 'text-emerald-700' },
  { bg: 'bg-indigo-100', text: 'text-indigo-700' },
];

function hashNameToPalette(name: string): { bg: string; text: string } {
  let hash = 0;
  for (let i = 0; i < name.length; i++) {
    hash = ((hash << 5) - hash + name.charCodeAt(i)) | 0;
  }
  return ICON_COLOR_PALETTES[Math.abs(hash) % ICON_COLOR_PALETTES.length];
}

const inputClsBase =
  'w-full px-3 py-2 bg-white border rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500';
const inputCls = `${inputClsBase} border-gray-200`;

function Section({
  title,
  required,
  children,
}: {
  title: string;
  required?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
      <h3 className="text-sm mb-4">
        {title}
        {required && (
          <>
            <span className="text-red-500 ml-0.5" aria-hidden="true">*</span>
            <span className="sr-only"> (required)</span>
          </>
        )}
      </h3>
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">{children}</div>
    </div>
  );
}

function Field({
  label,
  wide,
  required,
  children,
}: {
  label: string;
  wide?: boolean;
  required?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className={wide ? 'sm:col-span-2 lg:col-span-3' : ''}>
      <label className="block text-xs text-gray-600 mb-1">
        {label}
        {required && (
          <>
            <span className="text-red-500 ml-0.5" aria-hidden="true">*</span>
            <span className="sr-only"> (required)</span>
          </>
        )}
      </label>
      {children}
    </div>
  );
}

function CheckboxDropdown({
  options,
  selected,
  onToggle,
}: {
  options: { value: string; label: string }[];
  selected: Record<string, boolean>;
  onToggle: (value: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const handleClickOutside = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    if (open) document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, [open]);

  const selectedLabels = options
    .filter(o => selected[o.value])
    .map(o => o.label);

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen(v => !v)}
        className={`${inputCls} text-left flex items-center justify-between`}
      >
        <span className={selectedLabels.length === 0 ? 'text-gray-400' : 'text-gray-700'}>
          {selectedLabels.length === 0 ? 'Select features...' : selectedLabels.join(', ')}
        </span>
        <ChevronDown className="w-4 h-4 text-gray-400 shrink-0" />
      </button>
      {open && (
        <div className="absolute z-10 mt-1 w-full bg-white border border-gray-200 rounded-lg shadow-lg py-1">
          {options.map(opt => (
            <button
              key={opt.value}
              type="button"
              onClick={() => onToggle(opt.value)}
              className="w-full px-3 py-2 text-sm text-left hover:bg-gray-50 flex items-center gap-2"
            >
              <span className={`w-4 h-4 border rounded flex items-center justify-center ${
                selected[opt.value]
                  ? 'bg-slate-800 border-slate-800'
                  : 'border-gray-300 bg-white'
              }`}>
                {selected[opt.value] && <Check className="w-3 h-3 text-white" />}
              </span>
              {opt.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

export function CreateModel({ onBack, onSaved }: CreateModelProps) {
  // Identity
  const [name, setName] = useState('');
  const [servingName, setServingName] = useState('');

  // Classification
  const [categories, setCategories] = useState<ModelCategory[]>([]);
  const [tagsInput, setTagsInput] = useState('');
  const [isNew, setIsNew] = useState(false);

  // Description
  const [description, setDescription] = useState('');
  const [contextLength, setContextLength] = useState('');

  // Metadata
  const [state, setState] = useState('Ready');
  const [providerName, setProviderName] = useState('');
  const [huggingFace, setHuggingFace] = useState('');

  // Specification
  const [parameters, setParameters] = useState('');
  const [calibrated, setCalibrated] = useState(false);
  const [mixtureOfExperts, setMixtureOfExperts] = useState(false);

  // Icon
  const [iconBg, setIconBg] = useState('');
  const [iconText, setIconText] = useState('');
  const [iconTextColor, setIconTextColor] = useState('');

  // Pricing
  const [uncachedInput, setUncachedInput] = useState('');
  const [cachedInput, setCachedInput] = useState('');
  const [outputPrice, setOutputPrice] = useState('');
  const [perMinute, setPerMinute] = useState('');
  const [perImage, setPerImage] = useState('');

  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const toggleCategory = (cat: ModelCategory) => {
    setCategories(prev =>
      prev.includes(cat) ? prev.filter(c => c !== cat) : [...prev, cat],
    );
  };

  const handleSave = async () => {
    setError(null);

    if (!name.trim()) { setError('Model name is required'); return; }
    if (categories.length === 0) { setError('At least one category is required'); return; }
    if (!description.trim()) { setError('Description is required'); return; }
    if (!providerName.trim()) { setError('Provider name is required'); return; }
    if (!parameters.trim()) { setError('Parameters is required'); return; }

    const tags = tagsInput
      .split(',')
      .map(t => t.trim())
      .filter(t => t.length > 0);

    const req: CreateModelRequest = {
      name: name.trim(),
      servingName: servingName.trim() || undefined,
      description: description.trim(),
      contextLength: contextLength.trim() || undefined,
      categories,
      tags: tags.length > 0 ? tags : undefined,
      isNew,
      iconBg: iconBg.trim() || hashNameToPalette(name.trim()).bg,
      iconText: iconText.trim() || name.trim().slice(0, 2).toUpperCase() || undefined,
      iconTextColor: iconTextColor.trim() || hashNameToPalette(name.trim()).text,
      metadata: {
        state,
        providerName: providerName.trim(),
        ...(huggingFace.trim() && { huggingFace: huggingFace.trim() }),
      },
      specification: {
        calibrated,
        mixtureOfExperts,
        parameters: parameters.trim() || undefined,
      },
    };

    const hasPricing = uncachedInput || cachedInput || outputPrice || perMinute || perImage;
    if (hasPricing) {
      req.pricing = {
        ...(uncachedInput && { uncachedInput }),
        ...(cachedInput && { cachedInput }),
        ...(outputPrice && { output: outputPrice }),
        ...(perMinute && { perMinute }),
        ...(perImage && { perImage }),
      };
    }

    setSaving(true);
    try {
      const created = await createModel(req);
      onSaved(created.id);
    } catch (err) {
      setError(`Save failed: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="p-8 max-w-4xl mx-auto">
      {/* Header */}
      <div className="mb-6">
        <button
          onClick={onBack}
          className="inline-flex items-center gap-1 text-sm text-gray-500 hover:text-gray-700 mb-3"
        >
          <ChevronLeft className="w-4 h-4" />
          Model Library
        </button>
        <h1 className="text-2xl">Create Model</h1>
        <p className="text-sm text-gray-500 mt-1">
          Register a new model in the library.
        </p>
      </div>

      {/* Error banner */}
      {error && (
        <div className="mb-4 p-3 bg-red-50 border border-red-200 rounded-lg text-sm text-red-700">
          {error}
        </div>
      )}

      <div className="space-y-6">
        {/* Identity */}
        <Section title="Identity" required>
          <Field label="Name" required>
            <input
              type="text"
              value={name}
              onChange={e => setName(e.target.value)}
              placeholder="e.g. Qwen3-Omni-30B-A3B-Captioner"
              className={inputCls}
            />
          </Field>
          <Field label="Serving Name">
            <input
              type="text"
              value={servingName}
              onChange={e => setServingName(e.target.value)}
              placeholder="e.g. qwen3-omni-30b-a3b-captioner_data.aiplatform"
              className={inputCls}
            />
          </Field>
        </Section>

        {/* Classification */}
        <Section title="Classification" required>
          <Field label="Categories" required wide>
            <div className="flex flex-wrap gap-2">
              {CATEGORY_OPTIONS.map(cat => {
                const selected = categories.includes(cat);
                const iconCls = selected
                  ? 'w-3.5 h-3.5 text-white'
                  : `w-3.5 h-3.5 ${CATEGORY_COLORS[cat]}`;
                return (
                  <button
                    key={cat}
                    type="button"
                    onClick={() => toggleCategory(cat)}
                    className={`inline-flex items-center gap-1.5 px-3 py-1.5 text-xs rounded-full border transition-colors ${
                      selected
                        ? 'bg-slate-800 text-white border-slate-800'
                        : 'bg-white text-gray-600 border-gray-200 hover:border-gray-300'
                    }`}
                  >
                    {CATEGORY_ICONS[cat](iconCls)}
                    {cat}
                  </button>
                );
              })}
            </div>
          </Field>
          <Field label="Tags">
            <input
              type="text"
              value={tagsInput}
              onChange={e => setTagsInput(e.target.value)}
              placeholder="Serverless, Tunable (comma-separated)"
              className={inputCls}
            />
          </Field>
          <Field label="New">
            <label className="inline-flex items-center gap-2 mt-1">
              <input
                type="checkbox"
                checked={isNew}
                onChange={e => setIsNew(e.target.checked)}
                className="rounded border-gray-300"
              />
              <span className="text-sm text-gray-600">Show &quot;NEW&quot; badge</span>
            </label>
          </Field>
        </Section>

        {/* Description */}
        <Section title="Description" required>
          <Field label="Description" required wide>
            <textarea
              value={description}
              onChange={e => setDescription(e.target.value)}
              placeholder="Model description..."
              rows={3}
              className={`${inputCls} resize-none`}
            />
          </Field>
        </Section>

        {/* Metadata */}
        <Section title="Metadata" required>
          <Field label="State">
            <select
              value={state}
              onChange={e => setState(e.target.value)}
              className={inputCls}
            >
              <option value="Ready">Ready</option>
              <option value="Initializing">Initializing</option>
              <option value="Deprecated">Deprecated</option>
            </select>
          </Field>
          <Field label="Provider Name" required>
            <input
              type="text"
              value={providerName}
              onChange={e => setProviderName(e.target.value)}
              placeholder="e.g. Alibaba, Meta, DeepSeek"
              className={inputCls}
            />
          </Field>
          <Field label="Hugging Face">
            <input
              type="text"
              value={huggingFace}
              onChange={e => setHuggingFace(e.target.value)}
              placeholder="e.g. Qwen/Qwen3-Omni-30B-A3B-Captioner"
              className={inputCls}
            />
          </Field>
        </Section>

        {/* Specification */}
        <Section title="Specification" required>
          <Field label="Parameters" required>
            <input
              type="text"
              value={parameters}
              onChange={e => setParameters(e.target.value)}
              placeholder="e.g. 70B, 671B"
              className={inputCls}
            />
          </Field>
          <Field label="Context Length">
            <input
              type="text"
              value={contextLength}
              onChange={e => setContextLength(e.target.value)}
              placeholder="e.g. 32k, 128k"
              className={inputCls}
            />
          </Field>
          <Field label="Model Features">
            <CheckboxDropdown
              options={[
                { value: 'calibrated', label: 'Calibrated' },
                { value: 'moe', label: 'Mixture of Experts' },
              ]}
              selected={{
                calibrated,
                moe: mixtureOfExperts,
              }}
              onToggle={(key) => {
                if (key === 'calibrated') setCalibrated(v => !v);
                if (key === 'moe') setMixtureOfExperts(v => !v);
              }}
            />
          </Field>
        </Section>

        {/* Advanced: Icon + Pricing */}
        <details className="bg-white rounded-xl shadow-sm border border-gray-100">
          <summary className="p-6 text-sm cursor-pointer select-none">
            Advanced
          </summary>
          <div className="px-6 pb-6 space-y-6">
            {/* Icon */}
            <div>
              <h4 className="text-xs font-medium uppercase tracking-wide text-gray-500 mb-3">Icon</h4>
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
                <Field label="Background">
                  <input
                    type="text"
                    value={iconBg}
                    onChange={e => setIconBg(e.target.value)}
                    placeholder="e.g. bg-blue-100"
                    className={inputCls}
                  />
                </Field>
                <Field label="Text">
                  <input
                    type="text"
                    value={iconText}
                    onChange={e => setIconText(e.target.value)}
                    placeholder="1-2 char abbreviation"
                    className={inputCls}
                  />
                </Field>
                <Field label="Text Color">
                  <input
                    type="text"
                    value={iconTextColor}
                    onChange={e => setIconTextColor(e.target.value)}
                    placeholder="e.g. text-blue-700"
                    className={inputCls}
                  />
                </Field>
              </div>
            </div>

            {/* Pricing */}
            <div>
              <h4 className="text-xs font-medium uppercase tracking-wide text-gray-500 mb-3">Pricing</h4>
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
                <Field label="Uncached Input">
                  <input
                    type="text"
                    value={uncachedInput}
                    onChange={e => setUncachedInput(e.target.value)}
                    placeholder="e.g. $0.20/M"
                    className={inputCls}
                  />
                </Field>
                <Field label="Cached Input">
                  <input
                    type="text"
                    value={cachedInput}
                    onChange={e => setCachedInput(e.target.value)}
                    placeholder="e.g. $0.03/M"
                    className={inputCls}
                  />
                </Field>
                <Field label="Output">
                  <input
                    type="text"
                    value={outputPrice}
                    onChange={e => setOutputPrice(e.target.value)}
                    placeholder="e.g. $0.20/M"
                    className={inputCls}
                  />
                </Field>
                <Field label="Per Minute">
                  <input
                    type="text"
                    value={perMinute}
                    onChange={e => setPerMinute(e.target.value)}
                    placeholder="e.g. $0.0032/minute"
                    className={inputCls}
                  />
                </Field>
                <Field label="Per Image">
                  <input
                    type="text"
                    value={perImage}
                    onChange={e => setPerImage(e.target.value)}
                    placeholder="e.g. $0.04/ea"
                    className={inputCls}
                  />
                </Field>
              </div>
            </div>
          </div>
        </details>
      </div>

      {/* Actions */}
      <div className="mt-6 flex items-center justify-between">
        <button
          onClick={handleSave}
          disabled={saving}
          className="inline-flex items-center gap-2 px-6 py-2.5 text-sm bg-slate-800 text-white rounded-lg hover:bg-slate-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
        >
          <Save className="w-4 h-4" />
          {saving ? 'Creating...' : 'Create Model'}
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
