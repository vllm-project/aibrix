import { useState } from 'react';
import { ChevronLeft, Search, Upload, Check } from 'lucide-react';

interface CreateJobProps {
  onBack: () => void;
}

export function CreateJob({ onBack }: CreateJobProps) {
  const [currentStep, setCurrentStep] = useState<'model' | 'dataset' | 'settings'>('model');
  const [selectedModel, setSelectedModel] = useState('');
  const [showModelDropdown, setShowModelDropdown] = useState(false);
  const [displayName, setDisplayName] = useState('gsm8k-math-sample');
  const [maxTokens, setMaxTokens] = useState('');
  const [temperature, setTemperature] = useState('');
  const [topP, setTopP] = useState('');
  const [n, setN] = useState('');

  const models = [
    'Llama 3.1 8B Instruct',
    'Llama 3.1 70B Instruct 1$',
    'DeepSeek R1 Distil Qwen 7B',
    'DeepSeek R1 Distil Qwen 14B',
    'DeepSeek R1 Distil Llama 8B',
    'DeepSeek R1 Distil Llama 70B',
    'Qwen2.5-Coder 3B Instruct',
    'Qwen2.5-Coder 14B Instruct',
    'Qwen2.5-Coder 32B Instruct',
    'Qwen2.5 7B Instruct',
  ];

  return (
    <div className="p-8">
      <div className="mb-6">
        <button onClick={onBack} className="flex items-center gap-2 text-sm text-gray-500 hover:text-gray-900 mb-4">
          <ChevronLeft className="w-4 h-4" />
          Batch Inference / <span className="text-gray-400">Create</span>
        </button>
        
        <h1 className="text-2xl mb-2">Create a Batch Inference Job</h1>
        <p className="text-sm text-gray-500">Create a new batch inference job.</p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        <div className="lg:col-span-2 space-y-6">
          {/* Model Selection */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center gap-3 mb-4">
              <div className="w-6 h-6 rounded-full bg-teal-600 text-white flex items-center justify-center text-sm">
                {currentStep !== 'model' ? <Check className="w-4 h-4" /> : '1'}
              </div>
              <h2>Model Selection</h2>
            </div>
            
            {currentStep === 'model' && (
              <div>
                <div className="mb-4">
                  <label className="block text-sm mb-2">Choose a model to run batch inference.</label>
                  <div className="relative">
                    <button
                      onClick={() => setShowModelDropdown(!showModelDropdown)}
                      className="w-full px-4 py-2 border border-gray-200 rounded-lg text-left flex items-center justify-between hover:bg-gray-50"
                    >
                      <span className="text-sm">{selectedModel || 'Select Model'}</span>
                      <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 9l-7 7-7-7" />
                      </svg>
                    </button>
                    
                    {showModelDropdown && (
                      <div className="absolute z-10 w-full mt-2 bg-white border border-gray-200 rounded-xl shadow-lg max-h-64 overflow-y-auto">
                        <div className="p-2 border-b border-gray-100">
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
                                setCurrentStep('dataset');
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
                
                {selectedModel && (
                  <div className="text-sm text-gray-600 bg-gray-50 p-3 rounded-lg">
                    Model: accounts/aibrix/models/{selectedModel.toLowerCase().replace(/\s+/g, '-')}
                  </div>
                )}
              </div>
            )}
            
            {currentStep !== 'model' && selectedModel && (
              <div className="text-sm text-gray-600 bg-gray-50 p-3 rounded-lg">
                Model: accounts/aibrix/models/qwen2p5-coder-3b-instruct
              </div>
            )}
          </div>

          {/* Dataset */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center gap-3 mb-4">
              <div className={`w-6 h-6 rounded-full flex items-center justify-center text-sm ${
                currentStep === 'settings' ? 'bg-teal-600 text-white' : 
                currentStep === 'dataset' ? 'bg-teal-600 text-white' : 
                'bg-gray-200 text-gray-500'
              }`}>
                {currentStep === 'settings' ? <Check className="w-4 h-4" /> : '2'}
              </div>
              <h2>Dataset</h2>
            </div>
            
            {(currentStep === 'dataset' || currentStep === 'settings') && (
              <div>
                <div className="mb-4">
                  <label className="block text-sm mb-2">Upload a new dataset</label>
                  <div className="border-2 border-dashed border-gray-200 rounded-xl p-8 text-center hover:border-teal-400 transition-colors cursor-pointer">
                    <Upload className="w-8 h-8 text-gray-300 mx-auto mb-2" />
                    <p className="text-sm text-gray-500 mb-1">Click to upload a model or drag and drop</p>
                    <p className="text-xs text-gray-400">JSON</p>
                  </div>
                </div>
                
                <div className="flex items-center gap-3 mb-4">
                  <div className="flex-1 h-px bg-gray-200"></div>
                  <span className="text-sm text-gray-400">or</span>
                  <div className="flex-1 h-px bg-gray-200"></div>
                </div>
                
                <button className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50">
                  Select an existing dataset
                </button>
                
                {currentStep === 'dataset' && (
                  <button
                    onClick={() => setCurrentStep('settings')}
                    className="w-full mt-4 px-4 py-2 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700"
                  >
                    Continue
                  </button>
                )}
              </div>
            )}
          </div>

          {/* Optional Settings */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center gap-3 mb-4">
              <div className={`w-6 h-6 rounded-full flex items-center justify-center text-sm ${
                currentStep === 'settings' ? 'bg-teal-600 text-white' : 'bg-gray-200 text-gray-500'
              }`}>
                3
              </div>
              <h2>Optional Settings</h2>
            </div>
            
            {currentStep === 'settings' && (
              <div className="space-y-4">
                <div>
                  <label className="block text-sm mb-2">Display Name</label>
                  <input
                    type="text"
                    value={displayName}
                    onChange={(e) => setDisplayName(e.target.value)}
                    placeholder="gsm8k-math-sample"
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                </div>
                
                <div>
                  <label className="block text-sm mb-2">Output Dataset ID (Optional)</label>
                  <input
                    type="text"
                    placeholder="Enter a dataset ID"
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                </div>
                
                <div>
                  <label className="block text-sm mb-2">Max Tokens</label>
                  <input
                    type="text"
                    value={maxTokens}
                    onChange={(e) => setMaxTokens(e.target.value)}
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                </div>
                
                <div>
                  <label className="block text-sm mb-2">Temperature</label>
                  <input
                    type="text"
                    value={temperature}
                    onChange={(e) => setTemperature(e.target.value)}
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                </div>
                
                <div>
                  <label className="block text-sm mb-2">Top P</label>
                  <input
                    type="text"
                    value={topP}
                    onChange={(e) => setTopP(e.target.value)}
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                </div>
                
                <div>
                  <label className="block text-sm mb-2">N</label>
                  <input
                    type="text"
                    value={n}
                    onChange={(e) => setN(e.target.value)}
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                </div>
                
                <div>
                  <label className="block text-sm mb-2">Quantization Precision</label>
                  <div className="text-xs text-gray-400 mb-2">The precision with which the model is served.</div>
                  <select className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500">
                    <option>Unspecified</option>
                  </select>
                </div>
                
                <div className="flex gap-3 pt-4">
                  <button
                    onClick={() => setCurrentStep('dataset')}
                    className="px-6 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50"
                  >
                    Back
                  </button>
                  <button className="flex-1 px-6 py-2 bg-teal-600 text-white rounded-lg text-sm hover:bg-teal-700">
                    Create Job
                  </button>
                </div>
              </div>
            )}
          </div>
        </div>

        {/* Right Sidebar with Info */}
        <div className="space-y-6">
          {currentStep === 'model' && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <h3 className="mb-3">Choose a model to run batch inference</h3>
              <ul className="space-y-2 text-sm text-gray-500">
                <li className="flex gap-2">
                  <span className="text-teal-500">•</span>
                  <span>You can either choose a base model or a LoRA adapter as your starting point.</span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">•</span>
                  <span>The selected model will process your dataset and generate predictions based on its trained capabilities</span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">•</span>
                  <span>Consider factors such as accuracy, latency, and cost when selecting a model</span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">•</span>
                  <span>Some models may support additional configuration options, such as custom parameters or specific input formats</span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">•</span>
                  <span>Ensure that your dataset is compatible with the model's expected input requirements before proceeding</span>
                </li>
              </ul>
            </div>
          )}
          
          {currentStep === 'dataset' && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <h3 className="mb-3">Dataset Selection</h3>
              <p className="text-sm text-gray-500 mb-3">You can either select an existing dataset or upload a new one in JSONL format.</p>
              
              <div className="space-y-3 text-sm">
                <div>
                  <div className="mb-1">Input Dataset</div>
                  <div className="text-gray-500">
                    <div className="mb-1">Using Existing Datasets</div>
                    <ul className="space-y-1 ml-4">
                      <li className="flex gap-2"><span className="text-teal-500">•</span> Choose from your previously uploaded datasets</li>
                      <li className="flex gap-2"><span className="text-teal-500">•</span> Ensure the dataset matches your batch inference objectives</li>
                    </ul>
                  </div>
                </div>
                
                <div>
                  <div className="mb-1">Uploading New Datasets</div>
                  <ul className="space-y-1 text-gray-500 ml-4">
                    <li className="flex gap-2"><span className="text-teal-500">•</span> File must be in JSONL format</li>
                    <li className="flex gap-2"><span className="text-teal-500">•</span> Each line should be a valid JSON object</li>
                    <li className="flex gap-2"><span className="text-teal-500">•</span> Recommended size: 100-100,000 examples</li>
                    <li className="flex gap-2"><span className="text-teal-500">•</span> Data should be clean and high-quality</li>
                  </ul>
                </div>
                
                <div className="bg-teal-50 p-3 rounded-lg text-xs text-gray-600">
                  <strong>Tip:</strong> The quality of your batch inference data significantly impacts the performance of your batch inference.
                </div>
              </div>
            </div>
          )}
          
          {currentStep === 'settings' && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <h3 className="mb-3">Optional Settings</h3>
              <p className="text-sm text-gray-500 mb-4">We recommend initially batch inferencing with default hyperparameter values and adjusting hyperparameters only if results were not as expected.</p>
              
              <div className="space-y-3 text-sm">
                <div>
                  <div className="mb-1">Display name:</div>
                  <p className="text-gray-500">Set a custom name for your batch inferenced model.</p>
                </div>
                
                <div>
                  <div className="mb-1">Output Dataset ID:</div>
                  <p className="text-gray-500">The output dataset will be created automatically when you start the batch inference job</p>
                </div>
                
                <h4 className="mt-4 mb-2">Inference Parameters</h4>
                
                <div>
                  <div className="mb-1">Max Tokens:</div>
                  <p className="text-gray-500">The maximum number of tokens to generate.</p>
                </div>
                
                <div>
                  <div className="mb-1">Temperature:</div>
                  <p className="text-gray-500">Controls randomness. Lower values are more focused, higher values more creative.</p>
                </div>
                
                <div>
                  <div className="mb-1">Top P:</div>
                  <p className="text-gray-500">Controls diversity. Higher values increase the likelihood of less common tokens.</p>
                </div>
                
                <div>
                  <div className="mb-1">N:</div>
                  <p className="text-gray-500">The number of completions to generate for each prompt.</p>
                </div>
                
                <div className="bg-teal-50 p-3 rounded-lg text-xs text-gray-600 mt-4">
                  <strong>Tip:</strong> Start with default values and adjust only if needed. Monitor model performance to guide parameter tuning.
                </div>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}