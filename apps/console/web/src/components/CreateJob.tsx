import { useState, useEffect, useRef } from 'react';
import { ChevronLeft, Search, Upload, Check, X, AlertCircle, CheckCircle2, Loader2, Cpu, Layers as LayersIcon } from 'lucide-react';
import {
  createJob,
  uploadFile,
  listModels as apiListModels,
  listModelDeploymentTemplates,
  JobEndpoint,
} from '../utils/api';
import type { ModelDeploymentTemplate } from '../utils/api';
import { validateBatchFile, generateJobDisplayName, ValidationResult } from '../utils/batchValidation';
import { Model } from '../data/mockData';

interface CreateJobProps {
  onBack: () => void;
}

function parseNumber(value: string): number | undefined {
  if (value.trim() === '') return undefined;
  const n = Number(value);
  return isNaN(n) ? undefined : n;
}

type Step = 'model' | 'template' | 'dataset' | 'settings';
const STEP_INDEX: Record<Step, number> = { model: 1, template: 2, dataset: 3, settings: 4 };

export function CreateJob({ onBack }: CreateJobProps) {
  const [currentStep, setCurrentStep] = useState<Step>('model');
  const [selectedModel, setSelectedModel] = useState('');
  const [selectedModelId, setSelectedModelId] = useState('');
  const [showModelDropdown, setShowModelDropdown] = useState(false);
  const [displayName, setDisplayName] = useState('');
  const [maxTokens, setMaxTokens] = useState('');
  const [temperature, setTemperature] = useState('');
  const [topP, setTopP] = useState('');
  const [n, setN] = useState('');

  const [models, setModels] = useState<Model[]>([]);
  const [modelsLoading, setModelsLoading] = useState(true);
  const [modelSearchQuery, setModelSearchQuery] = useState('');

  // Template selection
  const [templates, setTemplates] = useState<ModelDeploymentTemplate[]>([]);
  const [templatesLoading, setTemplatesLoading] = useState(false);
  const [templatesError, setTemplatesError] = useState<string | null>(null);
  const [selectedTemplate, setSelectedTemplate] = useState<ModelDeploymentTemplate | null>(null);

  const [selectedFile, setSelectedFile] = useState<File | null>(null);
  const [uploadedFileId, setUploadedFileId] = useState<string | null>(null);
  const [uploading, setUploading] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);

  // Validation state
  const [validating, setValidating] = useState(false);
  const [validation, setValidation] = useState<ValidationResult | null>(null);

  // Hyperparameter validation errors
  const [paramErrors, setParamErrors] = useState<Record<string, string>>({});

  const fileInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    apiListModels().then(m => {
      setModels(m);
      setModelsLoading(false);
    }).catch(err => {
      console.error('Failed to fetch models:', err);
      setModelsLoading(false);
    });
  }, []);

  const filteredModels = models.filter(m =>
    m.name.toLowerCase().includes(modelSearchQuery.toLowerCase())
  );

  const handleSelectModel = (model: Model) => {
    setSelectedModel(model.name);
    setSelectedModelId(model.id);
    setShowModelDropdown(false);
    setModelSearchQuery('');
    setDisplayName(generateJobDisplayName(model.name));
    // Reset downstream state when model changes
    setSelectedTemplate(null);
    setTemplates([]);
    setTemplatesError(null);
    setCurrentStep('template');

    // Load templates for this model
    setTemplatesLoading(true);
    listModelDeploymentTemplates(model.id, 'active')
      .then((tpls) => {
        setTemplates(tpls);
        if (tpls.length === 0) {
          setTemplatesError(
            `No active deployment templates registered for ${model.name}. ` +
              `Create one from the model detail page before submitting a batch job.`,
          );
        }
      })
      .catch((err) => {
        setTemplatesError(`Failed to load templates: ${err instanceof Error ? err.message : String(err)}`);
        setTemplates([]);
      })
      .finally(() => setTemplatesLoading(false));

    // Re-validate already-selected file against the new model
    if (selectedFile) {
      setValidating(true);
      validateBatchFile(selectedFile, model.name).then((r) => {
        setValidation(r);
        setValidating(false);
      });
    }
  };

  const handleSelectTemplate = (tpl: ModelDeploymentTemplate) => {
    setSelectedTemplate(tpl);
    setCurrentStep('dataset');
  };

  const handleFileSelect = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;

    setSelectedFile(file);
    setUploadedFileId(null);
    setValidation(null);
    setSubmitError(null);

    // Validate immediately
    setValidating(true);
    try {
      const result = await validateBatchFile(file, selectedModel);
      setValidation(result);
    } catch (err) {
      setValidation({
        valid: false,
        totalLines: 0,
        errors: [`Failed to read file: ${err instanceof Error ? err.message : 'unknown error'}`],
        warnings: [],
        detectedModel: null,
        endpoints: [],
      });
    } finally {
      setValidating(false);
    }
  };

  const handleRemoveFile = () => {
    setSelectedFile(null);
    setUploadedFileId(null);
    setValidation(null);
    setSubmitError(null);
    if (fileInputRef.current) {
      fileInputRef.current.value = '';
    }
  };

  const validateParam = (_field: string, value: string, min: number, max: number, isInt: boolean): string => {
    if (value.trim() === '') return '';
    const num = Number(value);
    if (isNaN(num)) return 'Must be a number';
    if (isInt && !Number.isInteger(num)) return 'Must be an integer';
    if (num < min) return `Min: ${min}`;
    if (num > max) return `Max: ${max}`;
    return '';
  };

  const handleParamChange = (
    field: string,
    value: string,
    setter: (v: string) => void,
    min: number,
    max: number,
    isInt: boolean,
  ) => {
    setter(value);
    const err = validateParam(field, value, min, max, isInt);
    setParamErrors(prev => ({ ...prev, [field]: err }));
  };

  const hasParamErrors = Object.values(paramErrors).some(e => e !== '');

  const canSubmit =
    selectedModel &&
    selectedTemplate &&
    !submitting &&
    !hasParamErrors &&
    displayName.trim() !== '' &&
    (!selectedFile || (validation?.valid ?? false));

  const handleCreateJob = async () => {
    if (!canSubmit) return;
    setSubmitting(true);
    setSubmitError(null);
    try {
      let datasetId = uploadedFileId;
      if (selectedFile && !datasetId) {
        setUploading(true);
        try {
          const fileInfo = await uploadFile(selectedFile, 'batch');
          datasetId = fileInfo.id;
          setUploadedFileId(fileInfo.id);
        } catch (uploadErr) {
          // Upload must succeed so the backend can resolve the dataset id.
          const msg = uploadErr instanceof Error ? uploadErr.message : 'Unknown error';
          setSubmitError(`Failed to upload dataset: ${msg}`);
          return;
        } finally {
          setUploading(false);
        }
      }

      // Pick the endpoint from JSONL validation; default to chat completions.
      const endpoint: JobEndpoint =
        (validation?.endpoints[0] as JobEndpoint | undefined) || '/v1/chat/completions';

      await createJob({
        inputDataset: datasetId || '',
        endpoint,
        completionWindow: '24h',
        name: displayName,
        modelTemplateName: selectedTemplate?.name,
        modelTemplateVersion: selectedTemplate?.version,
        ...(maxTokens.trim() !== '' && { maxTokens: parseNumber(maxTokens) }),
        ...(temperature.trim() !== '' && { temperature: parseNumber(temperature) }),
        ...(topP.trim() !== '' && { topP: parseNumber(topP) }),
        ...(n.trim() !== '' && { n: parseNumber(n) }),
      });
      onBack();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      setSubmitError(`Failed to create job: ${msg}`);
    } finally {
      setSubmitting(false);
    }
  };

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
          {/* Step 1: Model Selection */}
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
                              value={modelSearchQuery}
                              onChange={(e) => setModelSearchQuery(e.target.value)}
                              className="w-full pl-10 pr-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                            />
                          </div>
                        </div>
                        <div className="p-2">
                          {modelsLoading ? (
                            <div className="px-3 py-4 text-center text-sm text-gray-500">Loading models...</div>
                          ) : filteredModels.length === 0 ? (
                            <div className="px-3 py-4 text-center text-sm text-gray-500">No models found.</div>
                          ) : (
                            filteredModels.map((model) => (
                              <button
                                key={model.id}
                                onClick={() => handleSelectModel(model)}
                                className="w-full px-3 py-2 text-left text-sm hover:bg-gray-50 rounded flex items-center gap-2"
                              >
                                <div className="w-5 h-5 rounded-full bg-teal-50 flex items-center justify-center">
                                  <div className="w-2 h-2 rounded-full bg-teal-500"></div>
                                </div>
                                {model.name}
                              </button>
                            ))
                          )}
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
                Model: accounts/aibrix/models/{selectedModel.toLowerCase().replace(/\s+/g, '-')}
              </div>
            )}
          </div>

          {/* Step 2: Deployment Template */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center gap-3 mb-4">
              <div
                className={`w-6 h-6 rounded-full flex items-center justify-center text-sm ${
                  STEP_INDEX[currentStep] > STEP_INDEX.template
                    ? 'bg-teal-600 text-white'
                    : currentStep === 'template'
                      ? 'bg-teal-600 text-white'
                      : 'bg-gray-200 text-gray-500'
                }`}
              >
                {STEP_INDEX[currentStep] > STEP_INDEX.template ? (
                  <Check className="w-4 h-4" />
                ) : (
                  '2'
                )}
              </div>
              <h2>Deployment Template</h2>
            </div>

            {currentStep === 'template' && (
              <div>
                <p className="text-sm text-gray-500 mb-4">
                  Pick the engine + accelerator + tuning preset this batch should run on.
                  Templates are curated per model from the Model Library.
                </p>
                {templatesLoading ? (
                  <div className="flex items-center gap-2 text-sm text-gray-500 py-6 justify-center">
                    <Loader2 className="w-4 h-4 animate-spin" />
                    Loading templates for {selectedModel}...
                  </div>
                ) : templatesError ? (
                  <div className="flex items-start gap-2 text-sm text-red-700 bg-red-50 border border-red-200 rounded-lg p-3">
                    <AlertCircle className="w-4 h-4 mt-0.5 shrink-0" />
                    <div>{templatesError}</div>
                  </div>
                ) : (
                  <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                    {templates.map((tpl) => (
                      <TemplateCard
                        key={tpl.id}
                        template={tpl}
                        selected={selectedTemplate?.id === tpl.id}
                        onSelect={() => handleSelectTemplate(tpl)}
                      />
                    ))}
                  </div>
                )}
              </div>
            )}

            {STEP_INDEX[currentStep] > STEP_INDEX.template && selectedTemplate && (
              <SelectedTemplateSummary
                template={selectedTemplate}
                onChange={() => {
                  setSelectedTemplate(null);
                  setCurrentStep('template');
                }}
              />
            )}
          </div>

          {/* Step 3: Dataset */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center gap-3 mb-4">
              <div
                className={`w-6 h-6 rounded-full flex items-center justify-center text-sm ${
                  STEP_INDEX[currentStep] > STEP_INDEX.dataset
                    ? 'bg-teal-600 text-white'
                    : currentStep === 'dataset'
                      ? 'bg-teal-600 text-white'
                      : 'bg-gray-200 text-gray-500'
                }`}
              >
                {STEP_INDEX[currentStep] > STEP_INDEX.dataset ? (
                  <Check className="w-4 h-4" />
                ) : (
                  '3'
                )}
              </div>
              <h2>Dataset</h2>
            </div>

            {(currentStep === 'dataset' || currentStep === 'settings') && (
              <div>
                <div className="mb-4">
                  <label className="block text-sm mb-2">Upload a new dataset</label>
                  <input
                    type="file"
                    ref={fileInputRef}
                    onChange={handleFileSelect}
                    accept=".json,.jsonl"
                    className="hidden"
                  />
                  {selectedFile ? (
                    <div className={`border-2 rounded-xl p-4 flex items-center justify-between ${
                      validation?.valid === false
                        ? 'border-red-200 bg-red-50'
                        : 'border-teal-200 bg-teal-50'
                    }`}>
                      <div className="flex items-center gap-3">
                        <Upload className={`w-5 h-5 ${validation?.valid === false ? 'text-red-600' : 'text-teal-600'}`} />
                        <span className="text-sm text-gray-900">{selectedFile.name}</span>
                      </div>
                      <button
                        onClick={handleRemoveFile}
                        className="text-gray-400 hover:text-gray-600"
                      >
                        <X className="w-4 h-4" />
                      </button>
                    </div>
                  ) : (
                    <div
                      onClick={() => fileInputRef.current?.click()}
                      className="border-2 border-dashed border-gray-200 rounded-xl p-8 text-center hover:border-teal-400 transition-colors cursor-pointer"
                    >
                      <Upload className="w-8 h-8 text-gray-300 mx-auto mb-2" />
                      <p className="text-sm text-gray-500 mb-1">Click to upload or drag and drop</p>
                      <p className="text-xs text-gray-400">JSONL format</p>
                    </div>
                  )}

                  {/* Validation status */}
                  {validating && (
                    <div className="mt-3 flex items-center gap-2 text-sm text-gray-500">
                      <Loader2 className="w-4 h-4 animate-spin" />
                      Validating file...
                    </div>
                  )}

                  {validation && !validating && (
                    <div className="mt-3 space-y-2">
                      {validation.valid ? (
                        <div className="flex items-start gap-2 text-sm text-emerald-700 bg-emerald-50 border border-emerald-200 rounded-lg p-3">
                          <CheckCircle2 className="w-4 h-4 mt-0.5 shrink-0" />
                          <div>
                            <div className="font-medium">
                              {validation.totalLines} request{validation.totalLines !== 1 ? 's' : ''} — valid
                            </div>
                            <div className="text-emerald-600 text-xs mt-1">
                              {validation.detectedModel && <>Model: {validation.detectedModel}</>}
                              {validation.endpoints.length > 0 && (
                                <> &middot; Endpoint{validation.endpoints.length > 1 ? 's' : ''}: {validation.endpoints.join(', ')}</>
                              )}
                            </div>
                          </div>
                        </div>
                      ) : (
                        <div className="flex items-start gap-2 text-sm text-red-700 bg-red-50 border border-red-200 rounded-lg p-3">
                          <AlertCircle className="w-4 h-4 mt-0.5 shrink-0" />
                          <div>
                            <div className="font-medium mb-1">Validation failed</div>
                            <ul className="space-y-0.5 text-xs text-red-600">
                              {validation.errors.map((err, i) => (
                                <li key={i}>{err}</li>
                              ))}
                            </ul>
                          </div>
                        </div>
                      )}

                      {validation.warnings.length > 0 && (
                        <div className="flex items-start gap-2 text-sm text-amber-700 bg-amber-50 border border-amber-200 rounded-lg p-3">
                          <AlertCircle className="w-4 h-4 mt-0.5 shrink-0" />
                          <ul className="space-y-0.5 text-xs">
                            {validation.warnings.map((w, i) => (
                              <li key={i}>{w}</li>
                            ))}
                          </ul>
                        </div>
                      )}
                    </div>
                  )}
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
                  <div className="flex gap-3 mt-4">
                    <button
                      onClick={() => setCurrentStep('template')}
                      className="px-6 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50"
                    >
                      Back
                    </button>
                    <button
                      onClick={() => setCurrentStep('settings')}
                      disabled={selectedFile != null && !validation?.valid}
                      className={`flex-1 px-4 py-2 rounded-lg text-sm text-white ${
                        selectedFile != null && !validation?.valid
                          ? 'bg-teal-400 cursor-not-allowed'
                          : 'bg-teal-600 hover:bg-teal-700'
                      }`}
                    >
                      Continue
                    </button>
                  </div>
                )}
              </div>
            )}
          </div>

          {/* Step 4: Settings */}
          <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
            <div className="flex items-center gap-3 mb-4">
              <div className={`w-6 h-6 rounded-full flex items-center justify-center text-sm ${
                currentStep === 'settings' ? 'bg-teal-600 text-white' : 'bg-gray-200 text-gray-500'
              }`}>
                4
              </div>
              <h2>Settings</h2>
            </div>

            {currentStep === 'settings' && (
              <div className="space-y-4">
                {/* Display Name - required */}
                <div>
                  <label className="block text-sm mb-1">
                    Display Name <span className="text-red-500">*</span>
                  </label>
                  <p className="text-xs text-gray-400 mb-2">
                    Auto-generated from model name. You can override it.
                  </p>
                  <input
                    type="text"
                    value={displayName}
                    onChange={(e) => setDisplayName(e.target.value)}
                    placeholder="my-batch-job"
                    className={`w-full px-4 py-2 border rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 ${
                      displayName.trim() === '' ? 'border-red-300' : 'border-gray-200'
                    }`}
                  />
                  {displayName.trim() === '' && (
                    <p className="text-xs text-red-500 mt-1">Display name is required</p>
                  )}
                </div>

                <div>
                  <label className="block text-sm mb-2">Output Dataset ID (Optional)</label>
                  <input
                    type="text"
                    placeholder="Enter a dataset ID"
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                  />
                </div>

                {/* Hyperparameters section */}
                <div className="border-t border-gray-100 pt-4">
                  <h3 className="text-sm font-medium mb-1">Inference Parameters</h3>
                  <p className="text-xs text-gray-400 mb-4">
                    If not set, per-request values from the JSONL file are used, or model defaults apply.
                  </p>

                  <div className="grid grid-cols-2 gap-4">
                    <div>
                      <label className="block text-sm mb-1">Max Tokens</label>
                      <p className="text-xs text-gray-400 mb-1">1 – model max</p>
                      <input
                        type="text"
                        value={maxTokens}
                        onChange={(e) => handleParamChange('maxTokens', e.target.value, setMaxTokens, 1, 1048576, true)}
                        placeholder="e.g. 4096"
                        className={`w-full px-4 py-2 border rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 ${
                          paramErrors.maxTokens ? 'border-red-300' : 'border-gray-200'
                        }`}
                      />
                      {paramErrors.maxTokens && (
                        <p className="text-xs text-red-500 mt-1">{paramErrors.maxTokens}</p>
                      )}
                    </div>

                    <div>
                      <label className="block text-sm mb-1">Temperature</label>
                      <p className="text-xs text-gray-400 mb-1">0.0 – 2.0</p>
                      <input
                        type="text"
                        value={temperature}
                        onChange={(e) => handleParamChange('temperature', e.target.value, setTemperature, 0, 2, false)}
                        placeholder="e.g. 1.0"
                        className={`w-full px-4 py-2 border rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 ${
                          paramErrors.temperature ? 'border-red-300' : 'border-gray-200'
                        }`}
                      />
                      {paramErrors.temperature && (
                        <p className="text-xs text-red-500 mt-1">{paramErrors.temperature}</p>
                      )}
                    </div>

                    <div>
                      <label className="block text-sm mb-1">Top P</label>
                      <p className="text-xs text-gray-400 mb-1">0.0 – 1.0</p>
                      <input
                        type="text"
                        value={topP}
                        onChange={(e) => handleParamChange('topP', e.target.value, setTopP, 0, 1, false)}
                        placeholder="e.g. 1.0"
                        className={`w-full px-4 py-2 border rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 ${
                          paramErrors.topP ? 'border-red-300' : 'border-gray-200'
                        }`}
                      />
                      {paramErrors.topP && (
                        <p className="text-xs text-red-500 mt-1">{paramErrors.topP}</p>
                      )}
                    </div>

                    <div>
                      <label className="block text-sm mb-1">N</label>
                      <p className="text-xs text-gray-400 mb-1">1 – 128</p>
                      <input
                        type="text"
                        value={n}
                        onChange={(e) => handleParamChange('n', e.target.value, setN, 1, 128, true)}
                        placeholder="e.g. 1"
                        className={`w-full px-4 py-2 border rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 ${
                          paramErrors.n ? 'border-red-300' : 'border-gray-200'
                        }`}
                      />
                      {paramErrors.n && (
                        <p className="text-xs text-red-500 mt-1">{paramErrors.n}</p>
                      )}
                    </div>
                  </div>
                </div>

                {/* Error display */}
                {submitError && (
                  <div className="flex items-start gap-2 text-sm text-red-700 bg-red-50 border border-red-200 rounded-lg p-3">
                    <AlertCircle className="w-4 h-4 mt-0.5 shrink-0" />
                    <div>{submitError}</div>
                  </div>
                )}

                <div className="flex gap-3 pt-4">
                  <button
                    onClick={() => setCurrentStep('dataset')}
                    className="px-6 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50"
                  >
                    Back
                  </button>
                  <button
                    onClick={handleCreateJob}
                    disabled={!canSubmit}
                    className={`flex-1 px-6 py-2 rounded-lg text-sm text-white ${
                      !canSubmit
                        ? 'bg-teal-400 cursor-not-allowed'
                        : 'bg-teal-600 hover:bg-teal-700'
                    }`}
                  >
                    {uploading ? 'Uploading file...' : submitting ? 'Creating...' : 'Create Job'}
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
                  <span className="text-teal-500">&#8226;</span>
                  <span>You can either choose a base model or a LoRA adapter as your starting point.</span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">&#8226;</span>
                  <span>The selected model will process your dataset and generate predictions based on its trained capabilities</span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">&#8226;</span>
                  <span>Consider factors such as accuracy, latency, and cost when selecting a model</span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">&#8226;</span>
                  <span>Some models may support additional configuration options, such as custom parameters or specific input formats</span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">&#8226;</span>
                  <span>Ensure that your dataset is compatible with the model's expected input requirements before proceeding</span>
                </li>
              </ul>
            </div>
          )}

          {currentStep === 'template' && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <h3 className="mb-3">Why a deployment template?</h3>
              <ul className="space-y-2 text-sm text-gray-500">
                <li className="flex gap-2">
                  <span className="text-teal-500">&#8226;</span>
                  <span>
                    A template binds the model to a specific engine version, accelerator
                    SKU, parallelism, and tuning preset — so two batches against the same
                    model run on identical, reproducible hardware.
                  </span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">&#8226;</span>
                  <span>
                    Each card is one curated combination. Pick the one that matches your
                    throughput / cost target; advanced overrides land in a follow-up step.
                  </span>
                </li>
                <li className="flex gap-2">
                  <span className="text-teal-500">&#8226;</span>
                  <span>
                    Templates are managed by admins on each model's detail page. If
                    nothing fits, ask for a new template instead of forking the
                    underlying spec — that's the audit / cost-attribution path.
                  </span>
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
                      <li className="flex gap-2"><span className="text-teal-500">&#8226;</span> Choose from your previously uploaded datasets</li>
                      <li className="flex gap-2"><span className="text-teal-500">&#8226;</span> Ensure the dataset matches your batch inference objectives</li>
                    </ul>
                  </div>
                </div>

                <div>
                  <div className="mb-1">Uploading New Datasets</div>
                  <ul className="space-y-1 text-gray-500 ml-4">
                    <li className="flex gap-2"><span className="text-teal-500">&#8226;</span> File must be in JSONL format</li>
                    <li className="flex gap-2"><span className="text-teal-500">&#8226;</span> Each line must be a valid JSON object with custom_id, method, url, and body</li>
                    <li className="flex gap-2"><span className="text-teal-500">&#8226;</span> All requests must use the same model, matching your selection in Step 1</li>
                    <li className="flex gap-2"><span className="text-teal-500">&#8226;</span> Recommended size: 100–100,000 requests</li>
                    <li className="flex gap-2"><span className="text-teal-500">&#8226;</span> Data should be clean and high-quality</li>
                  </ul>
                </div>

                <div className="bg-gray-50 p-3 rounded-lg text-xs text-gray-600">
                  <strong>Example JSONL line:</strong>
                  <pre className="mt-1 overflow-x-auto whitespace-pre-wrap break-all text-[11px] text-gray-500">
{`{"custom_id":"req-001","method":"POST","url":"/v1/responses","body":{"model":"${selectedModel || 'your-model'}","input":"Hello"}}`}
                  </pre>
                </div>

                <div className="bg-teal-50 p-3 rounded-lg text-xs text-gray-600">
                  <strong>Tip:</strong> The file is validated on upload. Model names in the file must match your selected model.
                </div>
              </div>
            </div>
          )}

          {currentStep === 'settings' && (
            <div className="bg-white rounded-xl shadow-sm border border-gray-100 p-6">
              <h3 className="mb-3">Settings</h3>
              <p className="text-sm text-gray-500 mb-4">We recommend initially running with default hyperparameter values and adjusting only if results were not as expected.</p>

              <div className="space-y-3 text-sm">
                <div>
                  <div className="mb-1">Display Name <span className="text-red-500">*</span></div>
                  <p className="text-gray-500">A human-readable name for your batch job. Auto-generated but can be customized.</p>
                </div>

                <div>
                  <div className="mb-1">Output Dataset ID:</div>
                  <p className="text-gray-500">The output dataset will be created automatically when you start the batch inference job</p>
                </div>

                <h4 className="mt-4 mb-2">Inference Parameters</h4>
                <p className="text-xs text-gray-400 mb-2">All parameters are optional. If left empty, per-request values from the JSONL file take precedence, otherwise model defaults apply.</p>

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
                  <p className="text-gray-500">Controls diversity via nucleus sampling.</p>
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

function TemplateCard({
  template,
  selected,
  onSelect,
}: {
  template: ModelDeploymentTemplate;
  selected: boolean;
  onSelect: () => void;
}) {
  const a = template.spec?.accelerator;
  const e = template.spec?.engine;
  const p = template.spec?.parallelism;
  const q = template.spec?.quantization;
  const endpoints = template.spec?.supportedEndpoints ?? [];

  return (
    <button
      type="button"
      onClick={onSelect}
      className={`text-left border rounded-xl p-4 transition-all ${
        selected
          ? 'border-teal-500 bg-teal-50/40 ring-2 ring-teal-500/20'
          : 'border-gray-200 hover:border-gray-300'
      }`}
    >
      <div className="flex items-start justify-between mb-2">
        <div>
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium text-gray-900">{template.name}</span>
            <span className="text-xs text-gray-400">{template.version}</span>
          </div>
        </div>
        {selected && (
          <CheckCircle2 className="w-4 h-4 text-teal-600 shrink-0" />
        )}
      </div>
      <div className="grid grid-cols-2 gap-2 text-xs text-gray-600 mb-2">
        <div className="flex items-center gap-1.5">
          <Cpu className="w-3 h-3 text-gray-400" />
          {e?.type ?? '—'} {e?.version ?? ''}
        </div>
        <div>
          {a?.type ?? '—'} × {a?.count ?? '?'}
          {a?.interconnect ? ` (${a.interconnect})` : ''}
        </div>
        <div className="flex items-center gap-1.5">
          <LayersIcon className="w-3 h-3 text-gray-400" />
          TP={p?.tp ?? 1} PP={p?.pp ?? 1} DP={p?.dp ?? 1}
        </div>
        <div>
          {q?.weight ? `weight=${q.weight}` : 'bf16'}
          {q?.kvCache ? ` / kv=${q.kvCache}` : ''}
        </div>
      </div>
      {endpoints.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {endpoints.map((ep) => (
            <span
              key={ep}
              className="px-1.5 py-0.5 bg-gray-50 text-[10px] text-gray-600 rounded font-mono"
            >
              {ep}
            </span>
          ))}
        </div>
      )}
    </button>
  );
}

function SelectedTemplateSummary({
  template,
  onChange,
}: {
  template: ModelDeploymentTemplate;
  onChange: () => void;
}) {
  const a = template.spec?.accelerator;
  const e = template.spec?.engine;
  return (
    <div className="text-sm text-gray-700 bg-gray-50 p-3 rounded-lg flex items-center justify-between">
      <div className="flex items-center gap-2 min-w-0">
        <CheckCircle2 className="w-4 h-4 text-teal-600 shrink-0" />
        <span className="font-medium">{template.name}</span>
        <span className="text-xs text-gray-400">{template.version}</span>
        <span className="text-xs text-gray-500 truncate">
          · {e?.type ?? '—'} on {a?.type ?? '?'} × {a?.count ?? '?'}
        </span>
      </div>
      <button
        type="button"
        onClick={onChange}
        className="text-xs text-teal-600 hover:text-teal-700 shrink-0 ml-3"
      >
        Change
      </button>
    </div>
  );
}
