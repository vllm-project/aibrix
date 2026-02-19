import { useState } from 'react';
import { ChevronLeft, Search, Check, Info, Rocket } from 'lucide-react';

interface CreateDeploymentProps {
  onBack: () => void;
}

export function CreateDeployment({ onBack }: CreateDeploymentProps) {
  const [deploymentName, setDeploymentName] = useState('gsm8k-math-sample');
  const [selectedModel, setSelectedModel] = useState('Qwen2.5 72B');
  const [showModelDropdown, setShowModelDropdown] = useState(false);
  const [region, setRegion] = useState('GLOBAL');
  const [selectedTab, setSelectedTab] = useState<'model-library' | 'custom'>('model-library');
  const [selectedShape, setSelectedShape] = useState<'throughput' | 'minimal' | 'fast'>('minimal');
  const [acceleratorType, setAcceleratorType] = useState('NVIDIA A100 80GB');
  const [acceleratorCount, setAcceleratorCount] = useState('Auto');
  const [quantization, setQuantization] = useState('BF16');
  const [enableAutoScaling, setEnableAutoScaling] = useState(true);
  const [minReplicas, setMinReplicas] = useState('1');
  const [maxReplicas, setMaxReplicas] = useState('1');
  const [scaleToZero, setScaleToZero] = useState('60');
  const [scaleUp, setScaleUp] = useState('30');
  const [scaleDown, setScaleDown] = useState('10');
  const [optimizeLongPrompts, setOptimizeLongPrompts] = useState(false);
  const [enableMultiLoRA, setEnableMultiLoRA] = useState(true);

  const models = [
    'Qwen2.5 32B',
    'Qwen2.5 72B',
    'Qwen2.5 72B Instruct',
    'Qwen2.5 72B',
    'Qwen2.5-Coder 15B Instruct',
    'Qwen2.5-Coder 15B',
    'DeepSeek v3.2',
  ];

  return (
    <div className="p-8">
      <div className="mb-6">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" />
          Deployments / <span className="text-gray-400">Create</span>
        </button>
        
        <h1 className="text-2xl mb-2">Create Deployment</h1>
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-3 gap-6">
        <div className="xl:col-span-2 space-y-6">
          {/* Deployment Name */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <label className="block text-sm mb-2">Deployment Name</label>
            <input
              type="text"
              value={deploymentName}
              onChange={(e) => setDeploymentName(e.target.value)}
              className="w-full px-4 py-2 bg-teal-50/50 border border-teal-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
            />
          </div>

          {/* Base Model and Region */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Base Model
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <div className="relative">
                  <button
                    onClick={() => setShowModelDropdown(!showModelDropdown)}
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-left flex items-center justify-between hover:bg-gray-50 text-sm"
                  >
                    <div className="flex items-center gap-2">
                      <div className="w-5 h-5 rounded-full bg-teal-50 flex items-center justify-center">
                        <div className="w-2 h-2 rounded-full bg-teal-500"></div>
                      </div>
                      <span>{selectedModel}</span>
                    </div>
                    <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 9l-7 7-7-7" />
                    </svg>
                  </button>
                  
                  {showModelDropdown && (
                    <div className="absolute z-10 w-full mt-2 bg-white border border-gray-200 rounded-xl shadow-lg max-h-64 overflow-y-auto">
                      <div className="p-2 border-b border-gray-100">
                        <div className="flex items-center gap-2 bg-gray-100 rounded-lg p-0.5 mb-2">
                          <button
                            onClick={() => setSelectedTab('model-library')}
                            className={`flex-1 px-3 py-1.5 rounded-md text-sm transition-colors ${
                              selectedTab === 'model-library'
                                ? 'bg-white text-gray-900 shadow-sm'
                                : 'text-gray-500'
                            }`}
                          >
                            Model Library
                          </button>
                          <button
                            onClick={() => setSelectedTab('custom')}
                            className={`flex-1 px-3 py-1.5 rounded-md text-sm transition-colors ${
                              selectedTab === 'custom'
                                ? 'bg-white text-gray-900 shadow-sm'
                                : 'text-gray-500'
                            }`}
                          >
                            Custom Models
                          </button>
                        </div>
                        <div className="relative">
                          <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
                          <input
                            type="text"
                            placeholder="Search"
                            className="w-full pl-10 pr-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                          />
                        </div>
                      </div>
                      <div className="p-2">
                        {models.map((model) => (
                          <button
                            key={model}
                            onClick={() => {
                              setSelectedModel(model);
                              setShowModelDropdown(false);
                            }}
                            className="w-full px-3 py-2 text-left text-sm hover:bg-gray-50 rounded flex items-center gap-2"
                          >
                            <div className="w-5 h-5 rounded-full bg-teal-50 flex items-center justify-center">
                              <div className="w-2 h-2 rounded-full bg-teal-500"></div>
                            </div>
                            {model}
                          </button>
                        ))}
                      </div>
                    </div>
                  )}
                </div>
              </div>
              
              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Region
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <select
                  value={region}
                  onChange={(e) => setRegion(e.target.value)}
                  className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                >
                  <option>GLOBAL</option>
                  <option>US Iowa 1</option>
                  <option>EU West</option>
                </select>
              </div>
            </div>
          </div>

          {/* Performance */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center justify-between mb-4">
              <h2>Performance</h2>
              <a href="#" className="text-sm text-teal-600 hover:underline">Learn more</a>
            </div>

            <div className="flex items-center gap-2 mb-4">
              <button className="px-4 py-2 bg-white border border-gray-200 rounded-lg text-sm">
                Deployment Shapes
              </button>
              <button className="px-4 py-2 bg-white border border-gray-200 rounded-lg text-sm text-gray-400">
                Manual Config
              </button>
            </div>

            <div className="grid grid-cols-1 sm:grid-cols-2 md:grid-cols-3 gap-4">
              <button
                onClick={() => setSelectedShape('throughput')}
                className={`p-4 border-2 rounded-xl text-left transition-colors ${
                  selectedShape === 'throughput' ? 'border-teal-500 bg-teal-50/50' : 'border-gray-200 hover:border-gray-300'
                }`}
              >
                <div className="flex items-center justify-between mb-2">
                  <div>Throughput</div>
                  <Info className="w-4 h-4 text-gray-400" />
                </div>
                <div className="flex items-center gap-2 mb-2">
                  <span className="text-xs text-gray-500">NVIDIA H100 80GB</span>
                </div>
                <div className="text-xl mb-1">$48.00</div>
                <div className="text-xs text-gray-500">1 GPUs</div>
                <div className="text-xs text-gray-500">Precision: FP8 MM</div>
              </button>

              <button
                onClick={() => setSelectedShape('minimal')}
                className={`p-4 border-2 rounded-xl text-left transition-colors ${
                  selectedShape === 'minimal' ? 'border-teal-500 bg-teal-50/50' : 'border-gray-200 hover:border-gray-300'
                }`}
              >
                <div className="flex items-center justify-between mb-2">
                  <div>Minimal</div>
                  <Info className="w-4 h-4 text-gray-400" />
                </div>
                <div className="flex items-center gap-2 mb-2">
                  <span className="text-xs text-gray-500">NVIDIA H100 80GB</span>
                </div>
                <div className="text-xl mb-1">$4.00</div>
                <div className="text-xs text-gray-500">1 GPUs</div>
                <div className="text-xs text-gray-500">Precision: FP8 MM V2</div>
              </button>

              <button
                onClick={() => setSelectedShape('fast')}
                className={`p-4 border-2 rounded-xl text-left transition-colors ${
                  selectedShape === 'fast' ? 'border-teal-500 bg-teal-50/50' : 'border-gray-200 hover:border-gray-300'
                }`}
              >
                <div className="flex items-center justify-between mb-2">
                  <div>Fast</div>
                  <Info className="w-4 h-4 text-gray-400" />
                </div>
                <div className="flex items-center gap-2 mb-2">
                  <span className="text-xs text-gray-500">NVIDIA H100 80GB</span>
                </div>
                <div className="text-xl mb-1">$4.00</div>
                <div className="text-xs text-gray-500">1 GPUs</div>
                <div className="text-xs text-gray-500">Precision: FP8 MM V2</div>
              </button>
            </div>

            <div className="mt-6 grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Accelerator Type
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <select
                  value={acceleratorType}
                  onChange={(e) => setAcceleratorType(e.target.value)}
                  className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                >
                  <option>NVIDIA A100 80GB - $2.9</option>
                  <option>NVIDIA H100 80GB - $4.0</option>
                </select>
              </div>

              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Accelerator Count
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <select
                  value={acceleratorCount}
                  onChange={(e) => setAcceleratorCount(e.target.value)}
                  className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                >
                  <option>Auto</option>
                  <option>1</option>
                  <option>2</option>
                  <option>4</option>
                  <option>8</option>
                </select>
              </div>
            </div>

            <div className="mt-4 grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Quantization Precision
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <select
                  value={quantization}
                  onChange={(e) => setQuantization(e.target.value)}
                  className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                >
                  <option>BF16</option>
                  <option>FP8</option>
                  <option>INT4</option>
                </select>
              </div>
            </div>

            <div className="mt-4 flex flex-wrap items-center gap-x-6 gap-y-3">
              <label className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={optimizeLongPrompts}
                  onChange={(e) => setOptimizeLongPrompts(e.target.checked)}
                  className="w-4 h-4 text-teal-600 rounded focus:ring-teal-500 border-gray-300"
                />
                <span className="text-sm">Optimize for Long Prompts</span>
              </label>

              <label className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={enableMultiLoRA}
                  onChange={(e) => setEnableMultiLoRA(e.target.checked)}
                  className="w-4 h-4 text-teal-600 rounded focus:ring-teal-500 border-gray-300"
                />
                <span className="text-sm">Enable Multi-LoRA</span>
                <Info className="w-4 h-4 text-gray-400" />
              </label>
            </div>
          </div>

          {/* Deployment Scaling */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center justify-between mb-4">
              <h2>Deployment Scaling</h2>
              <div className="flex items-center gap-2 text-sm text-gray-500">
                <span>Autoscaling</span>
                <span>•</span>
                <span>Replicas: 0-1</span>
                <span>•</span>
                <span>Scale to zero: 60 minutes</span>
                <button>
                  <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 9l-7 7-7-7" />
                  </svg>
                </button>
              </div>
            </div>

            <label className="flex items-center gap-2 mb-4">
              <input
                type="checkbox"
                checked={enableAutoScaling}
                onChange={(e) => setEnableAutoScaling(e.target.checked)}
                className="w-4 h-4 text-teal-600 rounded focus:ring-teal-500 border-gray-300"
              />
              <span className="text-sm">Enable Auto Scaling</span>
            </label>

            <div className="grid grid-cols-2 gap-4 mb-4">
              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Min Replicas
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <input
                  type="text"
                  value={minReplicas}
                  onChange={(e) => setMinReplicas(e.target.value)}
                  className="w-full px-4 py-2 border-2 border-teal-500 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30"
                />
              </div>

              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Max Replicas
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <input
                  type="text"
                  value={maxReplicas}
                  onChange={(e) => setMaxReplicas(e.target.value)}
                  className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                />
              </div>
            </div>

            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Scale to Zero Window
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <div className="flex items-center gap-2">
                  <input
                    type="text"
                    value={scaleToZero}
                    onChange={(e) => setScaleToZero(e.target.value)}
                    className="min-w-0 flex-1 px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                  <select className="shrink-0 px-3 py-2 border border-gray-200 rounded-lg text-sm">
                    <option>Sec</option>
                    <option>Min</option>
                  </select>
                </div>
              </div>

              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Scale Up Window
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <div className="flex items-center gap-2">
                  <input
                    type="text"
                    value={scaleUp}
                    onChange={(e) => setScaleUp(e.target.value)}
                    className="min-w-0 flex-1 px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                  <select className="shrink-0 px-3 py-2 border border-gray-200 rounded-lg text-sm">
                    <option>Sec</option>
                    <option>Min</option>
                  </select>
                </div>
              </div>

              <div>
                <label className="flex items-center gap-2 text-sm mb-2">
                  Scale Down Window
                  <Info className="w-4 h-4 text-gray-400" />
                </label>
                <div className="flex items-center gap-2">
                  <input
                    type="text"
                    value={scaleDown}
                    onChange={(e) => setScaleDown(e.target.value)}
                    className="min-w-0 flex-1 px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                  <select className="shrink-0 px-3 py-2 border border-gray-200 rounded-lg text-sm">
                    <option>Sec</option>
                    <option>Min</option>
                  </select>
                </div>
              </div>
            </div>
          </div>

          <div className="flex flex-wrap items-center justify-between gap-4">
            <div className="text-sm text-gray-500">
              Projected hourly cost: <span className="text-xl text-gray-900">$0 - $11.6</span>
            </div>
            <button className="px-6 py-2.5 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700 transition-colors flex items-center gap-2">
              Deploy
              <Rocket className="w-4 h-4" />
            </button>
          </div>
        </div>

        {/* Right sidebar - info */}
        <div className="space-y-6">
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <h3 className="mb-3">About Deployments</h3>
            <p className="text-sm text-gray-500 mb-4">
              Deployments allow you to serve models via API endpoints with customizable performance and scaling options.
            </p>
            <ul className="space-y-2 text-sm text-gray-500">
              <li className="flex gap-2">
                <span className="text-teal-500">���</span>
                <span>Choose between throughput, minimal, or fast configurations</span>
              </li>
              <li className="flex gap-2">
                <span className="text-teal-500">•</span>
                <span>Configure auto-scaling to match your traffic patterns</span>
              </li>
              <li className="flex gap-2">
                <span className="text-teal-500">•</span>
                <span>Enable multi-LoRA for serving multiple adapters</span>
              </li>
            </ul>
          </div>

          <div className="bg-teal-50 rounded-xl p-4 border border-teal-100">
            <div className="text-sm mb-2 text-teal-800">Tip</div>
            <p className="text-sm text-gray-600">
              Start with minimal configuration and scale up based on your actual usage patterns.
            </p>
          </div>
        </div>
      </div>
    </div>
  );
}
