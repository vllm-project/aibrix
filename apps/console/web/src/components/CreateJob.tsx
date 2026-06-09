import { useState, useEffect, useRef } from 'react';
import {
  AlertCircle,
  Check,
  CheckCircle2,
  ChevronLeft,
  Cpu,
  FileText,
  Layers as LayersIcon,
  Loader2,
  RefreshCw,
  Search,
  Upload,
  X,
} from 'lucide-react';
import {
  createJob,
  listFiles,
  uploadFile,
  listModels as apiListModels,
  listModelDeploymentTemplates,
  JobEndpoint,
} from '../utils/api';
import type { FileInfo, ModelDeploymentTemplate } from '../utils/api';
import {
  validateBatchFile,
  validateBatchLines,
  generateJobDisplayName,
  ValidationResult,
  MutationDiff,
  ParseResult,
  parseJsonl,
  validateBatchFileName,
  applyBatchOverrides,
  serializeJsonl,
  hasAnyOverride,
  BatchOverrides,
} from '../utils/batchValidation';
import {
  formatBytes,
  formatFileCreatedAt,
  formatModelSelectionLabel,
  getBatchExampleJsonlLine,
  getCreateJobEndpoint,
  getCreateJobReadiness,
} from '../utils/batchProduct';
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
  const [selectedModel, setSelectedModel] = useState('');           // display name (model.name)
  const [selectedModelId, setSelectedModelId] = useState('');
  const [selectedServingName, setSelectedServingName] = useState(''); // serving identifier (model.serving_name)
  const [showModelDropdown, setShowModelDropdown] = useState(false);
  const [displayName, setDisplayName] = useState('');
  const [maxTokens, setMaxTokens] = useState('');
  const [temperature, setTemperature] = useState('');
  const [topP, setTopP] = useState('');
  const [n, setN] = useState('');
  const [selectedEndpoint, setSelectedEndpoint] = useState('');

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
  const [selectedExistingFile, setSelectedExistingFile] = useState<FileInfo | null>(null);
  const [existingFiles, setExistingFiles] = useState<FileInfo[]>([]);
  const [filesLoading, setFilesLoading] = useState(false);
  const [filesError, setFilesError] = useState<string | null>(null);
  const [showExistingFiles, setShowExistingFiles] = useState(false);
  const [fileSearchQuery, setFileSearchQuery] = useState('');
  const [uploading, setUploading] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [datasetDragActive, setDatasetDragActive] = useState(false);

  // Validation state
  const [validating, setValidating] = useState(false);
  const [validation, setValidation] = useState<ValidationResult | null>(null);
  const [selectedFileParse, setSelectedFileParse] = useState<ParseResult | null>(null);
  const [overridePreview, setOverridePreview] = useState<MutationDiff | null>(null);
  const [overridePreviewLoading, setOverridePreviewLoading] = useState(false);

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

  const batchFiles = existingFiles.filter((file) => {
    const purpose = (file.purpose || '').toLowerCase();
    const q = fileSearchQuery.trim().toLowerCase();
    const matchesPurpose = purpose === '' || purpose === 'batch';
    const matchesQuery =
      q === '' ||
      file.id.toLowerCase().includes(q) ||
      (file.name || '').toLowerCase().includes(q);
    return matchesPurpose && matchesQuery;
  });

  const loadExistingFiles = () => {
    setShowExistingFiles(true);
    setFilesLoading(true);
    setFilesError(null);
    listFiles()
      .then((files) => setExistingFiles(files || []))
      .catch((err) => {
        setFilesError(err instanceof Error ? err.message : String(err));
        setExistingFiles([]);
      })
      .finally(() => setFilesLoading(false));
  };

  const handleSelectModel = (model: Model) => {
    setSelectedModel(model.name);
    setSelectedModelId(model.id);
    setSelectedServingName(model.servingName ?? '');
    setShowModelDropdown(false);
    setModelSearchQuery('');
    setDisplayName(generateJobDisplayName(model.name));
    // Reset downstream state when model changes
    setSelectedTemplate(null);
    setSelectedEndpoint('');
    setTemplates([]);
    setTemplatesError(null);
    setUploadedFileId(null);
    setSelectedExistingFile(null);
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

    // Re-validate already-selected file against the new model.
    // Note: supportedEndpoints is unknown until a template is picked in step 2;
    // endpoint check will be re-applied when a template is selected.
    if (selectedFile) {
      setValidating(true);
      const validate = selectedFileParse
        ? Promise.resolve(validateBatchLines(selectedFileParse, { expectedModel: model.servingName ?? '' }))
        : validateBatchFile(selectedFile, { expectedModel: model.servingName ?? '' });
      validate.then((r) => {
        setValidation(r);
        setValidating(false);
      });
    }
  };

  const handleSelectTemplate = (tpl: ModelDeploymentTemplate) => {
    setSelectedTemplate(tpl);
    setSelectedEndpoint(tpl.spec?.supportedEndpoints?.[0] || '/v1/chat/completions');
    setCurrentStep('dataset');
    // Re-validate selected file with the template's supported endpoints.
    if (selectedFile) {
      setValidating(true);
      const ctx = {
        expectedModel: selectedServingName,
        supportedEndpoints: tpl.spec?.supportedEndpoints,
      };
      const validate = selectedFileParse
        ? Promise.resolve(validateBatchLines(selectedFileParse, ctx))
        : validateBatchFile(selectedFile, ctx);
      validate
        .then((r) => setValidation(r))
        .finally(() => setValidating(false));
    }
  };

  const handleDatasetFile = async (file: File) => {
    setSelectedFile(file);
    setUploadedFileId(null);
    setSelectedExistingFile(null);
    setValidation(null);
    setSelectedFileParse(null);
    setSubmitError(null);

    const fileNameError = validateBatchFileName(file.name);
    if (fileNameError) {
      setValidation({
        valid: false,
        totalLines: 0,
        errors: [fileNameError],
        warnings: [],
        detectedModel: null,
        endpoints: [],
      });
      return;
    }

    // Validate immediately
    setValidating(true);
    try {
      const parsed = parseJsonl(await file.text());
      setSelectedFileParse(parsed);
      const result = validateBatchLines(parsed, {
        expectedModel: selectedServingName,
        supportedEndpoints: selectedTemplate?.spec?.supportedEndpoints,
      });
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

  const handleFileSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    void handleDatasetFile(file);
  };

  const handleDatasetDragOver = (e: React.DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.stopPropagation();
    e.dataTransfer.dropEffect = 'copy';
    setDatasetDragActive(true);
  };

  const handleDatasetDragLeave = (e: React.DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.stopPropagation();
    const nextTarget = e.relatedTarget;
    if (!nextTarget || !e.currentTarget.contains(nextTarget as Node)) {
      setDatasetDragActive(false);
    }
  };

  const handleDatasetDrop = (e: React.DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.stopPropagation();
    setDatasetDragActive(false);

    const file = e.dataTransfer.files?.[0];
    if (!file) return;
    if (fileInputRef.current) {
      fileInputRef.current.value = '';
    }
    void handleDatasetFile(file);
  };

  const handleRemoveFile = () => {
    setSelectedFile(null);
    setUploadedFileId(null);
    setValidation(null);
    setSelectedFileParse(null);
    setSubmitError(null);
    if (fileInputRef.current) {
      fileInputRef.current.value = '';
    }
  };

  const handleSelectExistingFile = (file: FileInfo) => {
    setSelectedExistingFile(file);
    setSelectedFile(null);
    setSelectedFileParse(null);
    setUploadedFileId(file.id);
    setValidation(null);
    setSubmitError(null);
    setShowExistingFiles(false);
    if (fileInputRef.current) {
      fileInputRef.current.value = '';
    }
  };

  const handleClearExistingFile = () => {
    setSelectedExistingFile(null);
    setUploadedFileId(null);
    setSubmitError(null);
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

  useEffect(() => {
    if (selectedFile) {
      setUploadedFileId(null);
    }
  }, [selectedFile, maxTokens, temperature, topP, n]);

  useEffect(() => {
    if (!selectedFile || !selectedFileParse || !(validation?.valid ?? false)) {
      setOverridePreview(null);
      setOverridePreviewLoading(false);
      return;
    }

    const overrides: BatchOverrides = {
      maxTokens: parseNumber(maxTokens),
      temperature: parseNumber(temperature),
      topP: parseNumber(topP),
      n: parseNumber(n),
    };

    if (!hasAnyOverride(overrides) || hasParamErrors) {
      setOverridePreview(null);
      setOverridePreviewLoading(false);
      return;
    }

    const { diff } = applyBatchOverrides(selectedFileParse.records, overrides);
    setOverridePreview(diff);
    setOverridePreviewLoading(false);
  }, [selectedFile, selectedFileParse, validation?.valid, maxTokens, temperature, topP, n, hasParamErrors]);

  const hasInferenceOverrides = hasAnyOverride({
    maxTokens: parseNumber(maxTokens),
    temperature: parseNumber(temperature),
    topP: parseNumber(topP),
    n: parseNumber(n),
  });
  const readiness = getCreateJobReadiness({
    selectedModel,
    selectedTemplate,
    displayName,
    hasParamErrors,
    submitting,
    hasValidUploadedFile: selectedFile != null && (validation?.valid ?? false),
    selectedExistingFileId: selectedExistingFile?.id ?? '',
    hasInferenceOverrides,
  });
  const canSubmit = readiness.canSubmit;
  const canContinueDataset = selectedExistingFile != null || (selectedFile != null && (validation?.valid ?? false));
  const selectedModelLabel = formatModelSelectionLabel(selectedModel, selectedServingName);

  const handleCreateJob = async () => {
    if (!canSubmit) return;
    setSubmitting(true);
    setSubmitError(null);
    try {
      let datasetId = selectedExistingFile?.id || uploadedFileId;
      if (selectedFile && !datasetId) {
        setUploading(true);
        try {
          const overrides: BatchOverrides = {
            maxTokens: parseNumber(maxTokens),
            temperature: parseNumber(temperature),
            topP: parseNumber(topP),
            n: parseNumber(n),
          };

          let fileToUpload: File = selectedFile;
          if (hasAnyOverride(overrides)) {
            const parsed = selectedFileParse ?? parseJsonl(await selectedFile.text());
            const { records } = applyBatchOverrides(parsed.records, overrides);
            fileToUpload = new File(
              [serializeJsonl(records)],
              selectedFile.name,
              { type: 'application/jsonl' },
            );
          }

          const fileInfo = await uploadFile(fileToUpload, 'batch');
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
      if (!datasetId) {
        setSubmitError('Select or upload a dataset before creating a job.');
        return;
      }

      // Pick the endpoint from JSONL validation; default to chat completions.
      const endpoint: JobEndpoint =
        getCreateJobEndpoint({
          validationEndpoints: validation?.endpoints ?? [],
          selectedEndpoint,
          selectedTemplate,
        }) as JobEndpoint;

      // maxTokens/temperature/topP/n are baked into the JSONL via
      // applyBatchOverrides above; no need to forward them to BFF.
      await createJob({
        inputDataset: datasetId || '',
        endpoint,
        completionWindow: '24h',
        name: displayName,
        modelTemplateName: selectedTemplate?.name,
        modelTemplateVersion: selectedTemplate?.version,
        modelId: selectedModelId || undefined,
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

            <div className="mb-4">
              <label className="block text-sm mb-2">Choose a model to run batch inference.</label>
              <div className="relative">
                <button
                  onClick={() => setShowModelDropdown(!showModelDropdown)}
                  className="w-full px-4 py-2 border border-gray-200 rounded-lg text-left flex items-center justify-between hover:bg-gray-50"
                >
                  <span className="text-sm truncate pr-2">{selectedModelLabel || 'Select Model'}</span>
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
                            <div className="w-5 h-5 rounded-full bg-teal-50 flex items-center justify-center shrink-0">
                              <div className="w-2 h-2 rounded-full bg-teal-500"></div>
                            </div>
                            <span className="min-w-0 flex-1 truncate">
                              {formatModelSelectionLabel(model.name, model.servingName)}
                            </span>
                          </button>
                        ))
                      )}
                    </div>
                  </div>
                )}
              </div>
            </div>
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
                    accept=".jsonl"
                    className="hidden"
                  />
                  {selectedFile ? (
                    <div
                      onDragOver={handleDatasetDragOver}
                      onDragLeave={handleDatasetDragLeave}
                      onDrop={handleDatasetDrop}
                      className={`border-2 rounded-xl p-4 flex items-center justify-between ${
                        validation?.valid === false
                          ? 'border-red-200 bg-red-50'
                          : datasetDragActive
                            ? 'border-teal-500 bg-teal-50'
                            : 'border-teal-200 bg-teal-50'
                      }`}
                    >
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
                      onDragOver={handleDatasetDragOver}
                      onDragLeave={handleDatasetDragLeave}
                      onDrop={handleDatasetDrop}
                      className={`border-2 border-dashed rounded-xl p-8 text-center transition-colors cursor-pointer ${
                        datasetDragActive
                          ? 'border-teal-500 bg-teal-50'
                          : 'border-gray-200 hover:border-teal-400'
                      }`}
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

                {selectedExistingFile ? (
                  <div className="border border-teal-200 bg-teal-50 rounded-xl p-4 flex items-center justify-between">
                    <div className="flex items-center gap-3 min-w-0">
                      <FileText className="w-5 h-5 text-teal-600 shrink-0" />
                      <div className="min-w-0">
                        <div className="text-sm text-gray-900 truncate">
                          {selectedExistingFile.name || selectedExistingFile.id}
                        </div>
                        <div className="text-xs text-gray-500 truncate">
                          {selectedExistingFile.id} · {formatBytes(selectedExistingFile.size)}
                        </div>
                      </div>
                    </div>
                    <button
                      onClick={handleClearExistingFile}
                      className="text-gray-400 hover:text-gray-600"
                      title="Clear selected dataset"
                    >
                      <X className="w-4 h-4" />
                    </button>
                  </div>
                ) : (
                  <button
                    type="button"
                    onClick={loadExistingFiles}
                    className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm hover:bg-gray-50"
                  >
                    Select an existing dataset
                  </button>
                )}

                {showExistingFiles && (
                  <div className="mt-3 border border-gray-200 rounded-xl bg-white overflow-hidden">
                    <div className="p-3 border-b border-gray-100 flex items-center gap-2">
                      <div className="relative flex-1">
                        <Search className="absolute left-3 top-1/2 transform -translate-y-1/2 w-4 h-4 text-gray-400" />
                        <input
                          type="text"
                          placeholder="Search files by name or id"
                          value={fileSearchQuery}
                          onChange={(e) => setFileSearchQuery(e.target.value)}
                          className="w-full pl-10 pr-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500"
                        />
                      </div>
                      <button
                        type="button"
                        onClick={loadExistingFiles}
                        className="w-9 h-9 rounded-lg border border-gray-200 flex items-center justify-center hover:bg-gray-50"
                        title="Refresh files"
                      >
                        <RefreshCw className={`w-4 h-4 ${filesLoading ? 'animate-spin' : ''}`} />
                      </button>
                    </div>
                    <div className="max-h-64 overflow-y-auto p-2">
                      {filesLoading ? (
                        <div className="flex items-center justify-center gap-2 py-6 text-sm text-gray-500">
                          <Loader2 className="w-4 h-4 animate-spin" />
                          Loading files...
                        </div>
                      ) : filesError ? (
                        <div className="text-sm text-red-700 bg-red-50 border border-red-200 rounded-lg p-3">
                          Failed to load files: {filesError}
                        </div>
                      ) : batchFiles.length === 0 ? (
                        <div className="text-sm text-gray-500 text-center py-6">
                          No batch files found. Upload a JSONL dataset instead.
                        </div>
                      ) : (
                        batchFiles.map((file) => (
                          <button
                            key={file.id}
                            type="button"
                            onClick={() => handleSelectExistingFile(file)}
                            className="w-full flex items-center justify-between gap-3 px-3 py-2.5 rounded-lg text-left hover:bg-gray-50"
                          >
                            <div className="flex items-center gap-3 min-w-0">
                              <FileText className="w-4 h-4 text-gray-400 shrink-0" />
                              <div className="min-w-0">
                                <div className="text-sm text-gray-900 truncate">{file.name || file.id}</div>
                                <div className="text-xs text-gray-400 truncate">{file.id}</div>
                              </div>
                            </div>
                            <div className="text-right shrink-0">
                              <div className="text-xs text-gray-500">{formatBytes(file.size)}</div>
                              <div className="text-[11px] text-gray-400">{formatFileCreatedAt(file.createdAt)}</div>
                            </div>
                          </button>
                        ))
                      )}
                    </div>
                    <div className="px-3 py-2 border-t border-gray-100 text-xs text-gray-500">
                      Existing files are not re-read in the browser; make sure the JSONL endpoint matches the selected template.
                    </div>
                  </div>
                )}

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
                      disabled={!canContinueDataset}
                      className={`flex-1 px-4 py-2 rounded-lg text-sm text-white ${
                        !canContinueDataset
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
                  <label className="block text-sm mb-2">Output dataset</label>
                  <div className="px-4 py-2 border border-gray-200 rounded-lg text-sm text-gray-500 bg-gray-50">
                    Created automatically when the batch completes.
                  </div>
                </div>

                <BatchEndpointControl
                  template={selectedTemplate}
                  selectedEndpoint={selectedEndpoint}
                  detectedEndpoint={validation?.endpoints[0] ?? ''}
                  onChange={setSelectedEndpoint}
                />

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

                  <ParameterOverridePreview
                    loading={overridePreviewLoading}
                    diff={overridePreview}
                    hasOverrides={hasInferenceOverrides}
                    usingExistingFile={selectedExistingFile != null}
                  />
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
                {!canSubmit && readiness.reason && (
                  <p className="text-xs text-gray-500 text-right">{readiness.reason}</p>
                )}
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
{getBatchExampleJsonlLine(selectedTemplate, selectedServingName || selectedModel)}
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
                  <div className="mb-1">Output Dataset:</div>
                  <p className="text-gray-500">The output file is created automatically after successful requests complete.</p>
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

function ParameterOverridePreview({
  loading,
  diff,
  hasOverrides,
  usingExistingFile,
}: {
  loading: boolean;
  diff: MutationDiff | null;
  hasOverrides: boolean;
  usingExistingFile: boolean;
}) {
  if (!hasOverrides) return null;

  if (usingExistingFile) {
    return (
      <div className="mt-4 flex items-start gap-2 text-sm text-amber-700 bg-amber-50 border border-amber-200 rounded-lg p-3">
        <AlertCircle className="w-4 h-4 mt-0.5 shrink-0" />
        <div>
          <div className="font-medium">Overrides require an uploaded file</div>
          <p className="text-xs mt-1">
            Existing files are not downloaded and rewritten in the browser. Clear these overrides or upload a JSONL file if you want to apply batch-level parameters.
          </p>
        </div>
      </div>
    );
  }

  if (loading) {
    return (
      <div className="mt-4 flex items-center gap-2 text-sm text-gray-500">
        <Loader2 className="w-4 h-4 animate-spin" />
        Calculating override preview...
      </div>
    );
  }

  if (!diff) return null;

  const changedFields = Object.entries(diff.fieldsChanged);
  return (
    <div className="mt-4 border border-teal-200 bg-teal-50 rounded-lg p-3 text-sm">
      <div className="flex items-start gap-2">
        <CheckCircle2 className="w-4 h-4 mt-0.5 text-teal-600 shrink-0" />
        <div className="min-w-0">
          <div className="font-medium text-teal-800">
            {diff.changedLines} of {diff.totalLines} request lines will be updated
          </div>
          {changedFields.length > 0 && (
            <div className="mt-2 flex flex-wrap gap-1.5">
              {changedFields.map(([field, count]) => (
                <span key={field} className="px-2 py-1 bg-white/70 rounded-md text-xs text-teal-800 font-mono">
                  {field}: {count}
                </span>
              ))}
            </div>
          )}
          {diff.samples.length > 0 && (
            <div className="mt-2 text-xs text-teal-700">
              Sample: line {diff.samples[0].lineNumber} changes {diff.samples[0].field}
            </div>
          )}
          {diff.skipped.length > 0 && (
            <div className="mt-2 text-xs text-amber-700">
              {diff.skipped.length} field updates skipped because their endpoint does not accept that parameter.
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function BatchEndpointControl({
  template,
  selectedEndpoint,
  detectedEndpoint,
  onChange,
}: {
  template: ModelDeploymentTemplate | null;
  selectedEndpoint: string;
  detectedEndpoint: string;
  onChange: (value: string) => void;
}) {
  const supported = template?.spec?.supportedEndpoints ?? [];
  const effectiveEndpoint = detectedEndpoint || selectedEndpoint || supported[0] || '/v1/chat/completions';

  if (detectedEndpoint) {
    return (
      <div>
        <label className="block text-sm mb-2">Batch endpoint</label>
        <div className="px-4 py-2 border border-gray-200 rounded-lg text-sm text-gray-700 bg-gray-50">
          {effectiveEndpoint}
        </div>
        <p className="text-xs text-gray-400 mt-1">Detected from the uploaded JSONL file.</p>
      </div>
    );
  }

  if (supported.length > 1) {
    return (
      <div>
        <label className="block text-sm mb-2">Batch endpoint</label>
        <select
          value={effectiveEndpoint}
          onChange={(e) => onChange(e.target.value)}
          className="w-full px-4 py-2 border border-gray-200 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-teal-500/30 focus:border-teal-500 bg-white"
        >
          {supported.map((endpoint) => (
            <option key={endpoint} value={endpoint}>{endpoint}</option>
          ))}
        </select>
        <p className="text-xs text-gray-400 mt-1">For existing files, choose the endpoint used in the JSONL request url.</p>
      </div>
    );
  }

  return (
    <div>
      <label className="block text-sm mb-2">Batch endpoint</label>
      <div className="px-4 py-2 border border-gray-200 rounded-lg text-sm text-gray-700 bg-gray-50">
        {effectiveEndpoint}
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
