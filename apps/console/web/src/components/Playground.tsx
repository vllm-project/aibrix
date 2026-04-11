import { useState, useRef, useEffect, useCallback } from 'react';
import {
  ChevronDown, ChevronRight, Copy, Maximize2, ExternalLink, Code2,
  Trash2, ImagePlus, Send, MessageCircle, Search, Check, X, Plus, Paperclip
} from 'lucide-react';
import { mockModels } from '../data/mockData';
import type { Model } from '../data/mockData';
import { copyToClipboard } from '../utils/clipboard';

// ─── Types ───────────────────────────────────────────────────────────
interface ChatMessage {
  role: 'user' | 'assistant';
  content: string;
  thinking?: string;
  stats?: { ttft: string; tokensPerSec: string };
}

interface FunctionDef {
  name: string;
  schema: string;
}

interface PlaygroundProps {
  onNavigateToModel?: (id: string) => void;
}

// ─── Mock streaming responses ────────────────────────────────────────
const mockThinking = `Let me analyze this question carefully.

The user is asking about my capabilities and how I compare to other models. I should:
- Be honest and balanced about strengths and weaknesses
- Highlight areas where I excel
- Acknowledge where other models may have advantages
- Suggest users evaluate based on their specific needs

GPT-4/GPT-4o are also excellent, particularly in certain reasoning tasks and have broader ecosystem integration. I should provide a balanced and accurate comparison.`;

const mockResponse = `I'm doing well, thanks for asking! Ready to help with whatever you're working on.

To answer your comparison question: **I'm an AI assistant**, and different models from different companies have varying strengths. "Performance" really depends on what you're doing.

**Where this model tends to shine:**

- **Long context**: Working with extremely long documents (up to millions of tokens), maintaining coherence across entire books, codebases, or lengthy conversation histories
- **Multimodal agentic capabilities**: Processing and reasoning across text, high-resolution images, and video content, including autonomous planning, execution, and tool integration with visual perception
- **Analysis & reasoning**: Strong performance on complex coding, mathematical reasoning, and deep document analysis tasks
- **Multilingual capabilities**: Robust support for multiple languages and cross-lingual tasks

**Where GPT models often excel:**

- **Ecosystem integration**: Deep integration with tools like DALL-E, web browsing, code interpreter, and the broader ChatGPT plugin ecosystem
- **Certain reasoning patterns**: GPT-4o and o1 models show particular strength in specific logical reasoning and step-by-step problem solving
- **Voice and real-time features**: Advanced audio and real-time conversation capabilities in their latest releases

**The honest answer:** For most everyday tasks, the differences are subtle and personal preference matters as much as benchmark scores. For specialized tasks—like analyzing a 500-page PDF or processing video content—my architecture might give me an edge, while for other use cases, GPT might feel more seamless.

The best way to compare is to test both on your specific workflow. What kind of tasks are you looking to handle?`;

const defaultFunctionSchema = `{
  "name": "chat",
  "description": "Chat with the model. The text to send to the model to generate a response and continue the chat",
  "parameters": {
    "type": "object",
    "properties": {
      "text": {
        "type": "string",
        "description": "The text to send to the model to generate a response and continue the chat"
      }
    },
    "required": [
      "text"
    ]
  }
}`;

// ─── Model Selector Dropdown ─────────────────────────────────────────
function ModelSelector({
  selectedModel,
  onSelect,
  isOpen,
  onToggle,
}: {
  selectedModel: Model;
  onSelect: (m: Model) => void;
  isOpen: boolean;
  onToggle: () => void;
}) {
  const [search, setSearch] = useState('');
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onToggle();
    };
    if (isOpen) document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [isOpen, onToggle]);

  const filtered = mockModels.filter(
    (m) =>
      m.categories.includes('LLM') &&
      (m.name.toLowerCase().includes(search.toLowerCase()) ||
        m.provider.toLowerCase().includes(search.toLowerCase()))
  );

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={onToggle}
        className="flex items-center gap-2.5 hover:bg-gray-50 rounded-lg px-2 py-1.5 transition-colors"
      >
        <div
          className={`w-8 h-8 ${selectedModel.iconBg} rounded-lg flex items-center justify-center ${selectedModel.iconTextColor} text-sm font-semibold`}
        >
          {selectedModel.iconText}
        </div>
        <div className="text-left">
          <div className="text-[10px] text-gray-400 uppercase tracking-wider leading-none">Model</div>
          <div className="text-sm flex items-center gap-1">
            {selectedModel.name}
            <ChevronDown className="w-3 h-3 text-gray-400" />
          </div>
        </div>
      </button>

      {isOpen && (
        <div className="absolute top-full left-0 mt-1 w-80 bg-white rounded-lg shadow-xl border border-gray-200 z-50 max-h-[420px] flex flex-col">
          <div className="p-3 border-b border-gray-100">
            <div className="relative">
              <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
              <input
                autoFocus
                type="text"
                placeholder="Search model"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                className="w-full pl-9 pr-3 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
              />
            </div>
          </div>
          <div className="overflow-y-auto flex-1 py-1">
            <div className="px-4 py-2 text-xs text-gray-500">Featured</div>
            {filtered.map((m) => (
              <button
                key={m.id}
                onClick={() => {
                  onSelect(m);
                  onToggle();
                  setSearch('');
                }}
                className="w-full flex items-center gap-3 px-4 py-2.5 hover:bg-gray-50 transition-colors"
              >
                <div
                  className={`w-8 h-8 ${m.iconBg} rounded-lg flex items-center justify-center ${m.iconTextColor} text-xs font-semibold flex-shrink-0`}
                >
                  {m.iconText}
                </div>
                <div className="flex-1 text-left min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="text-sm truncate">{m.name}</span>
                    {m.isNew && (
                      <span className="px-1.5 py-0.5 bg-teal-500 text-white text-[10px] rounded-full leading-none">
                        NEW
                      </span>
                    )}
                  </div>
                  <div className="text-xs text-gray-400">{m.provider}</div>
                </div>
                {m.id === selectedModel.id && (
                  <Check className="w-4 h-4 text-teal-500 flex-shrink-0" />
                )}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// ─── Function Schema Modal ──────────────────────────────────────────
function FunctionSchemaModal({
  onClose,
  onSave,
}: {
  onClose: () => void;
  onSave: (fn: FunctionDef) => void;
}) {
  const [schema, setSchema] = useState(defaultFunctionSchema);
  const [exampleOpen, setExampleOpen] = useState(false);

  const handleSave = () => {
    try {
      const parsed = JSON.parse(schema);
      onSave({ name: parsed.name || 'Untitled', schema });
      onClose();
    } catch {
      onSave({ name: 'custom_function', schema });
      onClose();
    }
  };

  return (
    <div className="fixed inset-0 bg-black/40 flex items-center justify-center z-50">
      <div className="bg-white rounded-xl shadow-2xl w-[480px] max-h-[90vh] flex flex-col">
        {/* Header */}
        <div className="flex items-start justify-between p-6 pb-3">
          <div>
            <h2 className="text-lg">Function Schema</h2>
            <p className="text-sm text-gray-500 mt-1">
              Add JSON schema for a function below. Click save when you're done.
            </p>
          </div>
          <button onClick={onClose} className="text-gray-400 hover:text-gray-600 p-1">
            <X className="w-4 h-4" />
          </button>
        </div>

        {/* Body */}
        <div className="px-6 pb-4 flex-1 overflow-y-auto">
          <div className="flex items-center justify-between mb-2">
            <label className="text-sm font-medium">JSON Schema</label>
            <div className="relative">
              <button
                onClick={() => setExampleOpen(!exampleOpen)}
                className="text-sm text-gray-600 border border-gray-200 rounded-lg px-3 py-1.5 flex items-center gap-1 hover:bg-gray-50"
              >
                Add an example function
                <ChevronDown className="w-3 h-3" />
              </button>
              {exampleOpen && (
                <div className="absolute right-0 mt-1 w-52 bg-white border border-gray-200 rounded-lg shadow-lg z-10 py-1">
                  <button
                    onClick={() => {
                      setSchema(defaultFunctionSchema);
                      setExampleOpen(false);
                    }}
                    className="w-full text-left px-4 py-2 text-sm hover:bg-gray-50"
                  >
                    Chat function
                  </button>
                  <button
                    onClick={() => {
                      setSchema(`{
  "name": "get_weather",
  "description": "Get current weather for a location",
  "parameters": {
    "type": "object",
    "properties": {
      "location": {
        "type": "string",
        "description": "City and state, e.g. San Francisco, CA"
      }
    },
    "required": ["location"]
  }
}`);
                      setExampleOpen(false);
                    }}
                    className="w-full text-left px-4 py-2 text-sm hover:bg-gray-50"
                  >
                    Weather function
                  </button>
                </div>
              )}
            </div>
          </div>
          <textarea
            value={schema}
            onChange={(e) => setSchema(e.target.value)}
            className="w-full h-72 border border-gray-200 rounded-lg p-3 text-sm font-mono resize-y focus:outline-none focus:ring-2 focus:ring-teal-500/30"
          />
        </div>

        {/* Footer */}
        <div className="flex items-center justify-end gap-3 px-6 py-4 border-t border-gray-100">
          <button
            onClick={onClose}
            className="px-8 py-2 border border-gray-300 rounded-lg text-sm hover:bg-gray-50 transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={handleSave}
            className="px-8 py-2 bg-amber-400 text-black rounded-lg text-sm hover:bg-amber-500 transition-colors"
          >
            Save
          </button>
        </div>
      </div>
    </div>
  );
}

// ─── Collapsible Thinking Block ──────────────────────────────────────
function ThinkingBlock({ text, isStreaming }: { text: string; isStreaming: boolean }) {
  const [collapsed, setCollapsed] = useState(false);
  const [displayedText, setDisplayedText] = useState('');
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  useEffect(() => {
    if (isStreaming) {
      let idx = 0;
      setDisplayedText('');
      intervalRef.current = setInterval(() => {
        idx += 3;
        if (idx >= text.length) {
          setDisplayedText(text);
          if (intervalRef.current) clearInterval(intervalRef.current);
        } else {
          setDisplayedText(text.slice(0, idx));
        }
      }, 15);
      return () => {
        if (intervalRef.current) clearInterval(intervalRef.current);
      };
    } else {
      setDisplayedText(text);
    }
  }, [text, isStreaming]);

  return (
    <div className="mb-3">
      <button
        onClick={() => setCollapsed(!collapsed)}
        className="flex items-center gap-2 bg-gray-100 rounded-lg px-4 py-2 w-full text-left hover:bg-gray-200 transition-colors"
      >
        {collapsed ? (
          <ChevronRight className="w-4 h-4 text-gray-500" />
        ) : (
          <ChevronDown className="w-4 h-4 text-gray-500" />
        )}
        <span className="text-sm text-gray-600">Thinking</span>
        {isStreaming && !collapsed && (
          <span className="ml-auto flex gap-0.5">
            <span className="w-1.5 h-1.5 bg-teal-400 rounded-full animate-bounce [animation-delay:0ms]" />
            <span className="w-1.5 h-1.5 bg-teal-400 rounded-full animate-bounce [animation-delay:150ms]" />
            <span className="w-1.5 h-1.5 bg-teal-400 rounded-full animate-bounce [animation-delay:300ms]" />
          </span>
        )}
      </button>
      {!collapsed && (
        <div className="mt-2 px-4 py-3 bg-gray-50 rounded-lg border border-gray-100 text-sm text-gray-600 whitespace-pre-wrap leading-relaxed max-h-48 overflow-y-auto">
          {displayedText}
          {isStreaming && displayedText.length < text.length && (
            <span className="inline-block w-0.5 h-4 bg-gray-500 ml-0.5 animate-pulse align-text-bottom" />
          )}
        </div>
      )}
    </div>
  );
}

// ─── Markdown-ish renderer ───────────────────────────────────────────
function RenderMarkdown({ text }: { text: string }) {
  const lines = text.split('\n');
  const elements: React.ReactNode[] = [];
  let i = 0;
  let listItems: string[] = [];

  const flushList = () => {
    if (listItems.length > 0) {
      elements.push(
        <ul key={`list-${i}`} className="list-disc pl-6 my-2 space-y-1">
          {listItems.map((item, idx) => (
            <li key={idx} className="text-sm leading-relaxed">
              <InlineFormat text={item} />
            </li>
          ))}
        </ul>
      );
      listItems = [];
    }
  };

  while (i < lines.length) {
    const line = lines[i];

    if (line.startsWith('- ')) {
      listItems.push(line.slice(2));
      i++;
      continue;
    }

    flushList();

    if (line.startsWith('**') && line.endsWith('**')) {
      // Bold heading-like line
      elements.push(
        <p key={i} className="text-sm mt-4 mb-1">
          <strong>{line.replace(/\*\*/g, '')}</strong>
        </p>
      );
    } else if (line.trim() === '') {
      elements.push(<div key={i} className="h-2" />);
    } else {
      elements.push(
        <p key={i} className="text-sm leading-relaxed">
          <InlineFormat text={line} />
        </p>
      );
    }
    i++;
  }
  flushList();

  return <div>{elements}</div>;
}

function InlineFormat({ text }: { text: string }) {
  const parts = text.split(/(\*\*[^*]+\*\*)/g);
  return (
    <>
      {parts.map((part, i) => {
        if (part.startsWith('**') && part.endsWith('**')) {
          return <strong key={i}>{part.slice(2, -2)}</strong>;
        }
        return <span key={i}>{part}</span>;
      })}
    </>
  );
}

// ─── Slider control ──────────────────────────────────────────────────
function OptionSlider({
  label,
  value,
  min,
  max,
  step,
  onChange,
}: {
  label: string;
  value: number;
  min: number;
  max: number;
  step: number;
  onChange: (v: number) => void;
}) {
  return (
    <div className="mb-4">
      <div className="flex items-center justify-between mb-1.5">
        <label className="text-sm text-gray-700">{label}</label>
        <input
          type="number"
          value={value}
          onChange={(e) => onChange(Number(e.target.value))}
          step={step}
          min={min}
          max={max}
          className="w-16 text-sm text-center border border-gray-200 rounded-md py-1 focus:outline-none focus:ring-1 focus:ring-teal-500"
        />
      </div>
      <input
        type="range"
        value={value}
        min={min}
        max={max}
        step={step}
        onChange={(e) => onChange(Number(e.target.value))}
        className="w-full h-1.5 appearance-none rounded-full bg-gray-200 accent-green-500 cursor-pointer
          [&::-webkit-slider-thumb]:appearance-none [&::-webkit-slider-thumb]:w-3.5 [&::-webkit-slider-thumb]:h-3.5
          [&::-webkit-slider-thumb]:rounded-full [&::-webkit-slider-thumb]:bg-green-500 [&::-webkit-slider-thumb]:cursor-pointer
          [&::-webkit-slider-thumb]:shadow-sm"
        style={{
          background: `linear-gradient(to right, #22c55e 0%, #22c55e ${((value - min) / (max - min)) * 100}%, #e5e7eb ${((value - min) / (max - min)) * 100}%, #e5e7eb 100%)`
        }}
      />
    </div>
  );
}

// ─── Main Playground ─────────────────────────────────────────────────
export function Playground({ onNavigateToModel }: PlaygroundProps) {
  const defaultModel = mockModels.find((m) => m.id === 'model-minimax-m2.5') || mockModels[0];

  const [selectedModel, setSelectedModel] = useState<Model>(defaultModel);
  const [modelSelectorOpen, setModelSelectorOpen] = useState(false);
  const [mode, setMode] = useState<'Chat' | 'Completion'>('Chat');
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [inputText, setInputText] = useState('');
  const [isStreaming, setIsStreaming] = useState(false);
  const [streamingContent, setStreamingContent] = useState('');
  const [streamingThinking, setStreamingThinking] = useState('');
  const [streamPhase, setStreamPhase] = useState<'idle' | 'thinking' | 'responding'>('idle');
  const [uploadedFiles, setUploadedFiles] = useState<{ name: string; url: string }[]>([]);

  // Options
  const [temperature, setTemperature] = useState(0.6);
  const [maxTokens, setMaxTokens] = useState(4096);
  const [topP, setTopP] = useState(1);
  const [topK, setTopK] = useState(40);
  const [presencePenalty, setPresencePenalty] = useState(0);
  const [frequencyPenalty, setFrequencyPenalty] = useState(0);
  const [stopWord, setStopWord] = useState('');
  const [contextBehavior, setContextBehavior] = useState('none');
  const [contextDropdownOpen, setContextDropdownOpen] = useState(false);
  const [echo, setEcho] = useState(false);
  const [functions, setFunctions] = useState<FunctionDef[]>([]);
  const [showFunctionModal, setShowFunctionModal] = useState(false);

  const chatEndRef = useRef<HTMLDivElement>(null);
  const streamIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const scrollToBottom = useCallback(() => {
    chatEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, []);

  useEffect(() => {
    scrollToBottom();
  }, [messages, streamingContent, scrollToBottom]);

  // Clear any in-flight stream interval when the component unmounts.
  useEffect(() => {
    return () => {
      if (streamIntervalRef.current) {
        clearInterval(streamIntervalRef.current);
        streamIntervalRef.current = null;
      }
    };
  }, []);

  const modelSlug = selectedModel.name.toLowerCase().replace(/[\s.]+/g, '-').replace(/[()]/g, '');

  const handleSend = () => {
    if (!inputText.trim() || isStreaming) return;

    const userMsg: ChatMessage = { role: 'user', content: inputText.trim() };
    setMessages((prev) => [...prev, userMsg]);
    setInputText('');
    setIsStreaming(true);
    setStreamPhase('thinking');
    setStreamingThinking('');
    setStreamingContent('');

    // Phase 1: Stream thinking
    let thinkIdx = 0;
    streamIntervalRef.current = setInterval(() => {
      thinkIdx += 4;
      if (thinkIdx >= mockThinking.length) {
        setStreamingThinking(mockThinking);
        if (streamIntervalRef.current) clearInterval(streamIntervalRef.current);

        // Phase 2: Stream response
        setTimeout(() => {
          setStreamPhase('responding');
          let respIdx = 0;
          streamIntervalRef.current = setInterval(() => {
            respIdx += 5;
            if (respIdx >= mockResponse.length) {
              setStreamingContent(mockResponse);
              if (streamIntervalRef.current) clearInterval(streamIntervalRef.current);
              // Finalize
              setMessages((prev) => [
                ...prev,
                {
                  role: 'assistant',
                  content: mockResponse,
                  thinking: mockThinking,
                  stats: { ttft: '1,955 ms', tokensPerSec: '137.26' },
                },
              ]);
              setStreamPhase('idle');
              setStreamingThinking('');
              setStreamingContent('');
              setIsStreaming(false);
            } else {
              setStreamingContent(mockResponse.slice(0, respIdx));
            }
          }, 12);
        }, 400);
      } else {
        setStreamingThinking(mockThinking.slice(0, thinkIdx));
      }
    }, 15);
  };

  const handleClear = () => {
    setMessages([]);
    setStreamingContent('');
    setStreamingThinking('');
    setStreamPhase('idle');
    setIsStreaming(false);
    if (streamIntervalRef.current) clearInterval(streamIntervalRef.current);
  };

  const handleCopyModelId = () => {
    copyToClipboard(modelSlug);
  };

  const handleFileUpload = (e: React.ChangeEvent<HTMLInputElement>) => {
    const files = e.target.files;
    if (!files) return;
    const newFiles: { name: string; url: string }[] = [];
    Array.from(files).forEach((file) => {
      const url = URL.createObjectURL(file);
      newFiles.push({ name: file.name, url });
    });
    setUploadedFiles((prev) => [...prev, ...newFiles]);
    // Reset the input so the same file can be selected again
    e.target.value = '';
  };

  const removeFile = (index: number) => {
    setUploadedFiles((prev) => {
      const removed = prev[index];
      if (removed) URL.revokeObjectURL(removed.url);
      return prev.filter((_, i) => i !== index);
    });
  };

  return (
    <div className="flex flex-col h-full">
      {/* ── Top Bar ───────────────────────────────────── */}
      <div className="flex items-center gap-3 px-4 py-2.5 border-b border-gray-200 bg-white flex-shrink-0">
        <ModelSelector
          selectedModel={selectedModel}
          onSelect={setSelectedModel}
          isOpen={modelSelectorOpen}
          onToggle={() => setModelSelectorOpen(!modelSelectorOpen)}
        />

        <button className="p-2 hover:bg-gray-100 rounded-lg transition-colors text-gray-500">
          <Maximize2 className="w-4 h-4" />
        </button>

        <button
          onClick={() => onNavigateToModel?.(selectedModel.id)}
          className="flex items-center gap-1.5 px-3 py-1.5 border border-gray-200 rounded-lg text-sm text-gray-600 hover:bg-gray-50 transition-colors"
        >
          Model Details
          <ExternalLink className="w-3.5 h-3.5" />
        </button>

        <div className="flex-1" />
      </div>

      {/* ── Body ──────────────────────────────────────── */}
      <div className="flex flex-1 overflow-hidden">
        {/* ── Left Options Panel ─────────────────────── */}
        <div className="w-72 border-r border-gray-200 bg-white overflow-y-auto flex-shrink-0 p-5">
          <h3 className="text-base mb-4">Options</h3>

          <OptionSlider label="Temperature" value={temperature} min={0} max={2} step={0.1} onChange={setTemperature} />
          <OptionSlider label="Max Tokens" value={maxTokens} min={1} max={16384} step={1} onChange={setMaxTokens} />
          <OptionSlider label="Top P" value={topP} min={0} max={1} step={0.01} onChange={setTopP} />
          <OptionSlider label="Top K" value={topK} min={0} max={100} step={1} onChange={setTopK} />
          <OptionSlider label="Presence Penalty" value={presencePenalty} min={-2} max={2} step={0.1} onChange={setPresencePenalty} />
          <OptionSlider label="Frequency Penalty" value={frequencyPenalty} min={-2} max={2} step={0.1} onChange={setFrequencyPenalty} />

          <div className="mb-4">
            <label className="text-sm text-gray-700 block mb-1.5">Stop</label>
            <input
              type="text"
              placeholder="Enter a stop word"
              value={stopWord}
              onChange={(e) => setStopWord(e.target.value)}
              className="w-full px-3 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-teal-500"
            />
          </div>

          <div className="mb-4">
            <label className="text-sm text-gray-700 block mb-1.5">Context Length Exceeded Behavior</label>
            <div className="relative">
              <button
                onClick={() => setContextDropdownOpen(!contextDropdownOpen)}
                className="w-full flex items-center justify-between px-3 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50"
              >
                <span className="capitalize">{contextBehavior === 'none' ? 'None' : contextBehavior}</span>
                <ChevronDown className="w-4 h-4 text-gray-400" />
              </button>
              {contextDropdownOpen && (
                <div className="absolute top-full left-0 mt-1 w-full bg-white border border-gray-200 rounded-lg shadow-lg z-10 py-1">
                  {['none', 'truncate', 'error'].map((opt) => (
                    <button
                      key={opt}
                      onClick={() => {
                        setContextBehavior(opt);
                        setContextDropdownOpen(false);
                      }}
                      className={`w-full text-left px-4 py-2 text-sm hover:bg-gray-100 capitalize ${contextBehavior === opt ? 'bg-gray-100' : ''}`}
                    >
                      {opt === 'none' ? 'None' : opt}
                    </button>
                  ))}
                </div>
              )}
            </div>
          </div>

          <div className="flex items-center justify-between mb-5">
            <label className="text-sm text-gray-700">Echo</label>
            <button
              onClick={() => setEcho(!echo)}
              className={`w-10 h-5 rounded-full transition-colors relative ${echo ? 'bg-teal-500' : 'bg-gray-300'}`}
            >
              <div
                className={`absolute top-0.5 w-4 h-4 rounded-full bg-white shadow transition-transform ${echo ? 'translate-x-5' : 'translate-x-0.5'}`}
              />
            </button>
          </div>

          <div className="border-t border-gray-100 pt-4">
            <h4 className="text-sm font-medium mb-0.5">Function Calling</h4>
            <p className="text-xs text-gray-400 mb-3">JsonSchema definitions sent to the model</p>

            <div className="text-xs text-gray-500 mb-1">Name</div>
            <div className="border-t border-gray-100 pt-2">
              {functions.length === 0 ? (
                <p className="text-xs text-gray-400 mb-3">No functions currently added</p>
              ) : (
                <div className="space-y-2 mb-3">
                  {functions.map((fn, idx) => (
                    <div key={idx} className="flex items-center justify-between text-sm">
                      <span className="truncate">{fn.name}</span>
                      <button
                        onClick={() => setFunctions((prev) => prev.filter((_, i) => i !== idx))}
                        className="text-gray-400 hover:text-red-500"
                      >
                        <X className="w-3 h-3" />
                      </button>
                    </div>
                  ))}
                </div>
              )}
              <button
                onClick={() => setShowFunctionModal(true)}
                className="inline-flex items-center gap-1.5 px-4 py-2 border border-gray-300 rounded-lg text-sm hover:bg-gray-50 transition-colors"
              >
                Add Function
              </button>
            </div>
          </div>
        </div>

        {/* ── Chat Area ─────────────────────────────── */}
        <div className="flex-1 flex flex-col bg-white overflow-hidden">
          {/* Mode tabs + clear */}
          <div className="flex items-center justify-between px-6 pt-4 pb-2 flex-shrink-0">
            <div className="inline-flex border border-gray-200 rounded-lg overflow-hidden">
              {(['Chat', 'Completion'] as const).map((m) => (
                <button
                  key={m}
                  onClick={() => setMode(m)}
                  className={`px-4 py-1.5 text-sm transition-colors ${
                    mode === m ? 'bg-gray-900 text-white' : 'bg-white text-gray-600 hover:bg-gray-50'
                  }`}
                >
                  {m}
                </button>
              ))}
            </div>
            <button
              onClick={handleClear}
              className="p-2 text-gray-400 hover:text-gray-600 hover:bg-gray-100 rounded-lg transition-colors"
              title="Clear chat"
            >
              <Trash2 className="w-4 h-4" />
            </button>
          </div>

          {/* Messages */}
          <div className="flex-1 overflow-y-auto px-6 py-4">
            {messages.length === 0 && streamPhase === 'idle' && (
              <div className="flex items-center justify-center h-full">
                <div className="bg-gray-50 rounded-xl p-8 max-w-lg text-center">
                  <h3 className="text-lg mb-2">
                    Try out {selectedModel.name} (via our chat API)
                  </h3>
                  <p className="text-sm text-gray-500">
                    Kick the tires, see how {selectedModel.name} performs on AIBrix
                  </p>
                </div>
              </div>
            )}

            {messages.map((msg, idx) => (
              <div key={idx} className="mb-6">
                <div className="flex items-start gap-3">
                  {msg.role === 'user' ? (
                    <div className="w-8 h-8 rounded-full bg-gray-100 flex items-center justify-center flex-shrink-0">
                      <svg className="w-4 h-4 text-gray-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                        <path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2" />
                        <circle cx="12" cy="7" r="4" />
                      </svg>
                    </div>
                  ) : (
                    <div className={`w-8 h-8 rounded-full ${selectedModel.iconBg} flex items-center justify-center flex-shrink-0 ${selectedModel.iconTextColor} text-xs font-semibold`}>
                      {selectedModel.iconText}
                    </div>
                  )}
                  <div className="flex-1 min-w-0">
                    {msg.role === 'user' ? (
                      <p className="text-sm leading-relaxed">{msg.content}</p>
                    ) : (
                      <>
                        {msg.thinking && (
                          <ThinkingBlock text={msg.thinking} isStreaming={false} />
                        )}
                        <RenderMarkdown text={msg.content} />
                        {msg.stats && (
                          <p className="text-xs text-gray-400 mt-3 text-right">
                            {msg.stats.ttft} ttft • {msg.stats.tokensPerSec} tokens/s
                          </p>
                        )}
                      </>
                    )}
                  </div>
                </div>
              </div>
            ))}

            {/* Streaming assistant response */}
            {isStreaming && (
              <div className="mb-6">
                <div className="flex items-start gap-3">
                  <div className={`w-8 h-8 rounded-full ${selectedModel.iconBg} flex items-center justify-center flex-shrink-0 ${selectedModel.iconTextColor} text-xs font-semibold`}>
                    {selectedModel.iconText}
                  </div>
                  <div className="flex-1 min-w-0">
                    {(streamPhase === 'thinking' || streamPhase === 'responding') && streamingThinking && (
                      <ThinkingBlock
                        text={streamPhase === 'thinking' ? streamingThinking : mockThinking}
                        isStreaming={streamPhase === 'thinking'}
                      />
                    )}
                    {streamPhase === 'responding' && (
                      <div>
                        <RenderMarkdown text={streamingContent} />
                        <span className="inline-block w-0.5 h-4 bg-gray-800 ml-0.5 animate-pulse align-text-bottom" />
                      </div>
                    )}
                  </div>
                </div>
              </div>
            )}

            <div ref={chatEndRef} />
          </div>

          {/* Input bar */}
          <div className="px-6 py-3 border-t border-gray-100 flex-shrink-0">
            {/* Uploaded files preview */}
            {uploadedFiles.length > 0 && (
              <div className="flex flex-wrap gap-2 mb-2 px-2">
                {uploadedFiles.map((file, idx) => (
                  <div key={idx} className="relative group">
                    <img
                      src={file.url}
                      alt={file.name}
                      className="w-16 h-16 rounded-lg object-cover border border-gray-200"
                    />
                    <button
                      onClick={() => removeFile(idx)}
                      className="absolute -top-1.5 -right-1.5 w-5 h-5 bg-gray-800 text-white rounded-full flex items-center justify-center opacity-0 group-hover:opacity-100 transition-opacity"
                    >
                      <X className="w-3 h-3" />
                    </button>
                    <div className="absolute bottom-0 left-0 right-0 bg-black/50 text-white text-[9px] px-1 py-0.5 rounded-b-lg truncate">
                      {file.name}
                    </div>
                  </div>
                ))}
              </div>
            )}
            <div className="flex items-center gap-2 bg-gray-50 rounded-full px-2 py-1 border border-gray-200">
              <input
                ref={fileInputRef}
                type="file"
                accept="image/*"
                multiple
                onChange={handleFileUpload}
                className="hidden"
              />
              <button
                onClick={() => fileInputRef.current?.click()}
                className="flex items-center gap-1.5 px-3 py-1.5 text-sm text-teal-600 hover:bg-teal-50 rounded-full transition-colors flex-shrink-0"
              >
                <ImagePlus className="w-4 h-4" />
                Upload Images
              </button>
              <input
                type="text"
                placeholder="Type a message"
                value={inputText}
                onChange={(e) => setInputText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault();
                    handleSend();
                  }
                }}
                className="flex-1 bg-transparent text-sm py-2 focus:outline-none"
              />
              <button
                onClick={handleSend}
                disabled={!inputText.trim() || isStreaming}
                className="p-2 text-gray-400 hover:text-gray-600 disabled:opacity-30 transition-colors"
              >
                <Send className="w-4 h-4" />
              </button>
              <button className="p-2 bg-teal-600 text-white rounded-full hover:bg-teal-700 transition-colors flex-shrink-0">
                <MessageCircle className="w-4 h-4" />
              </button>
            </div>
          </div>
        </div>
      </div>

      {/* ── Function Schema Modal ────────────────────── */}
      {showFunctionModal && (
        <FunctionSchemaModal
          onClose={() => setShowFunctionModal(false)}
          onSave={(fn) => setFunctions((prev) => [...prev, fn])}
        />
      )}
    </div>
  );
}