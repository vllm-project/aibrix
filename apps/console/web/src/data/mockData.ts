// JobStatus is the console-facing job lifecycle. It includes the planner's
// pre-submit statuses plus the downstream MDS/OpenAI batch statuses.
export type JobStatus =
  | 'pending'
  | 'provisioning'
  | 'validating'
  | 'in_progress'
  | 'finalizing'
  | 'completed'
  | 'failed'
  | 'expired'
  | 'cancelling'
  | 'cancelled';

export interface JobUsage {
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
}

export interface JobRequestCounts {
  total: number;
  completed: number;
  failed: number;
}

// Job is the Console BFF's view of a batch — superset of OpenAI Batch.
// Timestamps are unix seconds.
export interface Job {
  id: string;
  object: string;
  endpoint: string;
  model: string;
  inputDataset: string;
  completionWindow: string;
  status: JobStatus;
  outputDataset?: string;
  errorDataset?: string;
  createdAt: number;
  inProgressAt?: number;
  expiresAt?: number;
  finalizingAt?: number;
  completedAt?: number;
  failedAt?: number;
  expiredAt?: number;
  cancellingAt?: number;
  cancelledAt?: number;
  requestCounts?: JobRequestCounts;
  usage?: JobUsage;
  metadata?: Record<string, string>;

  // Console-side fields persisted in the Console store
  name: string;
  createdBy: string;
  // Resolved deployment-template binding. Populated when the user picks a
  // template in the create-job wizard; empty for legacy/SDK-path jobs.
  modelTemplateName?: string;
  modelTemplateVersion?: string;
}

export interface Deployment {
  id: string;
  name: string;
  deploymentId: string;
  baseModel: string;
  baseModelId: string;
  replicas: string;
  gpusPerReplica: number;
  gpuType: string;
  region: string;
  createdBy: string;
  status: 'Ready' | 'Deploying' | 'Failed';
}

export type ModelCategory = 'LLM' | 'Audio' | 'Image' | 'Video' | 'Vision' | 'Embedding' | 'Reranks';

export interface Model {
  id: string;
  name: string;
  iconBg: string;
  iconText: string;
  iconTextColor: string;
  categories: ModelCategory[];
  isNew?: boolean;
  pricing: {
    uncachedInput?: string;
    cachedInput?: string;
    output?: string;
    perMinute?: string;
    perImage?: string;
  };
  contextLength?: string;
  description: string;
  metadata: {
    state: string;
    createdOn: string;
    providerName: string;
    huggingFace?: string;
  };
  specification: {
    calibrated: boolean;
    mixtureOfExperts: boolean;
    parameters: string;
  };
  tags: string[];
  // Identifier callers must put in `body.model` for inference. Empty when the
  // model isn't deployed yet — the validator should skip the identifier check.
  servingName?: string;
}

// Mock jobs used as a fallback when the Console BFF is unreachable
// (e.g. local dev without the metadata service running).
export const mockJobs: Job[] = [
  {
    id: 'batch_demo_27a6ee2c',
    object: 'batch',
    endpoint: '/v1/chat/completions',
    model: 'qwen2p5-32b-instruct',
    inputDataset: 'file-demo-gsm8k-math-dataset-1000',
    completionWindow: '24h',
    status: 'completed',
    outputDataset: 'file-demo-output-27a6ee2c',
    createdAt: 1737230220,
    inProgressAt: 1737230400,
    expiresAt: 1737316620,
    completedAt: 1737231240,
    requestCounts: { total: 1000, completed: 1000, failed: 0 },
    usage: { inputTokens: 476498, outputTokens: 16856, totalTokens: 493354 },
    name: 'gsm-8k-20260118',
    createdBy: 'demo@aibrix.ai',
  },
  {
    id: 'batch_demo_a0b13ef5',
    object: 'batch',
    endpoint: '/v1/chat/completions',
    model: 'qwen2p5-32b-instruct',
    inputDataset: 'file-demo-gsm8k-math-dataset-1000',
    completionWindow: '24h',
    status: 'validating',
    createdAt: 1737232500,
    expiresAt: 1737318900,
    name: 'gsm-8k-20260118-v2',
    createdBy: 'demo@aibrix.ai',
  },
];

export const mockDeployments: Deployment[] = [
  {
    id: 'deploy-1',
    name: 'gsm-8k-20260118',
    deploymentId: 'euxdnr5z',
    baseModel: 'DeepSeek R1 Distill Llama 8B',
    baseModelId: 'deepseek-r1-distil-llama-8b',
    replicas: '1[01]',
    gpusPerReplica: 1,
    gpuType: 'NVIDIA H100 80GB',
    region: 'US Iowa 1',
    createdBy: 'seedjeffwan@gmail.com',
    status: 'Ready',
  },
];

export const mockModels: Model[] = [
  // LLM models
  {
    id: 'model-minimax-m2.5',
    name: 'MiniMax-M2.5',
    iconBg: 'bg-red-100',
    iconText: 'M',
    iconTextColor: 'text-red-600',
    categories: ['LLM'],
    isNew: true,
    pricing: { uncachedInput: '$0.30/M', cachedInput: '$0.03/M', output: '$1.20/M' },
    contextLength: '192k Context',
    description: 'MiniMax-M2.5 is a powerful language model designed for complex reasoning and code generation tasks. It features an advanced attention mechanism for efficient long-context processing and excels at multi-turn conversations.',
    metadata: { state: 'Ready', createdOn: 'Feb 10, 2026', providerName: 'MiniMax', huggingFace: 'minimax/MiniMax-M2.5' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '250B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-glm-5',
    name: 'GLM-5',
    iconBg: 'bg-green-100',
    iconText: 'Z',
    iconTextColor: 'text-green-700',
    categories: ['LLM'],
    isNew: true,
    pricing: { uncachedInput: '$1.00/M', cachedInput: '$0.20/M', output: '$3.20/M' },
    contextLength: '198k Context',
    description: 'GLM-5 is Z.ai\'s SOTA model targeting complex systems engineering and long-horizon agentic tasks. It uses a mixture of experts architecture, so it only activates 40 billion of its 744 billion parameters. This model uses Deepseek Sparse Attention to select only the most relevant tokens for attention, reducing the cost of long-context processing. GLM-5 continues improving on top of GLM-4.7 for coding and agentic use cases, and it\'s also great for document generation for enterprise workloads.',
    metadata: { state: 'Ready', createdOn: 'Feb 12, 2026', providerName: 'Z.ai', huggingFace: 'zai-org/GLM-5' },
    specification: { calibrated: false, mixtureOfExperts: true, parameters: '700B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-kimi-k2.5',
    name: 'Kimi K2.5',
    iconBg: 'bg-gray-100',
    iconText: 'K',
    iconTextColor: 'text-gray-800',
    categories: ['LLM', 'Vision'],
    isNew: true,
    pricing: { uncachedInput: '$0.60/M', cachedInput: '$0.10/M', output: '$3.00/M' },
    contextLength: '256k Context',
    description: 'Kimi K2.5 is a multimodal language model from Moonshot AI that supports both text and vision inputs. It excels at reasoning tasks, code generation, and visual understanding with a large 256k context window.',
    metadata: { state: 'Ready', createdOn: 'Feb 8, 2026', providerName: 'Moonshot AI', huggingFace: 'moonshot/kimi-k2.5' },
    specification: { calibrated: false, mixtureOfExperts: true, parameters: '400B' },
    tags: ['Serverless', 'Tunable'],
  },
  {
    id: 'model-minimax-m2.1',
    name: 'MiniMax-M2.1',
    iconBg: 'bg-red-100',
    iconText: 'M',
    iconTextColor: 'text-red-600',
    categories: ['LLM'],
    pricing: { uncachedInput: '$0.30/M', cachedInput: '$0.03/M', output: '$1.20/M' },
    contextLength: '200k Context',
    description: 'MiniMax-M2.1 is a reliable language model optimized for text generation and reasoning. It provides strong performance across a variety of benchmarks with efficient inference.',
    metadata: { state: 'Ready', createdOn: 'Jan 20, 2026', providerName: 'MiniMax', huggingFace: 'minimax/MiniMax-M2.1' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '200B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-glm-4.7',
    name: 'GLM-4.7',
    iconBg: 'bg-green-100',
    iconText: 'Z',
    iconTextColor: 'text-green-700',
    categories: ['LLM'],
    pricing: { uncachedInput: '$0.60/M', cachedInput: '$0.30/M', output: '$2.20/M' },
    contextLength: '198k Context',
    description: 'GLM-4.7 is a high-performance language model from Z.ai optimized for coding and enterprise document generation tasks. It features efficient long-context processing.',
    metadata: { state: 'Ready', createdOn: 'Jan 15, 2026', providerName: 'Z.ai', huggingFace: 'zai-org/GLM-4.7' },
    specification: { calibrated: false, mixtureOfExperts: true, parameters: '500B' },
    tags: ['Serverless', 'Tunable'],
  },
  {
    id: 'model-deepseek-v3.2',
    name: 'Deepseek v3.2',
    iconBg: 'bg-purple-100',
    iconText: 'D',
    iconTextColor: 'text-purple-700',
    categories: ['LLM'],
    pricing: { uncachedInput: '$0.56/M' },
    contextLength: '128k Context',
    description: 'Deepseek v3.2 is the latest iteration of the DeepSeek language model series, featuring improved reasoning capabilities and coding performance. It uses a mixture of experts architecture for efficient inference.',
    metadata: { state: 'Ready', createdOn: 'Jan 25, 2026', providerName: 'DeepSeek', huggingFace: 'deepseek-ai/deepseek-v3.2' },
    specification: { calibrated: false, mixtureOfExperts: true, parameters: '671B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-qwen3.5-397b',
    name: 'Qwen3.5 397B A17B (Beta)',
    iconBg: 'bg-purple-100',
    iconText: 'Q',
    iconTextColor: 'text-purple-600',
    categories: ['LLM', 'Vision'],
    isNew: true,
    pricing: {},
    contextLength: '256k Context',
    description: 'Qwen3.5 397B A17B is Alibaba\'s flagship vision-language model in beta, combining strong text reasoning with advanced visual understanding capabilities.',
    metadata: { state: 'Ready', createdOn: 'Feb 14, 2026', providerName: 'Alibaba', huggingFace: 'Qwen/Qwen3.5-397B-A17B' },
    specification: { calibrated: false, mixtureOfExperts: true, parameters: '397B' },
    tags: [],
  },
  {
    id: 'model-llama-3.3-70b',
    name: 'Llama 3.3 70B Instruct',
    iconBg: 'bg-blue-100',
    iconText: 'L',
    iconTextColor: 'text-blue-700',
    categories: ['LLM'],
    pricing: { uncachedInput: '$0.20/M', output: '$0.20/M' },
    contextLength: '128k Context',
    description: 'Meta\'s Llama 3.3 70B Instruct model is optimized for instruction following and multi-turn dialogue, delivering strong performance with efficient inference.',
    metadata: { state: 'Ready', createdOn: 'Dec 10, 2025', providerName: 'Meta', huggingFace: 'meta-llama/Llama-3.3-70B-Instruct' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '70B' },
    tags: ['Serverless', 'Tunable', 'Function Calling'],
  },

  // Audio models
  {
    id: 'model-streaming-asr-v1',
    name: 'Streaming ASR v1',
    iconBg: 'bg-green-100',
    iconText: 'S',
    iconTextColor: 'text-green-600',
    categories: ['Audio'],
    pricing: { perMinute: '$0.0032/minute' },
    description: 'Streaming ASR v1 provides real-time automatic speech recognition with low latency, suitable for live transcription and voice interfaces.',
    metadata: { state: 'Ready', createdOn: 'Nov 5, 2025', providerName: 'AIBrix' },
    specification: { calibrated: true, mixtureOfExperts: false, parameters: '1.5B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-streaming-asr-v2',
    name: 'Streaming ASR v2',
    iconBg: 'bg-green-100',
    iconText: 'S',
    iconTextColor: 'text-green-600',
    categories: ['Audio'],
    pricing: { perMinute: '$0.0035/minute' },
    description: 'Streaming ASR v2 is an improved version of the streaming speech recognition model with better accuracy and support for more languages.',
    metadata: { state: 'Ready', createdOn: 'Jan 12, 2026', providerName: 'AIBrix' },
    specification: { calibrated: true, mixtureOfExperts: false, parameters: '2B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-whisper-v3-large',
    name: 'Whisper V3 Large',
    iconBg: 'bg-gray-100',
    iconText: 'W',
    iconTextColor: 'text-gray-700',
    categories: ['Audio'],
    pricing: { perMinute: '$0.0015/minute' },
    description: 'OpenAI\'s Whisper V3 Large is a general-purpose speech recognition model trained on a large dataset of diverse audio, supporting multilingual transcription and translation.',
    metadata: { state: 'Ready', createdOn: 'Oct 20, 2025', providerName: 'OpenAI', huggingFace: 'openai/whisper-large-v3' },
    specification: { calibrated: true, mixtureOfExperts: false, parameters: '1.5B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-whisper-v3-turbo',
    name: 'Whisper V3 Turbo',
    iconBg: 'bg-gray-100',
    iconText: 'W',
    iconTextColor: 'text-gray-700',
    categories: ['Audio'],
    pricing: { perMinute: '$0.0009/minute' },
    description: 'Whisper V3 Turbo is a faster, more cost-effective variant of the Whisper V3 model, optimized for high-throughput transcription workloads.',
    metadata: { state: 'Ready', createdOn: 'Nov 1, 2025', providerName: 'OpenAI', huggingFace: 'openai/whisper-large-v3-turbo' },
    specification: { calibrated: true, mixtureOfExperts: false, parameters: '800M' },
    tags: ['Serverless'],
  },

  // Image models
  {
    id: 'model-flux-kontext-pro',
    name: 'FLUX.1 Kontext Pro',
    iconBg: 'bg-gray-200',
    iconText: '△',
    iconTextColor: 'text-gray-700',
    categories: ['Image'],
    pricing: { perImage: '$0.04/ea' },
    description: 'FLUX.1 Kontext Pro is a professional-grade image generation model that supports context-aware editing and generation with high fidelity output.',
    metadata: { state: 'Ready', createdOn: 'Feb 1, 2026', providerName: 'Black Forest Labs' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '12B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-flux-kontext-max',
    name: 'FLUX.1 Kontext Max',
    iconBg: 'bg-gray-200',
    iconText: '△',
    iconTextColor: 'text-gray-700',
    categories: ['Image'],
    pricing: { perImage: '$0.08/ea' },
    description: 'FLUX.1 Kontext Max is the highest-quality variant of the Kontext series, delivering maximum fidelity and detail in generated images.',
    metadata: { state: 'Ready', createdOn: 'Feb 1, 2026', providerName: 'Black Forest Labs' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '24B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-flux-dev-fp8',
    name: 'FLUX.1 [dev] FP8',
    iconBg: 'bg-gray-200',
    iconText: '△',
    iconTextColor: 'text-gray-700',
    categories: ['Image'],
    pricing: {},
    description: 'FLUX.1 [dev] FP8 is the developer-focused variant with FP8 quantization for faster inference while maintaining high image quality.',
    metadata: { state: 'Ready', createdOn: 'Jan 5, 2026', providerName: 'Black Forest Labs' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '12B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-flux-schnell-fp8',
    name: 'FLUX.1 [schnell] FP8',
    iconBg: 'bg-gray-200',
    iconText: '△',
    iconTextColor: 'text-gray-700',
    categories: ['Image'],
    pricing: {},
    description: 'FLUX.1 [schnell] FP8 is optimized for fast generation with FP8 precision, ideal for rapid prototyping and high-throughput image generation.',
    metadata: { state: 'Ready', createdOn: 'Jan 5, 2026', providerName: 'Black Forest Labs' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '12B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-stable-diffusion-xl',
    name: 'Stable Diffusion XL',
    iconBg: 'bg-red-100',
    iconText: 'S.',
    iconTextColor: 'text-red-600',
    categories: ['Image'],
    pricing: {},
    description: 'Stable Diffusion XL is a high-resolution image generation model from Stability AI that produces photorealistic images with detailed compositions.',
    metadata: { state: 'Ready', createdOn: 'Sep 15, 2025', providerName: 'Stability AI', huggingFace: 'stabilityai/stable-diffusion-xl-base-1.0' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '3.5B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-playground-v2-1024',
    name: 'Playground v2 1024',
    iconBg: 'bg-purple-100',
    iconText: 'P',
    iconTextColor: 'text-purple-600',
    categories: ['Image'],
    pricing: {},
    description: 'Playground v2 1024 generates high-quality 1024x1024 images with excellent prompt adherence and artistic quality.',
    metadata: { state: 'Ready', createdOn: 'Oct 1, 2025', providerName: 'Playground' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '2.5B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-playground-v2.5-1024',
    name: 'Playground v2.5 1024',
    iconBg: 'bg-purple-100',
    iconText: 'P',
    iconTextColor: 'text-purple-600',
    categories: ['Image'],
    pricing: {},
    description: 'Playground v2.5 1024 is an improved version with better color accuracy, contrast, and prompt following capabilities.',
    metadata: { state: 'Ready', createdOn: 'Nov 1, 2025', providerName: 'Playground' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '2.5B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-segmind-ssd-1b',
    name: 'Segmind Stable Diffusion 1B (SSD-1B)',
    iconBg: 'bg-pink-100',
    iconText: 'Sg',
    iconTextColor: 'text-pink-600',
    categories: ['Image'],
    pricing: {},
    description: 'Segmind SSD-1B is a compact and efficient image generation model that delivers fast results with good quality at a lower computational cost.',
    metadata: { state: 'Ready', createdOn: 'Aug 20, 2025', providerName: 'Segmind' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '1B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-japanese-sdxl',
    name: 'Japanese Stable Diffusion XL',
    iconBg: 'bg-red-100',
    iconText: 'S.',
    iconTextColor: 'text-red-600',
    categories: ['Image'],
    pricing: {},
    description: 'Japanese Stable Diffusion XL is fine-tuned for generating images with Japanese aesthetics and cultural elements, supporting Japanese text prompts.',
    metadata: { state: 'Ready', createdOn: 'Oct 10, 2025', providerName: 'Stability AI' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '3.5B' },
    tags: ['Serverless'],
  },

  // Vision models
  {
    id: 'model-qwen3-vl-235b-thinking',
    name: 'Qwen3 VL 235B A22B Thinking',
    iconBg: 'bg-purple-100',
    iconText: 'Q',
    iconTextColor: 'text-purple-600',
    categories: ['LLM', 'Vision'],
    pricing: {},
    contextLength: '256k Context',
    description: 'Qwen3 VL 235B A22B Thinking combines vision and language understanding with advanced chain-of-thought reasoning capabilities for complex multimodal tasks.',
    metadata: { state: 'Ready', createdOn: 'Feb 5, 2026', providerName: 'Alibaba', huggingFace: 'Qwen/Qwen3-VL-235B-A22B-Thinking' },
    specification: { calibrated: false, mixtureOfExperts: true, parameters: '235B' },
    tags: ['Tunable'],
  },
  {
    id: 'model-qwen3-vl-235b-instruct',
    name: 'Qwen3 VL 235B A22B Instruct',
    iconBg: 'bg-purple-100',
    iconText: 'Q',
    iconTextColor: 'text-purple-600',
    categories: ['LLM', 'Vision'],
    pricing: {},
    contextLength: '256k Context',
    description: 'Qwen3 VL 235B A22B Instruct is optimized for instruction-following tasks with both text and image inputs, suitable for vision-language applications.',
    metadata: { state: 'Ready', createdOn: 'Feb 5, 2026', providerName: 'Alibaba', huggingFace: 'Qwen/Qwen3-VL-235B-A22B-Instruct' },
    specification: { calibrated: false, mixtureOfExperts: true, parameters: '235B' },
    tags: ['Tunable'],
  },

  // Embedding models
  {
    id: 'model-qwen3-reranker-8b',
    name: 'Qwen3 Reranker 8B',
    iconBg: 'bg-purple-100',
    iconText: 'Q',
    iconTextColor: 'text-purple-600',
    categories: ['Embedding'],
    pricing: { uncachedInput: '$0.20/M' },
    contextLength: '40k Context',
    description: 'Qwen3 Reranker 8B is a cross-encoder model for reranking search results with high precision, optimized for retrieval-augmented generation pipelines.',
    metadata: { state: 'Ready', createdOn: 'Feb 1, 2026', providerName: 'Alibaba', huggingFace: 'Qwen/Qwen3-Reranker-8B' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '8B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-qwen3-embedding-8b',
    name: 'Qwen3 Embedding 8B',
    iconBg: 'bg-purple-100',
    iconText: 'Q',
    iconTextColor: 'text-purple-600',
    categories: ['Embedding'],
    pricing: { uncachedInput: '$0.10/M' },
    contextLength: '40k Context',
    description: 'Qwen3 Embedding 8B generates high-quality text embeddings for semantic search, clustering, and classification tasks.',
    metadata: { state: 'Ready', createdOn: 'Feb 1, 2026', providerName: 'Alibaba', huggingFace: 'Qwen/Qwen3-Embedding-8B' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '8B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-nomic-embed-text',
    name: 'Nomic Embed Text v1.5',
    iconBg: 'bg-teal-100',
    iconText: 'N',
    iconTextColor: 'text-teal-700',
    categories: ['Embedding'],
    pricing: { uncachedInput: '$0.008/M' },
    contextLength: '8k Context',
    description: 'Nomic Embed Text v1.5 is a lightweight, high-performance text embedding model with excellent retrieval accuracy for its parameter count.',
    metadata: { state: 'Ready', createdOn: 'Sep 1, 2025', providerName: 'Nomic AI', huggingFace: 'nomic-ai/nomic-embed-text-v1.5' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '137M' },
    tags: ['Serverless'],
  },
  {
    id: 'model-gte-base',
    name: 'GTE Base EN v1.5',
    iconBg: 'bg-purple-100',
    iconText: 'G',
    iconTextColor: 'text-purple-600',
    categories: ['Embedding'],
    pricing: { uncachedInput: '$0.008/M' },
    contextLength: '8k Context',
    description: 'GTE Base EN v1.5 is a general text embedding model trained on large-scale English corpus, providing strong semantic understanding.',
    metadata: { state: 'Ready', createdOn: 'Aug 15, 2025', providerName: 'Alibaba', huggingFace: 'Alibaba-NLP/gte-base-en-v1.5' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '109M' },
    tags: ['Serverless'],
  },
  {
    id: 'model-gte-large',
    name: 'GTE Large EN v1.5',
    iconBg: 'bg-purple-100',
    iconText: 'G',
    iconTextColor: 'text-purple-600',
    categories: ['Embedding'],
    pricing: { uncachedInput: '$0.016/M' },
    contextLength: '8k Context',
    description: 'GTE Large EN v1.5 is a larger embedding model with improved accuracy for nuanced semantic similarity and retrieval tasks.',
    metadata: { state: 'Ready', createdOn: 'Aug 15, 2025', providerName: 'Alibaba', huggingFace: 'Alibaba-NLP/gte-large-en-v1.5' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '335M' },
    tags: ['Serverless'],
  },
  {
    id: 'model-jina-embed',
    name: 'Jina Embeddings v3',
    iconBg: 'bg-orange-100',
    iconText: 'J',
    iconTextColor: 'text-orange-600',
    categories: ['Embedding'],
    pricing: { uncachedInput: '$0.02/M' },
    contextLength: '8k Context',
    description: 'Jina Embeddings v3 is a multilingual embedding model supporting 100+ languages with task-specific adapter layers for optimal performance.',
    metadata: { state: 'Ready', createdOn: 'Oct 1, 2025', providerName: 'Jina AI', huggingFace: 'jinaai/jina-embeddings-v3' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '560M' },
    tags: ['Serverless'],
  },

  // Reranks models
  {
    id: 'model-reranker-qwen3-8b',
    name: 'Qwen3 Reranker 8B',
    iconBg: 'bg-purple-100',
    iconText: 'Q',
    iconTextColor: 'text-purple-600',
    categories: ['Reranks'],
    pricing: { uncachedInput: '$0.20/M' },
    contextLength: '40k Context',
    description: 'Qwen3 Reranker 8B is a high-accuracy cross-encoder reranking model for improving search result relevance.',
    metadata: { state: 'Ready', createdOn: 'Feb 1, 2026', providerName: 'Alibaba', huggingFace: 'Qwen/Qwen3-Reranker-8B' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '8B' },
    tags: ['Serverless'],
  },
  {
    id: 'model-reranker-qwen3-4b',
    name: 'Qwen3 Reranker 4B',
    iconBg: 'bg-purple-100',
    iconText: 'Q',
    iconTextColor: 'text-purple-600',
    categories: ['Reranks'],
    pricing: {},
    contextLength: '40k Context',
    description: 'Qwen3 Reranker 4B is a compact reranking model that balances accuracy and efficiency for search result reranking.',
    metadata: { state: 'Ready', createdOn: 'Feb 1, 2026', providerName: 'Alibaba', huggingFace: 'Qwen/Qwen3-Reranker-4B' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '4B' },
    tags: [],
  },
  {
    id: 'model-reranker-qwen3-0.6b',
    name: 'Qwen3 Reranker 0.6B',
    iconBg: 'bg-purple-100',
    iconText: 'Q',
    iconTextColor: 'text-purple-600',
    categories: ['Reranks'],
    pricing: {},
    contextLength: '40k Context',
    description: 'Qwen3 Reranker 0.6B is the smallest reranking model in the Qwen3 series, designed for low-latency and edge deployment scenarios.',
    metadata: { state: 'Ready', createdOn: 'Feb 1, 2026', providerName: 'Alibaba', huggingFace: 'Qwen/Qwen3-Reranker-0.6B' },
    specification: { calibrated: false, mixtureOfExperts: false, parameters: '0.6B' },
    tags: [],
  },
];
