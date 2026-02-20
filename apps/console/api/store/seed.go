/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package store

import (
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
)

func strPtr(s string) *string { return &s }

func (s *MemoryStore) seedData() {
	s.seedDeployments()
	s.seedJobs()
	s.seedModels()
	s.seedAPIKeys()
	s.seedSecrets()
	s.seedQuotas()
}

func (s *MemoryStore) seedDeployments() {
	s.deployments = []*pb.Deployment{
		{
			Id:             "deploy-1",
			Name:           "gsm-8k-20260118",
			DeploymentId:   "euxdnr5z",
			BaseModel:      "DeepSeek R1 Distill Llama 8B",
			BaseModelId:    "deepseek-r1-distil-llama-8b",
			Replicas:       "1[01]",
			GpusPerReplica: 1,
			GpuType:        "NVIDIA H100 80GB",
			Region:         "US Iowa 1",
			CreatedBy:      "seedjeffwan@gmail.com",
			Status:         "Ready",
		},
	}
}

func (s *MemoryStore) seedJobs() {
	s.jobs = []*pb.Job{
		{
			Id:             "job-1",
			Name:           "gsm-8k-20260118",
			InferenceId:    "vpswvq8h",
			Model:          "qwen2p5-32b-instruct",
			ModelId:        "qwen1zp5-32b-instruct",
			InputDataset:   "demo-gsm8k-math-dataset-1000",
			InputDatasetId: "demo-gsm8k-math-dataset-1000",
			CreateDate:     "Jan 18, 2026",
			CreateTime:     "3:37 PM",
			CreatedBy:      "seedjeffwan@gmail.com",
			Status:         "Completed",
			FullPath:       "accounts/aibrix/batchInference/27a6ee2c",
		},
		{
			Id:             "job-2",
			Name:           "gsm-8k-20260118-v2",
			InferenceId:    "abc12345",
			Model:          "qwen2p5-32b-instruct",
			ModelId:        "qwen1zp5-32b-instruct",
			InputDataset:   "demo-gsm8k-math-dataset-1000",
			InputDatasetId: "demo-gsm8k-math-dataset-1000",
			CreateDate:     "Jan 18, 2026",
			CreateTime:     "4:15 PM",
			CreatedBy:      "seedjeffwan@gmail.com",
			Status:         "Validating",
			FullPath:       "accounts/aibrix/batchInference/a0b13ef5",
		},
	}
}

func (s *MemoryStore) seedModels() {
	s.models = []*pb.Model{
		{
			Id: "model-minimax-m2.5", Name: "MiniMax-M2.5", Provider: "MiniMax",
			IconBg: "bg-red-100", IconText: "M", IconTextColor: "text-red-600",
			Categories: []string{"LLM"}, IsNew: true,
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.30/M", CachedInput: "$0.03/M", Output: "$1.20/M"},
			ContextLength: "192k Context",
			Description:   "MiniMax-M2.5 is a powerful language model designed for complex reasoning and code generation tasks.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 10, 2026", Kind: "Base model", ProviderName: "MiniMax", HuggingFace: "minimax/MiniMax-M2.5"},
			Specification: &pb.ModelSpecification{Parameters: "250B"},
			Tags:          []string{"Serverless"},
		},
		{
			Id: "model-glm-5", Name: "GLM-5", Provider: "Z.ai",
			IconBg: "bg-green-100", IconText: "Z", IconTextColor: "text-green-700",
			Categories: []string{"LLM"}, IsNew: true,
			Pricing:       &pb.ModelPricing{UncachedInput: "$1.00/M", CachedInput: "$0.20/M", Output: "$3.20/M"},
			ContextLength: "198k Context",
			Description:   "GLM-5 is Z.ai's SOTA model targeting complex systems engineering and long-horizon agentic tasks.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 12, 2026", Kind: "Base model", ProviderName: "Z.ai", HuggingFace: "zai-org/GLM-5"},
			Specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "700B"},
			Tags:          []string{"Serverless"},
		},
		{
			Id: "model-kimi-k2.5", Name: "Kimi K2.5", Provider: "Moonshot AI",
			IconBg: "bg-gray-100", IconText: "K", IconTextColor: "text-gray-800",
			Categories: []string{"LLM", "Vision"}, IsNew: true,
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.60/M", CachedInput: "$0.10/M", Output: "$3.00/M"},
			ContextLength: "256k Context",
			Description:   "Kimi K2.5 is a multimodal language model from Moonshot AI that supports both text and vision inputs.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 8, 2026", Kind: "Base model", ProviderName: "Moonshot AI", HuggingFace: "moonshot/kimi-k2.5"},
			Specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "400B"},
			Tags:          []string{"Serverless", "Tunable"},
		},
		{
			Id: "model-deepseek-v3.2", Name: "Deepseek v3.2", Provider: "DeepSeek",
			IconBg: "bg-purple-100", IconText: "D", IconTextColor: "text-purple-700",
			Categories: []string{"LLM"},
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.56/M"},
			ContextLength: "128k Context",
			Description:   "Deepseek v3.2 is the latest iteration of the DeepSeek language model series.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Jan 25, 2026", Kind: "Base model", ProviderName: "DeepSeek", HuggingFace: "deepseek-ai/deepseek-v3.2"},
			Specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "671B"},
			Tags:          []string{"Serverless"},
		},
		{
			Id: "model-llama-3.3-70b", Name: "Llama 3.3 70B Instruct", Provider: "Meta",
			IconBg: "bg-blue-100", IconText: "L", IconTextColor: "text-blue-700",
			Categories: []string{"LLM"},
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.20/M", Output: "$0.20/M"},
			ContextLength: "128k Context",
			Description:   "Meta's Llama 3.3 70B Instruct model is optimized for instruction following and multi-turn dialogue.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Dec 10, 2025", Kind: "Base model", ProviderName: "Meta", HuggingFace: "meta-llama/Llama-3.3-70B-Instruct"},
			Specification: &pb.ModelSpecification{Parameters: "70B"},
			Tags:          []string{"Serverless", "Tunable", "Function Calling"},
		},
		// Audio
		{
			Id: "model-whisper-v3-large", Name: "Whisper V3 Large", Provider: "OpenAI",
			IconBg: "bg-gray-100", IconText: "W", IconTextColor: "text-gray-700",
			Categories:    []string{"Audio"},
			Pricing:       &pb.ModelPricing{PerMinute: "$0.0015/minute"},
			Description:   "OpenAI's Whisper V3 Large is a general-purpose speech recognition model.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Oct 20, 2025", Kind: "Base model", ProviderName: "OpenAI", HuggingFace: "openai/whisper-large-v3"},
			Specification: &pb.ModelSpecification{Calibrated: true, Parameters: "1.5B"},
			Tags:          []string{"Serverless"},
		},
		// Image
		{
			Id: "model-flux-kontext-pro", Name: "FLUX.1 Kontext Pro", Provider: "Black Forest Labs",
			IconBg: "bg-gray-200", IconText: "△", IconTextColor: "text-gray-700",
			Categories:    []string{"Image"},
			Pricing:       &pb.ModelPricing{PerEa: "$0.04/ea"},
			Description:   "FLUX.1 Kontext Pro is a professional-grade image generation model.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 1, 2026", Kind: "Base model", ProviderName: "Black Forest Labs"},
			Specification: &pb.ModelSpecification{Parameters: "12B"},
			Tags:          []string{"Serverless"},
		},
		// Embedding
		{
			Id: "model-nomic-embed-text", Name: "Nomic Embed Text v1.5", Provider: "Nomic AI",
			IconBg: "bg-teal-100", IconText: "N", IconTextColor: "text-teal-700",
			Categories:    []string{"Embedding"},
			Pricing:       &pb.ModelPricing{PerTokens: "$0.008/M Tokens"},
			ContextLength: "8k Context",
			Description:   "Nomic Embed Text v1.5 is a lightweight, high-performance text embedding model.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Sep 1, 2025", Kind: "Base model", ProviderName: "Nomic AI", HuggingFace: "nomic-ai/nomic-embed-text-v1.5"},
			Specification: &pb.ModelSpecification{Parameters: "137M"},
			Tags:          []string{"Serverless"},
		},
	}
}

func (s *MemoryStore) seedAPIKeys() {
	s.apiKeys = []*apiKeyEntry{
		{
			key: &pb.APIKey{
				Id:        "key_5VFKZKA2qxmqU5aJ",
				Name:      "FW_API_KEY",
				SecretKey: "fw_WSo2...",
				CreatedAt: "Jan 19, 2026 7:34 AM",
			},
			secret: "fw_WSo2xxxxxxxxxxxxxxxxxx",
		},
	}
}

func (s *MemoryStore) seedSecrets() {
	s.secrets = []*secretEntry{
		{
			secret: &pb.Secret{Id: "1", Name: "GENERAL_SECRET_KEY"},
			value:  "secret-value",
		},
	}
}

func (s *MemoryStore) seedQuotas() {
	s.quotas = []*pb.Quota{
		{Id: "1", Name: "Deployed Model Count", QuotaId: "deployed-model-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 100},
		{Id: "2", Name: "Eval Protocol Free Daily Credits", QuotaId: "eval-protocol-free-daily-credits", CurrentUsage: 0, UsagePercentage: 0, Quota: 0},
		{Id: "3", Name: "GLOBAL - A100 Count", QuotaId: "global--a100-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 16},
		{Id: "4", Name: "GLOBAL - B200 Count", QuotaId: "global--b200-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 16},
		{Id: "5", Name: "GLOBAL - H100 Count", QuotaId: "global--h100-count", CurrentUsage: 1, UsagePercentage: 6.25, Quota: 16},
		{Id: "6", Name: "GLOBAL - H200 Count", QuotaId: "global--h200-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 16},
		{Id: "7", Name: "Job Submission Count", QuotaId: "job-submission-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 8},
	}
}
