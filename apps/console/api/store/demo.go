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
	"context"
	"fmt"
	"time"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/store/models"
	"gorm.io/gorm/clause"
)

// onConflictSkip is used by demo seeders so re-running against an existing
// (e.g. file-backed sqlite) database is a no-op instead of a primary-key failure.
var onConflictSkip = clause.OnConflict{DoNothing: true}

type apiKeyEntry struct {
	key    *pb.APIKey
	secret string // full unmasked key
}

type secretEntry struct {
	secret *pb.Secret
	value  string
}

func (s *GORMStore) loadDemoData() error {
	if err := s.loadDemoDeployments(); err != nil {
		return err
	}
	if err := s.loadDemoJobs(); err != nil {
		return err
	}
	if err := s.loadDemoModels(); err != nil {
		return err
	}
	if err := s.loadDemoModelDeploymentTemplates(); err != nil {
		return err
	}
	if err := s.loadDemoAPIKeys(); err != nil {
		return err
	}
	if err := s.loadDemoSecrets(); err != nil {
		return err
	}
	if err := s.loadDemoQuotas(); err != nil {
		return err
	}
	return nil
}

// LoadDemoData populates the store with demo records. Safe to call once on a
// fresh store; calling on a store that already has data appends and may yield
// duplicate IDs depending on the demo seed.
func (s *GORMStore) LoadDemoData() error {
	return s.loadDemoData()
}

// ListDemoJobs returns the full pb.Job records (including OpenAI Batch state
// fields) currently stored, used by the JobHandler dev fallback when MDS is
// unreachable. Unlike the Store.ListJobs interface method which only surfaces
// the Console-owned overlay subset, this method returns whatever was seeded
// directly into s.
func (s *GORMStore) ListDemoJobs() []*pb.Job {
	var jobs []*models.Job
	if err := s.db.Order("created_at DESC").Find(&jobs).Error; err != nil {
		return nil
	}
	out := make([]*pb.Job, 0, len(jobs))
	for _, j := range jobs {
		pbJob, err := j.ToPB()
		if err != nil {
			continue
		}
		out = append(out, pbJob)
	}
	return out
}

// GetDemoJob looks up a single full pb.Job by id. Counterpart of ListDemoJobs.
func (s *GORMStore) GetDemoJob(id string) (*pb.Job, bool) {
	job, err := s.GetJob(context.Background(), id)
	return job, err == nil
}

func (s *GORMStore) loadDemoDeployments() error {
	deployments := []*pb.Deployment{
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
			CreatedBy:      "demo@aibrix.ai",
			Status:         "Ready",
		},
	}

	for _, pbDep := range deployments {
		var dep models.Deployment
		if err := dep.FromPB(pbDep); err != nil {
			return fmt.Errorf("convert demo deployment %s: %w", pbDep.Id, err)
		}
		if err := s.db.Clauses(onConflictSkip).Create(&dep).Error; err != nil {
			return fmt.Errorf("insert demo deployment %s: %w", dep.ID, err)
		}
	}
	return nil
}

func (s *GORMStore) loadDemoJobs() error {
	// Full OpenAI Batch records, including state fields normally owned by the
	// metadata service. The Store interface (UpsertJob/GetJob/ListJobs) only
	// surfaces the Console-owned subset; the JobHandler dev fallback uses
	// ListDemoJobs / GetDemoJob to retrieve these full records when MDS is
	// unavailable.
	now := time.Now().UTC().Unix()
	jobs := map[string]*pb.Job{
		"batch_demo_27a6ee2c": {
			Id:               "batch_demo_27a6ee2c",
			Object:           "batch",
			Endpoint:         "/v1/chat/completions",
			Model:            "Llama 3.3 70B Instruct",
			InputDataset:     "file_demo_in_001",
			OutputDataset:    "file_demo_out_001",
			CompletionWindow: "24h",
			Status:           "completed",
			CreatedAt:        now - 86400,
			InProgressAt:     now - 86400 + 60,
			FinalizingAt:     now - 3700,
			CompletedAt:      now - 3600,
			ExpiresAt:        now,
			RequestCounts:    &pb.JobRequestCounts{Total: 1000, Completed: 998, Failed: 2},
			Usage:            &pb.JobUsage{InputTokens: 250000, OutputTokens: 180000, TotalTokens: 430000},
			Name:             "gsm-8k-20260118",
			CreatedBy:        "demo@aibrix.ai",
		},
		"batch_demo_a0b13ef5": {
			Id:               "batch_demo_a0b13ef5",
			Object:           "batch",
			Endpoint:         "/v1/chat/completions",
			Model:            "GLM-5",
			InputDataset:     "file_demo_in_002",
			CompletionWindow: "24h",
			Status:           "in_progress",
			CreatedAt:        now - 1800,
			InProgressAt:     now - 1700,
			ExpiresAt:        now - 1800 + 86400,
			RequestCounts:    &pb.JobRequestCounts{Total: 500, Completed: 250, Failed: 0},
			Name:             "gsm-8k-20260118-v2",
			CreatedBy:        "demo@aibrix.ai",
		},
	}

	for _, pbJob := range jobs {
		var job models.Job
		if err := job.FromPB(pbJob); err != nil {
			return fmt.Errorf("convert demo job %s: %w", pbJob.Id, err)
		}
		if err := s.db.Clauses(onConflictSkip).Create(&job).Error; err != nil {
			return fmt.Errorf("insert demo job %s: %w", job.ID, err)
		}
	}
	return nil
}

func (s *GORMStore) loadDemoModels() error {
	dmodels := []*pb.Model{
		{
			Id: "model-minimax-m2.5", Name: "MiniMax-M2.5",
			IconBg: "bg-red-100", IconText: "M", IconTextColor: "text-red-600",
			Categories: []string{"LLM"}, IsNew: true,
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.30/M", CachedInput: "$0.03/M", Output: "$1.20/M"},
			ContextLength: "192k Context",
			Description:   "MiniMax-M2.5 is a powerful language model designed for complex reasoning and code generation tasks.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 10, 2026", ProviderName: "MiniMax", HuggingFace: "minimax/MiniMax-M2.5"},
			Specification: &pb.ModelSpecification{Parameters: "250B"},
			Tags:          []string{"Serverless"},
			ServingName:   "minimax/MiniMax-M2.5",
		},
		{
			Id: "model-glm-5", Name: "GLM-5",
			IconBg: "bg-green-100", IconText: "Z", IconTextColor: "text-green-700",
			Categories: []string{"LLM"}, IsNew: true,
			Pricing:       &pb.ModelPricing{UncachedInput: "$1.00/M", CachedInput: "$0.20/M", Output: "$3.20/M"},
			ContextLength: "198k Context",
			Description:   "GLM-5 is Z.ai's SOTA model targeting complex systems engineering and long-horizon agentic tasks.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 12, 2026", ProviderName: "Z.ai", HuggingFace: "zai-org/GLM-5"},
			Specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "700B"},
			Tags:          []string{"Serverless"},
			ServingName:   "zai-org/GLM-5",
		},
		{
			Id: "model-kimi-k2.5", Name: "Kimi K2.5",
			IconBg: "bg-gray-100", IconText: "K", IconTextColor: "text-gray-800",
			Categories: []string{"LLM", "Vision"}, IsNew: true,
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.60/M", CachedInput: "$0.10/M", Output: "$3.00/M"},
			ContextLength: "256k Context",
			Description:   "Kimi K2.5 is a multimodal language model from Moonshot AI that supports both text and vision inputs.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 8, 2026", ProviderName: "Moonshot AI", HuggingFace: "moonshot/kimi-k2.5"},
			Specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "400B"},
			Tags:          []string{"Serverless", "Tunable"},
			ServingName:   "moonshot/kimi-k2.5",
		},
		{
			Id: "model-deepseek-v3.2", Name: "Deepseek v3.2",
			IconBg: "bg-purple-100", IconText: "D", IconTextColor: "text-purple-700",
			Categories:    []string{"LLM"},
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.56/M"},
			ContextLength: "128k Context",
			Description:   "Deepseek v3.2 is the latest iteration of the DeepSeek language model series.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Jan 25, 2026", ProviderName: "DeepSeek", HuggingFace: "deepseek-ai/deepseek-v3.2"},
			Specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "671B"},
			Tags:          []string{"Serverless"},
			ServingName:   "deepseek-ai/deepseek-v3.2",
		},
		{
			Id: "model-llama-3.3-70b", Name: "Llama 3.3 70B Instruct",
			IconBg: "bg-blue-100", IconText: "L", IconTextColor: "text-blue-700",
			Categories:    []string{"LLM"},
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.20/M", Output: "$0.20/M"},
			ContextLength: "128k Context",
			Description:   "Meta's Llama 3.3 70B Instruct model is optimized for instruction following and multi-turn dialogue.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Dec 10, 2025", ProviderName: "Meta", HuggingFace: "meta-llama/Llama-3.3-70B-Instruct"},
			Specification: &pb.ModelSpecification{Parameters: "70B"},
			Tags:          []string{"Serverless", "Tunable", "Function Calling"},
			ServingName:   "meta-llama/Llama-3.3-70B-Instruct",
		},
		// Audio
		{
			Id: "model-whisper-v3-large", Name: "Whisper V3 Large",
			IconBg: "bg-gray-100", IconText: "W", IconTextColor: "text-gray-700",
			Categories:    []string{"Audio"},
			Pricing:       &pb.ModelPricing{PerMinute: "$0.0015/minute"},
			Description:   "OpenAI's Whisper V3 Large is a general-purpose speech recognition model.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Oct 20, 2025", ProviderName: "OpenAI", HuggingFace: "openai/whisper-large-v3"},
			Specification: &pb.ModelSpecification{Calibrated: true, Parameters: "1.5B"},
			Tags:          []string{"Serverless"},
			ServingName:   "openai/whisper-large-v3",
		},
		// Image
		{
			Id: "model-flux-kontext-pro", Name: "FLUX.1 Kontext Pro",
			IconBg: "bg-gray-200", IconText: "△", IconTextColor: "text-gray-700",
			Categories:    []string{"Image"},
			Pricing:       &pb.ModelPricing{PerImage: "$0.04/ea"},
			Description:   "FLUX.1 Kontext Pro is a professional-grade image generation model.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 1, 2026", ProviderName: "Black Forest Labs"},
			Specification: &pb.ModelSpecification{Parameters: "12B"},
			Tags:          []string{"Serverless"},
			ServingName:   "black-forest-labs/FLUX.1-Kontext-pro",
		},
		// Embedding
		{
			Id: "model-nomic-embed-text", Name: "Nomic Embed Text v1.5",
			IconBg: "bg-teal-100", IconText: "N", IconTextColor: "text-teal-700",
			Categories:    []string{"Embedding"},
			Pricing:       &pb.ModelPricing{UncachedInput: "$0.008/M"},
			ContextLength: "8k Context",
			Description:   "Nomic Embed Text v1.5 is a lightweight, high-performance text embedding model.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Sep 1, 2025", ProviderName: "Nomic AI", HuggingFace: "nomic-ai/nomic-embed-text-v1.5"},
			Specification: &pb.ModelSpecification{Parameters: "137M"},
			Tags:          []string{"Serverless"},
			ServingName:   "nomic-ai/nomic-embed-text-v1.5",
		},
		{
			// Test-only entry whose template + serving_name align with the
			// `mock-vllm` ModelDeploymentTemplate registered in the MDS
			// ConfigMap. Lets the console e2e demo submit a batch end-to-end
			// without a real engine.
			Id: "model-mock-vllm", Name: "Mock Engine (Test)",
			IconBg: "bg-gray-100", IconText: "M", IconTextColor: "text-gray-700",
			Categories:    []string{"LLM"},
			ContextLength: "—",
			Description:   "OpenAI-compatible echo engine for batch e2e testing. Pairs with the mock-vllm template registered in MDS.",
			Metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "—", ProviderName: "AIBrix"},
			Specification: &pb.ModelSpecification{Parameters: "—"},
			Tags:          []string{"Test"},
			ServingName:   "/models/mock",
		},
	}

	for _, pbModel := range dmodels {
		var model models.Model
		if err := model.FromPB(pbModel); err != nil {
			return fmt.Errorf("convert demo model %s: %w", pbModel.Id, err)
		}
		if err := s.db.Clauses(onConflictSkip).Create(&model).Error; err != nil {
			return fmt.Errorf("insert demo model %s: %w", model.ID, err)
		}
	}
	return nil
}

func (s *GORMStore) loadDemoModelDeploymentTemplates() error {
	now := "2026-04-26T00:00:00Z"

	// Only the mock-vllm template — name + spec mirror the entry registered
	// in the MDS ConfigMap (aibrix-model-deployment-templates) so a batch
	// submitted from console resolves end-to-end without bespoke setup.
	templates := []*pb.ModelDeploymentTemplate{
		{
			Id:        "tpl-mock-vllm",
			Name:      "mock-vllm",
			Version:   "v0.0.1",
			Status:    "active",
			ModelId:   "model-mock-vllm",
			CreatedAt: now,
			UpdatedAt: now,
			Spec: &pb.ModelDeploymentTemplateSpec{
				Engine: &pb.EngineSpec{
					Type:           "mock",
					Version:        "0.1.0",
					Image:          "aibrix/vllm-mock:nightly",
					Invocation:     "http_server",
					HealthEndpoint: "/ready",
				},
				ModelSource:        &pb.ModelSourceSpec{Type: "local", Uri: "/models/mock"},
				Accelerator:        &pb.AcceleratorSpec{Type: "CPU", Count: 1},
				Parallelism:        &pb.ParallelismSpec{Tp: 1},
				SupportedEndpoints: []string{"/v1/chat/completions", "/v1/embeddings"},
				DeploymentMode:     "dedicated",
			},
		},
	}

	for _, pbTpl := range templates {
		var tpl models.ModelDeploymentTemplate
		if err := tpl.FromPB(pbTpl); err != nil {
			return fmt.Errorf("convert demo template %s: %w", pbTpl.Id, err)
		}
		if err := s.db.Clauses(onConflictSkip).Create(&tpl).Error; err != nil {
			return fmt.Errorf("insert demo template %s: %w", tpl.ID, err)
		}
	}
	return nil
}

func (s *GORMStore) loadDemoAPIKeys() error {
	apiKeys := []*apiKeyEntry{
		{
			key: &pb.APIKey{
				Id:        "key_5VFKZKA2qxmqU5aJ",
				Name:      "API_KEY",
				SecretKey: "WSo2...",
				CreatedAt: "Jan 19, 2026 7:34 AM",
			},
			secret: "WSo2xxxxxxxxxxxxxxxxxx",
		},
	}
	for _, entry := range apiKeys {
		var key models.APIKey
		if err := key.FromPB(entry.key); err != nil {
			return fmt.Errorf("convert demo api key %s: %w", entry.key.Id, err)
		}
		key.KeyHash = entry.secret
		if err := s.db.Clauses(onConflictSkip).Create(&key).Error; err != nil {
			return fmt.Errorf("insert demo api key %s: %w", key.ID, err)
		}
	}
	return nil
}

func (s *GORMStore) loadDemoSecrets() error {
	secrets := []*secretEntry{
		{
			secret: &pb.Secret{Id: "1", Name: "GENERAL_SECRET_KEY"},
			value:  "secret-value",
		},
	}
	for _, entry := range secrets {
		var secretModel models.Secret
		if err := secretModel.FromPB(entry.secret); err != nil {
			return fmt.Errorf("convert demo secret %s: %w", entry.secret.Id, err)
		}
		secretModel.EncryptedValue = entry.value
		if err := s.db.Clauses(onConflictSkip).Create(&secretModel).Error; err != nil {
			return fmt.Errorf("insert demo secret %s: %w", secretModel.ID, err)
		}
	}
	return nil
}

func (s *GORMStore) loadDemoQuotas() error {
	quotas := []*pb.Quota{
		{Id: "1", Name: "Deployed Model Count", QuotaId: "deployed-model-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 100},
		{Id: "2", Name: "Eval Protocol Free Daily Credits", QuotaId: "eval-protocol-free-daily-credits", CurrentUsage: 0, UsagePercentage: 0, Quota: 0},
		{Id: "3", Name: "GLOBAL - A100 Count", QuotaId: "global--a100-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 16},
		{Id: "4", Name: "GLOBAL - B200 Count", QuotaId: "global--b200-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 16},
		{Id: "5", Name: "GLOBAL - H100 Count", QuotaId: "global--h100-count", CurrentUsage: 1, UsagePercentage: 6.25, Quota: 16},
		{Id: "6", Name: "GLOBAL - H200 Count", QuotaId: "global--h200-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 16},
		{Id: "7", Name: "Job Submission Count", QuotaId: "job-submission-count", CurrentUsage: 0, UsagePercentage: 0, Quota: 8},
	}
	for _, pbQuota := range quotas {
		var quota models.Quota
		if err := quota.FromPB(pbQuota); err != nil {
			return fmt.Errorf("convert demo quota %s: %w", pbQuota.Id, err)
		}
		if err := s.db.Clauses(onConflictSkip).Create(&quota).Error; err != nil {
			return fmt.Errorf("insert demo quota %s: %w", quota.QuotaID, err)
		}
	}
	return nil
}
