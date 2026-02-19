import { useState } from 'react';
import { ArrowLeft, Copy, Calendar, Box, Home as HomeIcon, Globe, Moon, Settings2, Layers, ChevronDown } from 'lucide-react';
import { mockModels } from '../data/mockData';
import { copyToClipboard } from '../utils/clipboard';

interface ModelDetailProps {
  modelId: string | null;
  onBack: () => void;
}

const languageTabs = ['Python', 'Typescript', 'Java', 'Go', 'Shell'] as const;
type Language = typeof languageTabs[number];

const modeTabs = ['Chat', 'Completion'] as const;
type Mode = typeof modeTabs[number];

function getCodeSnippet(modelName: string, language: Language, mode: Mode): string {
  const modelSlug = modelName.toLowerCase().replace(/[\s.]+/g, '-').replace(/[()]/g, '').replace(/\[|\]/g, '');
  const endpoint = mode === 'Chat' ? 'chat/completions' : 'completions';

  switch (language) {
    case 'Python':
      return mode === 'Chat'
        ? `import requests
import json

url = "https://api.inference.ai/inference/v1/${endpoint}"

payload = {
    "model": "accounts/inference/models/${modelSlug}",
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
    "model": "accounts/inference/models/${modelSlug}",
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

    case 'Typescript':
      return mode === 'Chat'
        ? `const response = await fetch(
  "https://api.inference.ai/inference/v1/${endpoint}",
  {
    method: "POST",
    headers: {
      "Accept": "application/json",
      "Content-Type": "application/json",
      "Authorization": "Bearer <API_KEY>"
    },
    body: JSON.stringify({
      model: "accounts/inference/models/${modelSlug}",
      max_tokens: 16384,
      messages: [
        { role: "user", content: "Hello, how are you?" }
      ]
    })
  }
);

const data = await response.json();
console.log(data);`
        : `const response = await fetch(
  "https://api.inference.ai/inference/v1/${endpoint}",
  {
    method: "POST",
    headers: {
      "Accept": "application/json",
      "Content-Type": "application/json",
      "Authorization": "Bearer <API_KEY>"
    },
    body: JSON.stringify({
      model: "accounts/inference/models/${modelSlug}",
      max_tokens: 16384,
      prompt: "Once upon a time"
    })
  }
);

const data = await response.json();
console.log(data);`;

    case 'Shell':
      return mode === 'Chat'
        ? `curl -X POST "https://api.inference.ai/inference/v1/${endpoint}" \\
  -H "Accept: application/json" \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer <API_KEY>" \\
  -d '{
    "model": "accounts/inference/models/${modelSlug}",
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
    "model": "accounts/inference/models/${modelSlug}",
    "max_tokens": 16384,
    "prompt": "Once upon a time"
  }'`;

    case 'Java':
      return `// Java example
// Use your preferred HTTP client library
// Model: accounts/inference/models/${modelSlug}
// Endpoint: https://api.inference.ai/inference/v1/${endpoint}`;

    case 'Go':
      return `// Go example
// Use net/http package
// Model: accounts/inference/models/${modelSlug}
// Endpoint: https://api.inference.ai/inference/v1/${endpoint}`;

    default:
      return '';
  }
}

export function ModelDetail({ modelId, onBack }: ModelDetailProps) {
  const [activeLanguage, setActiveLanguage] = useState<Language>('Python');
  const [activeMode, setActiveMode] = useState<Mode>('Chat');
  const [copied, setCopied] = useState(false);

  const model = mockModels.find(m => m.id === modelId);

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

  const codeSnippet = getCodeSnippet(model.name, activeLanguage, activeMode);

  const handleCopy = () => {
    copyToClipboard(codeSnippet);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  const hasPricing = model.pricing.uncachedInput || model.pricing.cachedInput || model.pricing.output || model.pricing.perMinute || model.pricing.perStep || model.pricing.perEa || model.pricing.perTokens;

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
          <p className="text-sm text-gray-500">by {model.provider}</p>
        </div>
      </div>

      <div className="flex gap-8">
        {/* Left content */}
        <div className="flex-1 min-w-0">
          {/* Description */}
          <p className="text-sm text-gray-700 mb-8 leading-relaxed">{model.description}</p>

          {/* Estimated Cost */}
          {hasPricing && (
            <div className="mb-8">
              <div className="flex items-center justify-between mb-4">
                <div>
                  <h2 className="text-lg mb-1">Use Serverless</h2>
                  <p className="text-xs text-gray-500">Run queries immediately, pay only for usage</p>
                </div>
              </div>
              <div className="flex flex-wrap gap-6 mb-6">
                {model.pricing.uncachedInput && (
                  <div>
                    <div className="text-xl">
                      {model.pricing.uncachedInput.replace('/M', '')}
                      <span className="text-sm text-gray-500">/1M</span>
                    </div>
                    <div className="text-xs text-gray-500">Uncached input</div>
                  </div>
                )}
                {model.pricing.cachedInput && (
                  <div>
                    <div className="text-xl">
                      {model.pricing.cachedInput.replace('/M', '')}
                      <span className="text-sm text-gray-500">/1M</span>
                    </div>
                    <div className="text-xs text-gray-500">Cached input</div>
                  </div>
                )}
                {model.pricing.output && (
                  <div>
                    <div className="text-xl">
                      {model.pricing.output.replace('/M', '')}
                      <span className="text-sm text-gray-500">/1M</span>
                    </div>
                    <div className="text-xs text-gray-500">Output</div>
                  </div>
                )}
                {model.pricing.perMinute && (
                  <div>
                    <div className="text-xl">{model.pricing.perMinute}</div>
                    <div className="text-xs text-gray-500">Per minute</div>
                  </div>
                )}
                {model.pricing.perStep && (
                  <div>
                    <div className="text-xl">{model.pricing.perStep}</div>
                    <div className="text-xs text-gray-500">Per step</div>
                  </div>
                )}
                {model.pricing.perEa && (
                  <div>
                    <div className="text-xl">{model.pricing.perEa}</div>
                    <div className="text-xs text-gray-500">Per image</div>
                  </div>
                )}
                {model.pricing.perTokens && (
                  <div>
                    <div className="text-xl">{model.pricing.perTokens}</div>
                    <div className="text-xs text-gray-500">Per token batch</div>
                  </div>
                )}
              </div>
            </div>
          )}

          {/* Usage / Code Snippet */}
          <div>
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

            {/* Copy + Get API Key row */}
            <div className="flex items-center gap-3 mb-2">
              <button
                onClick={handleCopy}
                className="flex items-center gap-1.5 px-3 py-1.5 text-sm text-gray-600 hover:text-gray-900 border border-gray-200 rounded-lg hover:bg-gray-50 transition-colors"
              >
                <Copy className="w-3.5 h-3.5" />
                {copied ? 'Copied!' : 'Copy'}
              </button>
              <button className="px-3 py-1.5 text-sm border border-gray-200 rounded-lg hover:bg-gray-50 transition-colors">
                Get API Key
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

        {/* Right sidebar */}
        <div className="w-72 flex-shrink-0">
          {/* Metadata */}
          <div className="mb-8">
            <h3 className="text-base mb-4">Metadata</h3>
            <div className="space-y-3">
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
                  <Box className="w-4 h-4" /> Kind
                </span>
                <span>{model.metadata.kind}</span>
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
                  <span className="text-right truncate max-w-[150px]">{model.metadata.huggingFace}</span>
                </div>
              )}
            </div>
          </div>

          {/* Specification */}
          <div>
            <h3 className="text-base mb-4">Specification</h3>
            <div className="space-y-3">
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
    </div>
  );
}