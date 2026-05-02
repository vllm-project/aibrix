import { useState, useEffect, useMemo } from 'react';
import {
  ArrowLeft, Copy, Calendar, Home as HomeIcon, Globe, Moon, Settings2, Layers, Plus, Cpu, Edit3, Trash2,
  ChevronLeft, ChevronRight,
} from 'lucide-react';
import { getModel, listModelDeploymentTemplates, deleteModelDeploymentTemplate } from '../utils/api';
import type { Model } from '../data/mockData';
import type { ModelDeploymentTemplate } from '../utils/api';
import { copyToClipboard } from '../utils/clipboard';

interface ModelDetailProps {
  modelId: string | null;
  onBack: () => void;
  onCreateTemplate?: (modelId: string) => void;
  onEditTemplate?: (modelId: string, templateId: string) => void;
}

const languageTabs = ['curl', 'Python'] as const;
type Language = typeof languageTabs[number];

const modeTabs = ['Chat', 'Completion'] as const;
type Mode = typeof modeTabs[number];

function getCodeSnippet(modelName: string, language: Language, mode: Mode): string {
  const endpoint = mode === 'Chat' ? 'chat/completions' : 'completions';

  switch (language) {
    case 'Python':
      return mode === 'Chat'
        ? `import requests
import json

url = "https://api.inference.ai/inference/v1/${endpoint}"

payload = {
    "model": "${modelName}",
    "max_tokens": 16384,
    "top_p": 1,
    "top_k": 40,
    "presence_penalty": 0,
    "frequency_penalty": 0,
    "temperature": 0.6,
    "messages": [
        {
            "role": "user",
            "content": "Hello, how are you?"
        }
    ]
}

headers = {
    "Accept": "application/json",
    "Content-Type": "application/json",
    "Authorization": "Bearer <API_KEY>"
}

response = requests.post(url, json=payload, headers=headers)
print(response.json())`
        : `import requests
import json

url = "https://api.inference.ai/inference/v1/${endpoint}"

payload = {
    "model": "${modelName}",
    "max_tokens": 16384,
    "prompt": "Once upon a time",
    "temperature": 0.6
}

headers = {
    "Accept": "application/json",
    "Content-Type": "application/json",
    "Authorization": "Bearer <API_KEY>"
}

response = requests.post(url, json=payload, headers=headers)
print(response.json())`;

    case 'curl':
      return mode === 'Chat'
        ? `curl -X POST "https://api.inference.ai/inference/v1/${endpoint}" \\
  -H "Accept: application/json" \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer <API_KEY>" \\
  -d '{
    "model": "${modelName}",
    "max_tokens": 16384,
    "messages": [
      {"role": "user", "content": "Hello, how are you?"}
    ]
  }'`
        : `curl -X POST "https://api.inference.ai/inference/v1/${endpoint}" \\
  -H "Accept: application/json" \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer <API_KEY>" \\
  -d '{
    "model": "${modelName}",
    "max_tokens": 16384,
    "prompt": "Once upon a time"
  }'`;

    default:
      return '';
  }
}

export function ModelDetail({ modelId, onBack, onCreateTemplate, onEditTemplate }: ModelDetailProps) {
  const [activeLanguage, setActiveLanguage] = useState<Language>('curl');
  const [activeMode, setActiveMode] = useState<Mode>('Chat');
  const [copied, setCopied] = useState(false);
  const [model, setModel] = useState<Model | null>(null);
  const [loading, setLoading] = useState(true);
  const [templates, setTemplates] = useState<ModelDeploymentTemplate[]>([]);
  const [templatesLoading, setTemplatesLoading] = useState(false);
  const [templatesPage, setTemplatesPage] = useState(0);
  const TEMPLATES_PAGE_SIZE = 8;

  const refreshTemplates = (id: string) => {
    setTemplatesLoading(true);
    listModelDeploymentTemplates(id)
      .then(setTemplates)
      .catch(err => {
        console.error('Failed to fetch templates:', err);
        setTemplates([]);
      })
      .finally(() => setTemplatesLoading(false));
  };

  useEffect(() => {
    if (!modelId) {
      setLoading(false);
      return;
    }
    setLoading(true);
    getModel(modelId)
      .then(m => setModel(m))
      .catch(err => {
        console.error('Failed to fetch model:', err);
        setModel(null);
      })
      .finally(() => setLoading(false));
    refreshTemplates(modelId);
  }, [modelId]);

  // Reset page when template list changes (e.g. after delete).
  useEffect(() => {
    setTemplatesPage(0);
  }, [templates.length]);

  // Hooks must run on every render, so keep useMemo above any early returns below.
  // Sort newest version first; localeCompare with numeric handles "v1.10" > "v1.9" correctly.
  const sortedTemplates = useMemo(
    () => [...templates].sort((a, b) =>
      b.version.localeCompare(a.version, undefined, { numeric: true, sensitivity: 'base' }),
    ),
    [templates],
  );

  const handleDeleteTemplate = async (templateId: string) => {
    if (!modelId) return;
    if (!window.confirm('Delete this deployment template?')) return;
    try {
      await deleteModelDeploymentTemplate(modelId, templateId);
      refreshTemplates(modelId);
    } catch (err) {
      console.error('Failed to delete template:', err);
    }
  };

  if (loading) {
    return (
      <div className="p-8">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-600 hover:text-gray-900 mb-4">
          <ArrowLeft className="w-4 h-4" />
          Back to Model Library
        </button>
        <p className="text-sm text-gray-400">Loading model...</p>
      </div>
    );
  }

  if (!model) {
    return (
      <div className="p-8">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-600 hover:text-gray-900 mb-4">
          <ArrowLeft className="w-4 h-4" />
          Back to Model Library
        </button>
        <p className="text-gray-500">Model not found.</p>
      </div>
    );
  }

  const totalPages = Math.max(1, Math.ceil(sortedTemplates.length / TEMPLATES_PAGE_SIZE));
  const safePage = Math.min(templatesPage, totalPages - 1);
  const pagedTemplates = sortedTemplates.slice(
    safePage * TEMPLATES_PAGE_SIZE,
    (safePage + 1) * TEMPLATES_PAGE_SIZE,
  );

  const codeSnippet = getCodeSnippet(model.name, activeLanguage, activeMode);

  const handleCopy = () => {
    copyToClipboard(codeSnippet);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div className="p-8">
      {/* Back button */}
      <button
        onClick={onBack}
        className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-6"
      >
        <ArrowLeft className="w-4 h-4" />
        Back to Model Library
      </button>

      {/* Model header */}
      <div className="flex items-center gap-4 mb-6">
        <div className={`w-12 h-12 ${model.iconBg} rounded-xl flex items-center justify-center ${model.iconTextColor} text-lg font-semibold`}>
          {model.iconText}
        </div>
        <div>
          <div className="flex items-center gap-3">
            <h1 className="text-2xl">{model.name}</h1>
            {model.isNew && (
              <span className="px-2 py-0.5 bg-teal-500 text-white text-xs rounded-full">NEW</span>
            )}
          </div>
          <p className="text-sm text-gray-500">by {model.metadata.providerName}</p>
        </div>
      </div>

      <div>
        {/* Description */}
        <p className="text-sm text-gray-700 mb-8 leading-relaxed">{model.description}</p>

        {/* Model Specs */}
        <div className="mb-8">
          <div className="mb-4">
            <h2 className="text-lg mb-1">Model Specs</h2>
            <p className="text-xs text-gray-500">Provenance and high-level architecture details.</p>
          </div>
          <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
            <div className="border border-gray-200 rounded-xl p-4">
              <h3 className="text-sm font-medium mb-3 text-gray-700">Metadata</h3>
              <div className="space-y-2.5">
                <div className="flex items-center justify-between text-sm">
                  <span className="flex items-center gap-2 text-gray-500">
                    <Settings2 className="w-4 h-4" /> State
                  </span>
                  <span className="flex items-center gap-1.5">
                    <span className="w-2 h-2 rounded-full bg-green-500" />
                    {model.metadata.state}
                  </span>
                </div>
                <div className="flex items-center justify-between text-sm">
                  <span className="flex items-center gap-2 text-gray-500">
                    <Calendar className="w-4 h-4" /> Created on
                  </span>
                  <span>{model.metadata.createdOn}</span>
                </div>
                <div className="flex items-center justify-between text-sm">
                  <span className="flex items-center gap-2 text-gray-500">
                    <HomeIcon className="w-4 h-4" /> Provider
                  </span>
                  <span>{model.metadata.providerName}</span>
                </div>
                {model.metadata.huggingFace && (
                  <div className="flex items-center justify-between text-sm">
                    <span className="flex items-center gap-2 text-gray-500">
                      <Globe className="w-4 h-4" /> Hugging Face
                    </span>
                    <span className="text-right truncate max-w-[180px]">{model.metadata.huggingFace}</span>
                  </div>
                )}
              </div>
            </div>
            <div className="border border-gray-200 rounded-xl p-4">
              <h3 className="text-sm font-medium mb-3 text-gray-700">Specification</h3>
              <div className="space-y-2.5">
                <div className="flex items-center justify-between text-sm">
                  <span className="flex items-center gap-2 text-gray-500">
                    <Moon className="w-4 h-4" /> Calibrated
                  </span>
                  <span>{model.specification.calibrated ? 'Yes' : 'No'}</span>
                </div>
                <div className="flex items-center justify-between text-sm">
                  <span className="flex items-center gap-2 text-gray-500">
                    <Layers className="w-4 h-4" /> Mixture-of-Experts
                  </span>
                  <span>{model.specification.mixtureOfExperts ? 'Yes' : 'No'}</span>
                </div>
                <div className="flex items-center justify-between text-sm">
                  <span className="flex items-center gap-2 text-gray-500">
                    <Settings2 className="w-4 h-4" /> Parameters
                  </span>
                  <span>{model.specification.parameters}</span>
                </div>
              </div>
            </div>
          </div>
        </div>

        {/* Deployment Templates */}
        <div className="mb-8">
          <div className="flex items-center justify-between mb-4">
            <div>
              <h2 className="text-lg mb-1">Deployment Templates</h2>
              <p className="text-xs text-gray-500">
                Pre-configured engine + accelerator combinations for online serving and batch jobs.
              </p>
            </div>
            {onCreateTemplate && modelId && (
              <button
                onClick={() => onCreateTemplate(modelId)}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm bg-slate-800 text-white rounded-lg hover:bg-slate-700 transition-colors"
              >
                <Plus className="w-3.5 h-3.5" />
                Create Template
              </button>
            )}
          </div>
          {templatesLoading ? (
            <p className="text-xs text-gray-400">Loading templates...</p>
          ) : templates.length === 0 ? (
            <div className="border border-dashed border-gray-200 rounded-xl p-6 text-center">
              <p className="text-sm text-gray-500 mb-1">No deployment templates yet.</p>
              <p className="text-xs text-gray-400">
                Create one to capture engine, accelerator, and tuning settings for this model.
              </p>
            </div>
          ) : (
            <>
              <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                {pagedTemplates.map((t) => {
                  const a = t.spec?.accelerator;
                  const e = t.spec?.engine;
                  const p = t.spec?.parallelism;
                  const q = t.spec?.quantization;
                  return (
                    <div
                      key={t.id}
                      className="border border-gray-200 rounded-xl p-4 hover:border-gray-300 transition-colors"
                    >
                      <div className="flex items-start justify-between mb-2">
                        <div>
                          <div className="flex items-center gap-2">
                            <span className="text-sm">{t.name}</span>
                            <span className="text-xs text-gray-400">{t.version}</span>
                            <span className={`px-1.5 py-0.5 text-[10px] rounded ${
                              t.status === 'active' ? 'bg-green-50 text-green-700' :
                              t.status === 'deprecated' ? 'bg-gray-100 text-gray-600' :
                              'bg-amber-50 text-amber-700'
                            }`}>
                              {t.status}
                            </span>
                          </div>
                        </div>
                        <div className="flex items-center gap-1">
                          {onEditTemplate && modelId && (
                            <button
                              onClick={() => onEditTemplate(modelId, t.id)}
                              className="p-1 text-gray-400 hover:text-gray-700"
                              title="Edit"
                            >
                              <Edit3 className="w-3.5 h-3.5" />
                            </button>
                          )}
                          <button
                            onClick={() => handleDeleteTemplate(t.id)}
                            className="p-1 text-gray-400 hover:text-red-600"
                            title="Delete"
                          >
                            <Trash2 className="w-3.5 h-3.5" />
                          </button>
                        </div>
                      </div>
                      <div className="grid grid-cols-2 gap-2 text-xs text-gray-600">
                        <div className="flex items-center gap-1.5">
                          <Cpu className="w-3 h-3 text-gray-400" />
                          {e?.type ?? '—'} {e?.version}
                        </div>
                        <div>
                          {a?.type ?? '—'} × {a?.count ?? '?'}
                          {a?.interconnect ? ` (${a.interconnect})` : ''}
                        </div>
                        <div>
                          TP={p?.tp ?? 1} PP={p?.pp ?? 1} DP={p?.dp ?? 1}
                        </div>
                        <div>
                          {q?.weight ? `weight=${q.weight}` : 'bf16'}
                          {q?.kvCache ? ` / kv=${q.kvCache}` : ''}
                        </div>
                      </div>
                      {t.spec?.supportedEndpoints && t.spec.supportedEndpoints.length > 0 && (
                        <div className="flex flex-wrap gap-1 mt-2">
                          {t.spec.supportedEndpoints.map((ep) => (
                            <span key={ep} className="px-1.5 py-0.5 bg-gray-50 text-[10px] text-gray-600 rounded font-mono">
                              {ep}
                            </span>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
              {totalPages > 1 && (
                <div className="flex items-center justify-end gap-2 mt-4 text-sm text-gray-600">
                  <span className="text-xs text-gray-500">
                    Page {safePage + 1} of {totalPages} ({sortedTemplates.length} templates)
                  </span>
                  <button
                    onClick={() => setTemplatesPage((p) => Math.max(0, p - 1))}
                    disabled={safePage === 0}
                    className="p-1 rounded border border-gray-200 disabled:opacity-30 hover:bg-gray-50"
                    title="Previous page"
                  >
                    <ChevronLeft className="w-4 h-4" />
                  </button>
                  <button
                    onClick={() => setTemplatesPage((p) => Math.min(totalPages - 1, p + 1))}
                    disabled={safePage >= totalPages - 1}
                    className="p-1 rounded border border-gray-200 disabled:opacity-30 hover:bg-gray-50"
                    title="Next page"
                  >
                    <ChevronRight className="w-4 h-4" />
                  </button>
                </div>
              )}
            </>
          )}
        </div>

        {/* Examples */}
        <div>
          <div className="mb-4">
            <h2 className="text-lg mb-1">Examples</h2>
            <p className="text-xs text-gray-500">Sample request snippets you can copy into your client.</p>
          </div>

          {/* Language + Mode tabs */}
          <div className="flex flex-wrap items-center gap-3 mb-3">
            <div className="inline-flex border border-gray-200 rounded-lg overflow-hidden">
              {languageTabs.map((lang) => (
                <button
                  key={lang}
                  onClick={() => setActiveLanguage(lang)}
                  className={`px-3 py-1.5 text-sm transition-colors ${
                    activeLanguage === lang
                      ? 'bg-slate-800 text-white'
                      : 'bg-white text-gray-500 hover:bg-gray-50'
                  }`}
                >
                  {lang}
                </button>
              ))}
            </div>

            <div className="inline-flex border border-gray-200 rounded-lg overflow-hidden">
              {modeTabs.map((mode) => (
                <button
                  key={mode}
                  onClick={() => setActiveMode(mode)}
                  className={`px-3 py-1.5 text-sm transition-colors ${
                    activeMode === mode
                      ? 'bg-slate-800 text-white'
                      : 'bg-white text-gray-500 hover:bg-gray-50'
                  }`}
                >
                  {mode}
                </button>
              ))}
            </div>
          </div>

          {/* Copy row */}
          <div className="flex items-center gap-3 mb-2">
            <button
              onClick={handleCopy}
              className="flex items-center gap-1.5 px-3 py-1.5 text-sm text-gray-600 hover:text-gray-900 border border-gray-200 rounded-lg hover:bg-gray-50 transition-colors"
            >
              <Copy className="w-3.5 h-3.5" />
              {copied ? 'Copied!' : 'Copy'}
            </button>
          </div>

          {/* Code block */}
          <div className="bg-slate-900 rounded-xl p-4 overflow-x-auto">
            <pre className="text-sm text-gray-300 font-mono whitespace-pre leading-relaxed">
              {codeSnippet}
            </pre>
          </div>
        </div>
      </div>
    </div>
  );
}
