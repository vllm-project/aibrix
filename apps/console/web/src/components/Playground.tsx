import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Check,
  ChevronDown,
  Copy,
  ExternalLink,
  Search,
  Send,
  Trash2,
} from 'lucide-react';
import type { Deployment } from '../data/mockData';
import { listDeployments } from '../utils/api';
import { copyToClipboard } from '../utils/clipboard';
import {
  callableDeployments,
  preferredDeployment,
  streamPlaygroundChat,
} from '../utils/playground';

interface ChatMessage {
  role: 'user' | 'assistant';
  content: string;
}

interface PlaygroundProps {
  initialDeploymentId?: string;
  onNavigateToDeployment?: (id: string) => void;
}

function deploymentInitials(deployment: Deployment): string {
  const source = deployment.baseModel || deployment.name;
  const words = source.trim().split(/\s+/).filter(Boolean);
  return (words.length > 1
    ? `${words[0]?.[0] ?? ''}${words[1]?.[0] ?? ''}`
    : source.slice(0, 2)).toUpperCase();
}

function DeploymentSelector({
  selectedDeployment,
  deployments,
  onSelect,
  isOpen,
  onToggle,
}: {
  selectedDeployment: Deployment;
  deployments: Deployment[];
  onSelect: (deployment: Deployment) => void;
  isOpen: boolean;
  onToggle: () => void;
}) {
  const [search, setSearch] = useState('');
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const closeOnOutsideClick = (event: MouseEvent) => {
      if (ref.current && !ref.current.contains(event.target as Node)) onToggle();
    };
    if (isOpen) document.addEventListener('mousedown', closeOnOutsideClick);
    return () => document.removeEventListener('mousedown', closeOnOutsideClick);
  }, [isOpen, onToggle]);

  const query = search.trim().toLowerCase();
  const filtered = deployments.filter((deployment) => !query || [
    deployment.name,
    deployment.baseModel,
    deployment.servingName,
  ].some((value) => value.toLowerCase().includes(query)));

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={onToggle}
        className="flex items-center gap-2.5 hover:bg-gray-50 rounded-lg px-2 py-1.5 transition-colors"
        aria-expanded={isOpen}
      >
        <div className="w-8 h-8 bg-teal-50 rounded-lg flex items-center justify-center text-teal-700 text-xs font-semibold">
          {deploymentInitials(selectedDeployment)}
        </div>
        <div className="text-left min-w-0">
          <div className="text-[10px] text-gray-400 uppercase tracking-wider leading-none">Deployment</div>
          <div className="text-sm flex items-center gap-1 max-w-64">
            <span className="truncate">{selectedDeployment.name}</span>
            <ChevronDown className="w-3 h-3 text-gray-400 flex-shrink-0" />
          </div>
        </div>
      </button>

      {isOpen && (
        <div className="absolute top-full left-0 mt-1 w-96 bg-white rounded-lg shadow-xl border border-gray-200 z-50 max-h-[420px] flex flex-col">
          <div className="p-3 border-b border-gray-100">
            <div className="relative">
              <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
              <input
                autoFocus
                type="text"
                placeholder="Search deployments"
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                className="w-full pl-9 pr-3 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
              />
            </div>
          </div>
          <div className="overflow-y-auto flex-1 py-1">
            {filtered.length === 0 && (
              <p className="px-4 py-5 text-sm text-center text-gray-500">No matching deployments.</p>
            )}
            {filtered.map((deployment) => (
              <button
                key={deployment.id}
                onClick={() => {
                  onSelect(deployment);
                  onToggle();
                  setSearch('');
                }}
                className="w-full flex items-center gap-3 px-4 py-2.5 hover:bg-gray-50 transition-colors"
              >
                <div className="w-8 h-8 bg-teal-50 rounded-lg flex items-center justify-center text-teal-700 text-xs font-semibold flex-shrink-0">
                  {deploymentInitials(deployment)}
                </div>
                <div className="flex-1 text-left min-w-0">
                  <div className="text-sm text-gray-900 truncate">{deployment.name}</div>
                  <div className="text-xs text-gray-400 truncate">{deployment.servingName}</div>
                </div>
                {deployment.id === selectedDeployment.id && (
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
  onChange: (value: number) => void;
}) {
  const percent = ((value - min) / (max - min)) * 100;
  return (
    <div className="mb-4">
      <div className="flex items-center justify-between mb-1.5">
        <label className="text-sm text-gray-700">{label}</label>
        <input
          type="number"
          value={value}
          onChange={(event) => onChange(Number(event.target.value))}
          step={step}
          min={min}
          max={max}
          className="w-20 text-sm text-center border border-gray-200 rounded-md py-1 focus:outline-none focus:ring-1 focus:ring-teal-500"
        />
      </div>
      <input
        type="range"
        value={value}
        min={min}
        max={max}
        step={step}
        onChange={(event) => onChange(Number(event.target.value))}
        className="w-full h-1.5 appearance-none rounded-full bg-gray-200 accent-teal-600 cursor-pointer"
        style={{
          background: `linear-gradient(to right, #0d9488 0%, #0d9488 ${percent}%, #e5e7eb ${percent}%, #e5e7eb 100%)`,
        }}
      />
    </div>
  );
}

function RenderMessage({ text }: { text: string }) {
  return (
    <div className="text-sm leading-relaxed whitespace-pre-wrap break-words">
      {text}
    </div>
  );
}

export function Playground({ initialDeploymentId, onNavigateToDeployment }: PlaygroundProps) {
  const [deployments, setDeployments] = useState<Deployment[]>([]);
  const [deploymentsLoading, setDeploymentsLoading] = useState(true);
  const [deploymentsError, setDeploymentsError] = useState<string | null>(null);
  const [selectedDeployment, setSelectedDeployment] = useState<Deployment | null>(null);
  const [selectorOpen, setSelectorOpen] = useState(false);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [inputText, setInputText] = useState('');
  const [isStreaming, setIsStreaming] = useState(false);
  const [streamingContent, setStreamingContent] = useState('');
  const [requestError, setRequestError] = useState<string | null>(null);
  const [temperature, setTemperature] = useState(0.6);
  const [maxTokens, setMaxTokens] = useState(4096);
  const [topP, setTopP] = useState(1);
  const [topK, setTopK] = useState(40);
  const [presencePenalty, setPresencePenalty] = useState(0);
  const [frequencyPenalty, setFrequencyPenalty] = useState(0);
  const [stopWord, setStopWord] = useState('');
  const [copied, setCopied] = useState(false);
  const chatEndRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let active = true;

    setDeploymentsLoading(true);
    setDeploymentsError(null);
    listDeployments()
      .then((items) => {
        if (!active) return;

        const callable = callableDeployments(items);
        setDeployments(callable);
        setSelectedDeployment(preferredDeployment(callable, initialDeploymentId));
      })
      .catch((error: unknown) => {
        if (!active) return;

        setDeploymentsError(error instanceof Error ? error.message : 'Failed to load deployments');
      })
      .finally(() => {
        if (active) setDeploymentsLoading(false);
      });

    return () => {
      active = false;
    };
  }, [initialDeploymentId]);

  const scrollToBottom = useCallback(() => {
    chatEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, []);

  useEffect(() => {
    scrollToBottom();
  }, [messages, streamingContent, scrollToBottom]);

  const clearConversation = () => {
    if (isStreaming) return;
    setMessages([]);
    setStreamingContent('');
    setRequestError(null);
  };

  const selectDeployment = (deployment: Deployment) => {
    setSelectedDeployment(deployment);
    clearConversation();
  };

  const handleCopyServingName = () => {
    if (!selectedDeployment) return;
    copyToClipboard(selectedDeployment.servingName).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    }).catch(() => setCopied(false));
  };

  const handleSend = async () => {
    const prompt = inputText.trim();
    if (!prompt || !selectedDeployment || isStreaming) return;

    const userMessage: ChatMessage = { role: 'user', content: prompt };
    const requestMessages = [...messages, userMessage].map((message) => ({
      role: message.role,
      content: message.content,
    }));
    setMessages((current) => [...current, userMessage]);
    setInputText('');
    setRequestError(null);
    setStreamingContent('');
    setIsStreaming(true);

    try {
      const response = await streamPlaygroundChat({
        model: selectedDeployment.servingName,
        messages: requestMessages,
        temperature,
        max_tokens: maxTokens,
        top_p: topP,
        top_k: topK,
        presence_penalty: presencePenalty,
        frequency_penalty: frequencyPenalty,
        stop: stopWord.trim() ? [stopWord.trim()] : undefined,
      }, (delta) => setStreamingContent((current) => current + delta));
      if (!response) throw new Error('The deployment returned an empty response');
      setMessages((current) => [...current, { role: 'assistant', content: response }]);
    } catch (error) {
      setRequestError(error instanceof Error ? error.message : 'The Playground request failed');
    } finally {
      setStreamingContent('');
      setIsStreaming(false);
    }
  };

  if (!selectedDeployment) {
    return (
      <div className="flex-1 flex items-center justify-center p-8">
        <div className="max-w-md text-center">
          <h2 className="text-lg text-gray-900 mb-2">
            {deploymentsLoading ? 'Loading deployments...' : 'No callable deployments'}
          </h2>
          <p className={`text-sm ${deploymentsError ? 'text-red-600' : 'text-gray-500'}`}>
            {deploymentsError
              ? `Failed to load deployments: ${deploymentsError}`
              : !deploymentsLoading && 'Create a deployment and wait for it to become Ready before using Playground.'}
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center gap-3 px-4 py-2.5 border-b border-gray-200 bg-white flex-shrink-0">
        <DeploymentSelector
          selectedDeployment={selectedDeployment}
          deployments={deployments}
          onSelect={selectDeployment}
          isOpen={selectorOpen}
          onToggle={() => setSelectorOpen((open) => !open)}
        />
        <button
          onClick={handleCopyServingName}
          className="p-2 hover:bg-gray-100 rounded-lg transition-colors text-gray-500"
          title="Copy serving name"
          aria-label="Copy serving name"
        >
          <Copy className="w-4 h-4" />
        </button>
        {copied && <span className="text-xs text-teal-600">Copied</span>}
        <button
          onClick={() => onNavigateToDeployment?.(selectedDeployment.id)}
          className="flex items-center gap-1.5 px-3 py-1.5 border border-gray-200 rounded-lg text-sm text-gray-600 hover:bg-gray-50 transition-colors"
        >
          Deployment details
          <ExternalLink className="w-3.5 h-3.5" />
        </button>
      </div>

      <div className="flex flex-1 overflow-hidden">
        <aside className="w-72 border-r border-gray-200 bg-white overflow-y-auto flex-shrink-0 p-5">
          <h3 className="text-base text-gray-900 mb-1">Options</h3>
          <p className="text-xs text-gray-400 mb-5">Sent with each chat completion request.</p>
          <OptionSlider label="Temperature" value={temperature} min={0} max={2} step={0.1} onChange={setTemperature} />
          <OptionSlider label="Max Tokens" value={maxTokens} min={1} max={16384} step={1} onChange={setMaxTokens} />
          <OptionSlider label="Top P" value={topP} min={0} max={1} step={0.01} onChange={setTopP} />
          <OptionSlider label="Top K" value={topK} min={0} max={100} step={1} onChange={setTopK} />
          <OptionSlider label="Presence Penalty" value={presencePenalty} min={-2} max={2} step={0.1} onChange={setPresencePenalty} />
          <OptionSlider label="Frequency Penalty" value={frequencyPenalty} min={-2} max={2} step={0.1} onChange={setFrequencyPenalty} />
          <div>
            <label className="text-sm text-gray-700 block mb-1.5">Stop sequence</label>
            <input
              type="text"
              placeholder="Optional"
              value={stopWord}
              onChange={(event) => setStopWord(event.target.value)}
              className="w-full px-3 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-teal-500"
            />
          </div>
        </aside>

        <section className="flex-1 flex flex-col bg-white overflow-hidden">
          <div className="flex items-center justify-between px-6 pt-4 pb-2 flex-shrink-0">
            <div>
              <div className="text-sm font-medium text-gray-900">Chat</div>
              <code className="text-xs text-gray-400">{selectedDeployment.servingName}</code>
            </div>
            <button
              onClick={clearConversation}
              disabled={isStreaming}
              className="p-2 text-gray-400 hover:text-gray-600 hover:bg-gray-100 rounded-lg transition-colors disabled:opacity-40"
              title="Clear chat"
              aria-label="Clear chat"
            >
              <Trash2 className="w-4 h-4" />
            </button>
          </div>

          <div className="flex-1 overflow-y-auto px-6 py-4">
            {messages.length === 0 && !isStreaming && (
              <div className="flex items-center justify-center h-full">
                <div className="bg-gray-50 rounded-xl p-8 max-w-lg text-center">
                  <h3 className="text-lg text-gray-900 mb-2">Try {selectedDeployment.name}</h3>
                  <p className="text-sm text-gray-500">
                    Responses are streamed from the deployment through the AIBrix Gateway.
                  </p>
                </div>
              </div>
            )}

            {messages.map((message, index) => (
              <div key={`${message.role}-${index}`} className="mb-6 flex items-start gap-3">
                <div className={`w-8 h-8 rounded-full flex items-center justify-center flex-shrink-0 text-xs font-semibold ${
                  message.role === 'user' ? 'bg-gray-100 text-gray-600' : 'bg-teal-50 text-teal-700'
                }`}>
                  {message.role === 'user' ? 'You' : deploymentInitials(selectedDeployment)}
                </div>
                <div className="flex-1 min-w-0 pt-1">
                  <RenderMessage text={message.content} />
                </div>
              </div>
            ))}

            {isStreaming && (
              <div className="mb-6 flex items-start gap-3">
                <div className="w-8 h-8 rounded-full bg-teal-50 text-teal-700 flex items-center justify-center flex-shrink-0 text-xs font-semibold">
                  {deploymentInitials(selectedDeployment)}
                </div>
                <div className="flex-1 min-w-0 pt-1">
                  {streamingContent ? <RenderMessage text={streamingContent} /> : (
                    <span className="inline-flex gap-1" aria-label="Waiting for response">
                      <span className="w-1.5 h-1.5 bg-teal-500 rounded-full animate-bounce" />
                      <span className="w-1.5 h-1.5 bg-teal-500 rounded-full animate-bounce [animation-delay:150ms]" />
                      <span className="w-1.5 h-1.5 bg-teal-500 rounded-full animate-bounce [animation-delay:300ms]" />
                    </span>
                  )}
                </div>
              </div>
            )}
            {requestError && (
              <div className="mb-4 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
                {requestError}
              </div>
            )}
            <div ref={chatEndRef} />
          </div>

          <div className="px-6 py-3 border-t border-gray-100 flex-shrink-0">
            <div className="flex items-end gap-2 bg-gray-50 rounded-2xl px-3 py-2 border border-gray-200 focus-within:border-teal-500 focus-within:ring-2 focus-within:ring-teal-500/20">
              <textarea
                rows={1}
                placeholder="Type a message"
                value={inputText}
                onChange={(event) => setInputText(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter' && !event.shiftKey) {
                    event.preventDefault();
                    void handleSend();
                  }
                }}
                className="flex-1 bg-transparent text-sm py-1.5 resize-none focus:outline-none max-h-32"
              />
              <button
                onClick={() => void handleSend()}
                disabled={!inputText.trim() || isStreaming}
                className="p-2 bg-teal-600 text-white rounded-full hover:bg-teal-700 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                title="Send message"
                aria-label="Send message"
              >
                <Send className="w-4 h-4" />
              </button>
            </div>
          </div>
        </section>
      </div>
    </div>
  );
}
