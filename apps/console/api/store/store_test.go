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
	"encoding/json"
	"fmt"
	"testing"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testDeploymentName = "gsm-8k-20260118"

func ptrStatus(s types.ProvisionStatus) *types.ProvisionStatus {
	return &s
}

//nolint:gocyclo // Test function with many sub-tests
func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	t.Cleanup(func() { _ = s.Close() })

	// load demo data for testing purpose
	if err := s.loadDemoData(); err != nil {
		t.Fatalf("loadDemoData failed: %v", err)
	}

	t.Run("Deployments", func(t *testing.T) {
		t.Run("ListDeployments", func(t *testing.T) {
			deps, err := s.ListDeployments(ctx, "")
			if err != nil {
				t.Fatalf("ListDeployments failed: %v", err)
			}
			if len(deps) == 0 {
				t.Error("expected some deployments from demo data")
			}
			// Verify demo deployment exists
			found := false
			for _, d := range deps {
				if d.Id == "deploy-1" {
					found = true
					if d.Name != testDeploymentName {
						t.Errorf("expected deployment name %q, got %q", testDeploymentName, d.Name)
					}
					break
				}
			}
			if !found {
				t.Error("demo deployment 'deploy-1' not found")
			}
		})

		t.Run("ListDeploymentsWithSearch", func(t *testing.T) {
			deps, err := s.ListDeployments(ctx, "gsm-8k")
			if err != nil {
				t.Fatalf("ListDeployments with search failed: %v", err)
			}
			if len(deps) == 0 {
				t.Error("expected to find deployment with search 'gsm-8k'")
			}
		})

		t.Run("GetDeployment", func(t *testing.T) {
			dep, err := s.GetDeployment(ctx, "deploy-1")
			if err != nil {
				t.Fatalf("GetDeployment failed: %v", err)
			}
			if dep == nil {
				t.Fatal("expected deployment, got nil")
			}
			if dep.Name != testDeploymentName {
				t.Errorf("expected name %q, got %q", testDeploymentName, dep.Name)
			}
			if dep.Status != "Ready" {
				t.Errorf("expected status 'Ready', got %q", dep.Status)
			}
		})

		t.Run("GetDeployment_NotFound", func(t *testing.T) {
			_, err := s.GetDeployment(ctx, "non-existent")
			if err == nil {
				t.Fatal("expected error for non-existent deployment")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})

		t.Run("CreateDeployment", func(t *testing.T) {
			req := &pb.CreateDeploymentRequest{
				Name:             "test-deployment",
				BaseModel:        "Test Model",
				MinReplicas:      1,
				MaxReplicas:      3,
				AcceleratorType:  "NVIDIA A100",
				AcceleratorCount: 2,
				Region:           "US West",
			}
			dep, err := s.CreateDeployment(ctx, req)
			if err != nil {
				t.Fatalf("CreateDeployment failed: %v", err)
			}
			if dep.Id == "" {
				t.Error("expected deployment ID to be set")
			}
			if dep.Name != req.Name {
				t.Errorf("expected name %q, got %q", req.Name, dep.Name)
			}
			if dep.Status != "Deploying" {
				t.Errorf("expected status 'Deploying', got %q", dep.Status)
			}

			// Verify it can be retrieved
			retrieved, err := s.GetDeployment(ctx, dep.Id)
			if err != nil {
				t.Fatalf("GetDeployment after create failed: %v", err)
			}
			if retrieved.Id != dep.Id {
				t.Error("retrieved deployment ID mismatch")
			}
		})

		t.Run("DeleteDeployment", func(t *testing.T) {
			// Create a deployment to delete
			req := &pb.CreateDeploymentRequest{
				Name:             "to-delete",
				BaseModel:        "Test Model",
				MinReplicas:      1,
				MaxReplicas:      1,
				AcceleratorType:  "NVIDIA T4",
				AcceleratorCount: 1,
				Region:           "US East",
			}
			dep, err := s.CreateDeployment(ctx, req)
			if err != nil {
				t.Fatalf("CreateDeployment for delete test failed: %v", err)
			}

			// Delete it
			err = s.DeleteDeployment(ctx, dep.Id)
			if err != nil {
				t.Fatalf("DeleteDeployment failed: %v", err)
			}

			// Verify it's gone
			_, err = s.GetDeployment(ctx, dep.Id)
			if err == nil {
				t.Error("expected error after deletion")
			}
		})

		t.Run("DeleteDeployment_NotFound", func(t *testing.T) {
			err := s.DeleteDeployment(ctx, "non-existent")
			if err == nil {
				t.Fatal("expected error for non-existent deployment")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})
	})

	t.Run("Jobs", func(t *testing.T) {
		t.Run("UpsertJob", func(t *testing.T) {
			job := &pb.Job{
				Id:        "test-job-1",
				Name:      "Test Job",
				CreatedBy: "test@aibrix.ai",
			}
			err := s.UpsertJob(ctx, job)
			if err != nil {
				t.Fatalf("UpsertJob failed: %v", err)
			}

			// Verify it can be retrieved
			retrieved, err := s.GetJob(ctx, job.Id)
			if err != nil {
				t.Fatalf("GetJob after upsert failed: %v", err)
			}
			if retrieved == nil {
				t.Fatal("expected job, got nil")
			}
			if retrieved.Name != job.Name {
				t.Errorf("expected name %q, got %q", job.Name, retrieved.Name)
			}
		})

		t.Run("UpsertJob_Update", func(t *testing.T) {
			job := &pb.Job{
				Id:        "test-job-update",
				Name:      "Original Name",
				CreatedBy: "test@aibrix.ai",
			}
			err := s.UpsertJob(ctx, job)
			if err != nil {
				t.Fatalf("UpsertJob (create) failed: %v", err)
			}

			// Update the job
			job.Name = "Updated Name"
			err = s.UpsertJob(ctx, job)
			if err != nil {
				t.Fatalf("UpsertJob (update) failed: %v", err)
			}

			retrieved, err := s.GetJob(ctx, job.Id)
			if err != nil {
				t.Fatalf("GetJob after update failed: %v", err)
			}
			if retrieved.Name != "Updated Name" {
				t.Errorf("expected updated name, got %q", retrieved.Name)
			}
		})

		t.Run("GetJob_NotFound", func(t *testing.T) {
			job, err := s.GetJob(ctx, "non-existent-job")
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if job != nil {
				t.Error("expected nil for non-existent job")
			}
		})

		t.Run("GetJob_DemoData", func(t *testing.T) {
			job, err := s.GetJob(ctx, "batch_demo_27a6ee2c")
			if err != nil {
				t.Fatalf("GetJob demo data failed: %v", err)
			}
			if job == nil {
				t.Fatal("expected demo job, got nil")
			}
			if job.Name != testDeploymentName {
				t.Errorf("expected demo job name, got %q", job.Name)
			}
		})

		t.Run("ListJobs", func(t *testing.T) {
			// Create test jobs
			for _, id := range []string{"list-job-1", "list-job-2"} {
				job := &pb.Job{Id: id, Name: "List Test", CreatedBy: "test@aibrix.ai"}
				if err := s.UpsertJob(ctx, job); err != nil {
					t.Fatalf("UpsertJob failed: %v", err)
				}
			}

			jobs, err := s.ListJobs(ctx, []string{"list-job-1", "list-job-2", "non-existent"})
			if err != nil {
				t.Fatalf("ListJobs failed: %v", err)
			}
			if len(jobs) != 2 {
				t.Errorf("expected 2 jobs, got %d", len(jobs))
			}
		})

		t.Run("ListJobs_Empty", func(t *testing.T) {
			jobs, err := s.ListJobs(ctx, []string{})
			if err != nil {
				t.Fatalf("ListJobs empty failed: %v", err)
			}
			if len(jobs) != 0 {
				t.Errorf("expected 0 jobs, got %d", len(jobs))
			}
		})

		t.Run("DeleteJob", func(t *testing.T) {
			job := &pb.Job{
				Id:        "job-to-delete",
				Name:      "To Delete",
				CreatedBy: "test@aibrix.ai",
			}
			err := s.UpsertJob(ctx, job)
			if err != nil {
				t.Fatalf("UpsertJob for delete test failed: %v", err)
			}

			err = s.DeleteJob(ctx, job.Id)
			if err != nil {
				t.Fatalf("DeleteJob failed: %v", err)
			}

			retrieved, err := s.GetJob(ctx, job.Id)
			if err != nil {
				t.Fatalf("GetJob after delete failed: %v", err)
			}
			if retrieved != nil {
				t.Error("expected nil after deletion")
			}
		})

		t.Run("UpsertJob_Invalid", func(t *testing.T) {
			err := s.UpsertJob(ctx, &pb.Job{Id: ""})
			if err == nil {
				t.Error("expected error for empty job ID")
			}

			err = s.UpsertJob(ctx, nil)
			if err == nil {
				t.Error("expected error for nil job")
			}
		})
	})

	t.Run("Models", func(t *testing.T) {
		t.Run("ListModels", func(t *testing.T) {
			models, err := s.ListModels(ctx, "", "")
			if err != nil {
				t.Fatalf("ListModels failed: %v", err)
			}
			if len(models) == 0 {
				t.Error("expected models from demo data")
			}

			// Check for specific demo models
			modelNames := make(map[string]bool)
			for _, m := range models {
				modelNames[m.Name] = true
			}
			expectedModels := []string{"Llama 3.3 70B Instruct", "Deepseek v3.2", "Kimi K2.5", "MiniMax-M2.5"}
			for _, name := range expectedModels {
				if !modelNames[name] {
					t.Errorf("expected demo model %q not found", name)
				}
			}
		})

		t.Run("ListModels_WithSearch", func(t *testing.T) {
			models, err := s.ListModels(ctx, "Llama", "")
			if err != nil {
				t.Fatalf("ListModels with search failed: %v", err)
			}
			if len(models) == 0 {
				t.Error("expected to find models with 'Llama' search")
			}
			for _, m := range models {
				if m.Name != "Llama 3.3 70B Instruct" {
					t.Errorf("unexpected model %q in search results", m.Name)
				}
			}
		})

		t.Run("ListModels_WithCategory", func(t *testing.T) {
			models, err := s.ListModels(ctx, "", "Audio")
			if err != nil {
				t.Fatalf("ListModels with category failed: %v", err)
			}
			if len(models) != 1 {
				t.Errorf("expected 1 audio model, got %d", len(models))
			}
			if len(models) > 0 && models[0].Name != "Whisper V3 Large" {
				t.Errorf("expected Whisper V3 Large, got %q", models[0].Name)
			}
		})

		t.Run("GetModel", func(t *testing.T) {
			model, err := s.GetModel(ctx, "model-llama-3.3-70b")
			if err != nil {
				t.Fatalf("GetModel failed: %v", err)
			}
			if model == nil {
				t.Fatal("expected model, got nil")
			}
			if model.Name != "Llama 3.3 70B Instruct" {
				t.Errorf("expected 'Llama 3.3 70B Instruct', got %q", model.Name)
			}
			if model.ContextLength != "128k Context" {
				t.Errorf("expected '128k Context', got %q", model.ContextLength)
			}
			if model.ServingName != "meta-llama/Llama-3.3-70B-Instruct" {
				t.Errorf("expected serving name 'meta-llama/Llama-3.3-70B-Instruct', got %q", model.ServingName)
			}
			// Check nested fields
			if model.Metadata == nil {
				t.Error("expected metadata to be populated")
			} else if model.Metadata.ProviderName != "Meta" {
				t.Errorf("expected provider 'Meta', got %q", model.Metadata.ProviderName)
			}
		})

		t.Run("GetModel_NotFound", func(t *testing.T) {
			_, err := s.GetModel(ctx, "non-existent-model")
			if err == nil {
				t.Fatal("expected error for non-existent model")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})
	})

	t.Run("ModelDeploymentTemplates", func(t *testing.T) {
		t.Run("ListModelDeploymentTemplates", func(t *testing.T) {
			tpls, err := s.ListModelDeploymentTemplates(ctx, "", "", "")
			if err != nil {
				t.Fatalf("ListModelDeploymentTemplates failed: %v", err)
			}
			if len(tpls) == 0 {
				t.Error("expected templates from demo data")
			}

			// Check for specific templates
			tplNames := make(map[string]bool)
			for _, tpl := range tpls {
				tplNames[tpl.Name] = true
			}
			expectedTpls := []string{"mock-vllm"}
			for _, name := range expectedTpls {
				if !tplNames[name] {
					t.Errorf("expected template %q not found", name)
				}
			}
		})

		t.Run("ListModelDeploymentTemplates_WithFilter", func(t *testing.T) {
			tpls, err := s.ListModelDeploymentTemplates(ctx, "model-mock-vllm", "", "")
			if err != nil {
				t.Fatalf("ListModelDeploymentTemplates with filter failed: %v", err)
			}
			if len(tpls) != 1 {
				t.Errorf("expected 1 mock template, got %d", len(tpls))
			}
		})

		t.Run("GetModelDeploymentTemplate", func(t *testing.T) {
			// First list to get an ID
			tpls, err := s.ListModelDeploymentTemplates(ctx, "model-mock-vllm", "", "mock-vllm")
			if err != nil {
				t.Fatalf("ListModelDeploymentTemplates for get test failed: %v", err)
			}
			if len(tpls) == 0 {
				t.Fatal("no templates found for get test")
			}

			tpl, err := s.GetModelDeploymentTemplate(ctx, "model-mock-vllm", tpls[0].Id)
			if err != nil {
				t.Fatalf("GetModelDeploymentTemplate failed: %v", err)
			}
			if tpl == nil {
				t.Fatal("expected template, got nil")
			}
			if tpl.Name != "mock-vllm" {
				t.Errorf("expected 'mock-vllm', got %q", tpl.Name)
			}
		})

		t.Run("GetModelDeploymentTemplate_NotFound", func(t *testing.T) {
			_, err := s.GetModelDeploymentTemplate(ctx, "model-llama-3.3-70b", "non-existent")
			if err == nil {
				t.Fatal("expected error for non-existent template")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})

		t.Run("CreateModelDeploymentTemplate", func(t *testing.T) {
			req := &pb.CreateModelDeploymentTemplateRequest{
				Name:    "test-template",
				ModelId: "model-llama-3.3-70b",
				Version: "v1.0.0",
				Status:  "active",
				Spec: &pb.ModelDeploymentTemplateSpec{
					Engine: &pb.EngineSpec{
						Type:       "vllm",
						Version:    "0.6.3",
						Image:      "vllm/vllm-openai:v0.6.3",
						Invocation: "http_server",
					},
					DeploymentMode: "dedicated",
				},
			}
			tpl, err := s.CreateModelDeploymentTemplate(ctx, req)
			if err != nil {
				t.Fatalf("CreateModelDeploymentTemplate failed: %v", err)
			}
			if tpl.Id == "" {
				t.Error("expected template ID to be set")
			}
			if tpl.Name != req.Name {
				t.Errorf("expected name %q, got %q", req.Name, tpl.Name)
			}
			if tpl.Spec == nil {
				t.Error("expected spec to be set")
			}
		})

		t.Run("CreateModelDeploymentTemplate_AlreadyExists", func(t *testing.T) {
			req := &pb.CreateModelDeploymentTemplateRequest{
				Name:    "mock-vllm",
				ModelId: "model-mock-vllm",
				Version: "v0.0.1",
				Spec: &pb.ModelDeploymentTemplateSpec{
					Engine:         &pb.EngineSpec{Type: "vllm"},
					DeploymentMode: "dedicated",
				},
			}
			_, err := s.CreateModelDeploymentTemplate(ctx, req)
			if err == nil {
				t.Fatal("expected error for duplicate template")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.AlreadyExists {
				t.Errorf("expected AlreadyExists error, got %v", err)
			}
		})

		t.Run("CreateModelDeploymentTemplate_Validation", func(t *testing.T) {
			// Missing name
			_, err := s.CreateModelDeploymentTemplate(ctx, &pb.CreateModelDeploymentTemplateRequest{
				ModelId: "model-1",
				Spec:    &pb.ModelDeploymentTemplateSpec{},
			})
			if err == nil {
				t.Error("expected error for missing name")
			}

			// Missing model_id
			_, err = s.CreateModelDeploymentTemplate(ctx, &pb.CreateModelDeploymentTemplateRequest{
				Name: "test",
				Spec: &pb.ModelDeploymentTemplateSpec{},
			})
			if err == nil {
				t.Error("expected error for missing model_id")
			}

			// Missing spec
			_, err = s.CreateModelDeploymentTemplate(ctx, &pb.CreateModelDeploymentTemplateRequest{
				Name:    "test",
				ModelId: "model-1",
			})
			if err == nil {
				t.Error("expected error for missing spec")
			}
		})

		t.Run("UpdateModelDeploymentTemplate", func(t *testing.T) {
			// First create a template to update
			req := &pb.CreateModelDeploymentTemplateRequest{
				Name:    "template-to-update",
				ModelId: "model-llama-3.3-70b",
				Version: "v1.0.0",
				Spec: &pb.ModelDeploymentTemplateSpec{
					Engine:         &pb.EngineSpec{Type: "vllm"},
					DeploymentMode: "dedicated",
				},
			}
			tpl, err := s.CreateModelDeploymentTemplate(ctx, req)
			if err != nil {
				t.Fatalf("CreateModelDeploymentTemplate for update test failed: %v", err)
			}

			updateReq := &pb.UpdateModelDeploymentTemplateRequest{
				Id:      tpl.Id,
				ModelId: "model-llama-3.3-70b",
				Name:    "updated-template-name",
				Status:  "inactive",
			}
			updated, err := s.UpdateModelDeploymentTemplate(ctx, updateReq)
			if err != nil {
				t.Fatalf("UpdateModelDeploymentTemplate failed: %v", err)
			}
			if updated.Name != "updated-template-name" {
				t.Errorf("expected updated name, got %q", updated.Name)
			}
			if updated.Status != "inactive" {
				t.Errorf("expected updated status, got %q", updated.Status)
			}
		})

		t.Run("UpdateModelDeploymentTemplate_NotFound", func(t *testing.T) {
			req := &pb.UpdateModelDeploymentTemplateRequest{
				Id:      "non-existent",
				ModelId: "model-llama-3.3-70b",
			}
			_, err := s.UpdateModelDeploymentTemplate(ctx, req)
			if err == nil {
				t.Fatal("expected error for non-existent template")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})

		t.Run("DeleteModelDeploymentTemplate", func(t *testing.T) {
			// Create a template to delete
			req := &pb.CreateModelDeploymentTemplateRequest{
				Name:    "template-to-delete",
				ModelId: "model-llama-3.3-70b",
				Version: "v1.0.0",
				Spec: &pb.ModelDeploymentTemplateSpec{
					Engine:         &pb.EngineSpec{Type: "vllm"},
					DeploymentMode: "dedicated",
				},
			}
			tpl, err := s.CreateModelDeploymentTemplate(ctx, req)
			if err != nil {
				t.Fatalf("CreateModelDeploymentTemplate for delete test failed: %v", err)
			}

			err = s.DeleteModelDeploymentTemplate(ctx, "model-llama-3.3-70b", tpl.Id)
			if err != nil {
				t.Fatalf("DeleteModelDeploymentTemplate failed: %v", err)
			}

			// Verify it's gone
			_, err = s.GetModelDeploymentTemplate(ctx, "model-llama-3.3-70b", tpl.Id)
			if err == nil {
				t.Error("expected error after deletion")
			}
		})

		t.Run("DeleteModelDeploymentTemplate_NotFound", func(t *testing.T) {
			err := s.DeleteModelDeploymentTemplate(ctx, "model-llama-3.3-70b", "non-existent")
			if err == nil {
				t.Fatal("expected error for non-existent template")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})

		t.Run("ResolveModelDeploymentTemplate", func(t *testing.T) {
			tpl, err := s.ResolveModelDeploymentTemplate(ctx, "model-mock-vllm", "mock-vllm", "v0.0.1")
			if err != nil {
				t.Fatalf("ResolveModelDeploymentTemplate failed: %v", err)
			}
			if tpl == nil {
				t.Fatal("expected template, got nil")
			}
			if tpl.Version != "v0.0.1" {
				t.Errorf("expected version v0.0.1, got %q", tpl.Version)
			}
		})

		t.Run("ResolveModelDeploymentTemplate_Latest", func(t *testing.T) {
			tpl, err := s.ResolveModelDeploymentTemplate(ctx, "model-mock-vllm", "mock-vllm", "")
			if err != nil {
				t.Fatalf("ResolveModelDeploymentTemplate (latest) failed: %v", err)
			}
			if tpl == nil {
				t.Fatal("expected template, got nil")
			}
			if tpl.Status != "active" {
				t.Errorf("expected active template, got status %q", tpl.Status)
			}
		})

		t.Run("ResolveModelDeploymentTemplate_NotFound", func(t *testing.T) {
			_, err := s.ResolveModelDeploymentTemplate(ctx, "model-mock-vllm", "non-existent", "")
			if err == nil {
				t.Fatal("expected error for non-existent template")
			}
		})
	})

	t.Run("APIKeys", func(t *testing.T) {
		t.Run("ListAPIKeys", func(t *testing.T) {
			keys, err := s.ListAPIKeys(ctx)
			if err != nil {
				t.Fatalf("ListAPIKeys failed: %v", err)
			}
			if len(keys) == 0 {
				t.Error("expected API keys from demo data")
			}
		})

		t.Run("CreateAPIKey", func(t *testing.T) {
			key, fullSecret, err := s.CreateAPIKey(ctx, "Test API Key")
			if err != nil {
				t.Fatalf("CreateAPIKey failed: %v", err)
			}
			if key.Id == "" {
				t.Error("expected key ID to be set")
			}
			if key.Name != "Test API Key" {
				t.Errorf("expected name 'Test API Key', got %q", key.Name)
			}
			if fullSecret == "" {
				t.Error("expected full secret to be returned")
			}
			if key.SecretKey == "" {
				t.Error("expected masked secret key to be set")
			}

			// Verify it appears in list
			keys, _ := s.ListAPIKeys(ctx)
			found := false
			for _, k := range keys {
				if k.Id == key.Id {
					found = true
					break
				}
			}
			if !found {
				t.Error("new key not found in list")
			}
		})

		t.Run("DeleteAPIKey", func(t *testing.T) {
			key, _, err := s.CreateAPIKey(ctx, "Key To Delete")
			if err != nil {
				t.Fatalf("CreateAPIKey for delete test failed: %v", err)
			}

			err = s.DeleteAPIKey(ctx, key.Id)
			if err != nil {
				t.Fatalf("DeleteAPIKey failed: %v", err)
			}

			// Verify it's gone
			keys, _ := s.ListAPIKeys(ctx)
			for _, k := range keys {
				if k.Id == key.Id {
					t.Error("deleted key still found in list")
					break
				}
			}
		})

		t.Run("DeleteAPIKey_NotFound", func(t *testing.T) {
			err := s.DeleteAPIKey(ctx, "non-existent-key")
			if err == nil {
				t.Fatal("expected error for non-existent key")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})
	})

	t.Run("Secrets", func(t *testing.T) {
		t.Run("ListSecrets", func(t *testing.T) {
			secrets, err := s.ListSecrets(ctx, "")
			if err != nil {
				t.Fatalf("ListSecrets failed: %v", err)
			}
			if len(secrets) == 0 {
				t.Error("expected secrets from demo data")
			}
		})

		t.Run("ListSecrets_WithSearch", func(t *testing.T) {
			secrets, err := s.ListSecrets(ctx, "GENERAL")
			if err != nil {
				t.Fatalf("ListSecrets with search failed: %v", err)
			}
			if len(secrets) != 1 {
				t.Errorf("expected 1 secret, got %d", len(secrets))
			}
		})

		t.Run("CreateSecret", func(t *testing.T) {
			secret, err := s.CreateSecret(ctx, "TEST_SECRET", "my-secret-value")
			if err != nil {
				t.Fatalf("CreateSecret failed: %v", err)
			}
			if secret.Id == "" {
				t.Error("expected secret ID to be set")
			}
			if secret.Name != "TEST_SECRET" {
				t.Errorf("expected name 'TEST_SECRET', got %q", secret.Name)
			}
		})

		t.Run("DeleteSecret", func(t *testing.T) {
			secret, err := s.CreateSecret(ctx, "SECRET_TO_DELETE", "value")
			if err != nil {
				t.Fatalf("CreateSecret for delete test failed: %v", err)
			}

			err = s.DeleteSecret(ctx, secret.Id)
			if err != nil {
				t.Fatalf("DeleteSecret failed: %v", err)
			}

			// Verify it's gone
			secrets, _ := s.ListSecrets(ctx, "")
			for _, sec := range secrets {
				if sec.Id == secret.Id {
					t.Error("deleted secret still found in list")
					break
				}
			}
		})

		t.Run("DeleteSecret_NotFound", func(t *testing.T) {
			err := s.DeleteSecret(ctx, "non-existent-secret")
			if err == nil {
				t.Fatal("expected error for non-existent secret")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})
	})

	t.Run("Quotas", func(t *testing.T) {
		t.Run("ListQuotas", func(t *testing.T) {
			quotas, err := s.ListQuotas(ctx, "")
			if err != nil {
				t.Fatalf("ListQuotas failed: %v", err)
			}
			if len(quotas) == 0 {
				t.Error("expected quotas from demo data")
			}
		})

		t.Run("ListQuotas_WithSearch", func(t *testing.T) {
			quotas, err := s.ListQuotas(ctx, "H100")
			if err != nil {
				t.Fatalf("ListQuotas with search failed: %v", err)
			}
			if len(quotas) != 1 {
				t.Errorf("expected 1 H100 quota, got %d", len(quotas))
			}
			if quotas[0].QuotaId != "global--h100-count" {
				t.Errorf("expected H100 quota, got %q", quotas[0].QuotaId)
			}
		})

		t.Run("QuotaValues", func(t *testing.T) {
			quotas, _ := s.ListQuotas(ctx, "")
			for _, q := range quotas {
				if q.Id == "" {
					t.Error("expected quota ID to be set")
				}
				if q.Name == "" {
					t.Error("expected quota name to be set")
				}
				if q.QuotaId == "" {
					t.Error("expected quota_id to be set")
				}
			}
		})
	})

	t.Run("Provision", func(t *testing.T) {
		t.Run("UpsertProvision_Insert", func(t *testing.T) {
			result := &types.ProvisionResult{
				IdempotencyKey: "idem-key-1",
				ProvisionID:    "prov-123",
				Status:         types.ProvisionStatusPending,
			}
			err := s.UpsertProvision(ctx, result)
			if err != nil {
				t.Fatalf("UpsertProvision failed: %v", err)
			}
		})

		t.Run("UpsertProvision_Update", func(t *testing.T) {
			// First insert
			result := &types.ProvisionResult{
				IdempotencyKey: "idem-key-upsert",
				ProvisionID:    "prov-upsert-1",
				Status:         types.ProvisionStatusPending,
			}
			err := s.UpsertProvision(ctx, result)
			if err != nil {
				t.Fatalf("UpsertProvision (insert) failed: %v", err)
			}

			// Retrieve to get the original CreatedAt
			original, err := s.GetProvision(ctx, "prov-upsert-1")
			if err != nil {
				t.Fatalf("GetProvision failed: %v", err)
			}

			// Update with same idempotency key but different provision_id and status
			updated := &types.ProvisionResult{
				IdempotencyKey: "idem-key-upsert",
				ProvisionID:    "prov-upsert-2",
				Status:         types.ProvisionStatusRunning,
			}
			err = s.UpsertProvision(ctx, updated)
			if err != nil {
				t.Fatalf("UpsertProvision (update) failed: %v", err)
			}

			// Retrieve the updated record
			retrieved, err := s.GetProvision(ctx, "prov-upsert-2")
			if err != nil {
				t.Fatalf("GetProvision after update failed: %v", err)
			}
			if retrieved.Status != types.ProvisionStatusRunning {
				t.Errorf("expected status 'running', got %q", retrieved.Status)
			}
			// Verify CreatedAt is preserved
			if !retrieved.CreatedAt.Equal(original.CreatedAt) {
				t.Errorf("CreatedAt should be preserved: original=%v, retrieved=%v", original.CreatedAt, retrieved.CreatedAt)
			}
		})

		t.Run("GetProvision", func(t *testing.T) {
			result := &types.ProvisionResult{
				IdempotencyKey: "idem-key-2",
				ProvisionID:    "prov-456",
				Status:         types.ProvisionStatusPending,
			}
			err := s.UpsertProvision(ctx, result)
			if err != nil {
				t.Fatalf("UpsertProvision for get test failed: %v", err)
			}

			retrieved, err := s.GetProvision(ctx, "prov-456")
			if err != nil {
				t.Fatalf("GetProvision failed: %v", err)
			}
			if retrieved == nil {
				t.Fatal("expected provision result, got nil")
			}
			if retrieved.ProvisionID != "prov-456" {
				t.Errorf("expected provision ID 'prov-456', got %q", retrieved.ProvisionID)
			}
		})

		t.Run("GetProvision_NotFound", func(t *testing.T) {
			_, err := s.GetProvision(ctx, "non-existent-provision-id")
			if err == nil {
				t.Fatal("expected error for non-existent provision")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.NotFound {
				t.Errorf("expected NotFound error, got %v", err)
			}
		})

		t.Run("ExistsProvision", func(t *testing.T) {
			result := &types.ProvisionResult{
				IdempotencyKey: "idem-key-3",
				ProvisionID:    "prov-789",
				Status:         types.ProvisionStatusPending,
			}
			if err := s.UpsertProvision(ctx, result); err != nil {
				t.Fatalf("UpsertProvision failed: %v", err)
			}

			exists, err := s.ExistsProvision(ctx, "prov-789")
			if err != nil {
				t.Fatalf("ExistsProvision failed: %v", err)
			}
			if !exists {
				t.Error("expected provision to exist")
			}

			exists, err = s.ExistsProvision(ctx, "non-existent-provision-id")
			if err != nil {
				t.Fatalf("ExistsProvision failed: %v", err)
			}
			if exists {
				t.Error("expected provision to not exist")
			}
		})

		t.Run("UpdateProvisionStatus", func(t *testing.T) {
			result := &types.ProvisionResult{
				IdempotencyKey: "idem-key-update",
				ProvisionID:    "prov-update",
				Status:         types.ProvisionStatusPending,
			}
			if err := s.UpsertProvision(ctx, result); err != nil {
				t.Fatalf("UpsertProvision failed: %v", err)
			}

			err := s.UpdateProvisionStatus(ctx, "prov-update", types.ProvisionStatusRunning)
			if err != nil {
				t.Fatalf("UpdateProvisionStatus failed: %v", err)
			}

			retrieved, err := s.GetProvision(ctx, "prov-update")
			if err != nil {
				t.Fatalf("GetProvision after update failed: %v", err)
			}
			if retrieved.Status != types.ProvisionStatusRunning {
				t.Errorf("expected status 'running', got %q", retrieved.Status)
			}
		})

		t.Run("DeleteProvision", func(t *testing.T) {
			result := &types.ProvisionResult{
				IdempotencyKey: "idem-key-delete",
				ProvisionID:    "prov-delete",
				Status:         types.ProvisionStatusPending,
			}
			if err := s.UpsertProvision(ctx, result); err != nil {
				t.Fatalf("UpsertProvision failed: %v", err)
			}

			err := s.DeleteProvision(ctx, "prov-delete")
			if err != nil {
				t.Fatalf("DeleteProvision failed: %v", err)
			}

			// Should not exist anymore
			exists, _ := s.ExistsProvision(ctx, "prov-delete")
			if exists {
				t.Error("expected provision to be deleted")
			}
		})

		t.Run("ListProvisions", func(t *testing.T) {
			// Insert multiple provisions
			for i := 1; i <= 3; i++ {
				result := &types.ProvisionResult{
					IdempotencyKey: fmt.Sprintf("idem-key-list-%d", i),
					ProvisionID:    fmt.Sprintf("prov-list-%d", i),
					Status:         types.ProvisionStatusPending,
				}
				if err := s.UpsertProvision(ctx, result); err != nil {
					t.Fatalf("UpsertProvision failed: %v", err)
				}
			}

			provisions, err := s.ListProvisions(ctx, &types.ListOptions{Limit: 10})
			if err != nil {
				t.Fatalf("ListProvisions failed: %v", err)
			}
			if len(provisions) < 3 {
				t.Errorf("expected at least 3 provisions, got %d", len(provisions))
			}
		})

		t.Run("ListProvisions_WithStatus", func(t *testing.T) {
			provisions, err := s.ListProvisions(ctx, &types.ListOptions{Status: ptrStatus(types.ProvisionStatusRunning), Limit: 10})
			if err != nil {
				t.Fatalf("ListProvisions with status failed: %v", err)
			}
			for _, p := range provisions {
				if p.Status != types.ProvisionStatusRunning {
					t.Errorf("expected running status, got %q", p.Status)
				}
			}
		})

		t.Run("ListProvisions_WithRegions", func(t *testing.T) {
			regionA := types.RegionSpec{Kubernetes: &types.KubernetesRegion{Context: "ctx-a", Namespace: "ns-a"}}
			regionB := types.RegionSpec{Kubernetes: &types.KubernetesRegion{Context: "ctx-b", Namespace: "ns-b"}}

			regionABytes, err := json.Marshal(regionA)
			if err != nil {
				t.Fatalf("marshal regionA failed: %v", err)
			}
			regionBBytes, err := json.Marshal(regionB)
			if err != nil {
				t.Fatalf("marshal regionB failed: %v", err)
			}

			resultA := &types.ProvisionResult{
				IdempotencyKey: "idem-key-region-a",
				ProvisionID:    "prov-region-a",
				Status:         types.ProvisionStatusPending,
				Region:         string(regionABytes),
			}
			if err := s.UpsertProvision(ctx, resultA); err != nil {
				t.Fatalf("UpsertProvision regionA failed: %v", err)
			}

			resultB := &types.ProvisionResult{
				IdempotencyKey: "idem-key-region-b",
				ProvisionID:    "prov-region-b",
				Status:         types.ProvisionStatusPending,
				Region:         string(regionBBytes),
			}
			if err := s.UpsertProvision(ctx, resultB); err != nil {
				t.Fatalf("UpsertProvision regionB failed: %v", err)
			}

			regions := []types.RegionSpec{regionA}
			provisions, err := s.ListProvisions(ctx, &types.ListOptions{Regions: &regions, Limit: 20})
			if err != nil {
				t.Fatalf("ListProvisions with regions failed: %v", err)
			}

			foundA := false
			for _, p := range provisions {
				if p.ProvisionID == "prov-region-a" {
					foundA = true
				}
				if p.ProvisionID == "prov-region-b" {
					t.Fatalf("unexpected provision from non-matching region: %s", p.ProvisionID)
				}
			}
			if !foundA {
				t.Fatalf("expected provision prov-region-a in filtered result")
			}
		})
	})

	t.Run("Close", func(t *testing.T) {
		// Close the main store instance and verify it doesn't error
		err := s.Close()
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	})
}
