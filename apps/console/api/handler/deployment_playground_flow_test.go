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

package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/deployment/provider"
	deploymentstatus "github.com/vllm-project/aibrix/apps/console/api/deployment/status"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

func TestCreateDeploymentDetailThenPlaygroundChat(t *testing.T) {
	var forwardedModel string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("gateway path = %s", r.URL.Path)
		}
		var body chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode gateway request: %v", err)
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		forwardedModel = body.Model
		if !body.Stream {
			t.Error("playground proxy did not force streaming")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello from mock\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(gateway.Close)

	ctx := deploymentContext("owner@example.com")
	s := store.NewMemoryStore(nil)
	t.Cleanup(func() { _ = s.Close() })

	model, err := s.CreateModel(ctx, &pb.Model{Id: "model-1", Name: "Mock Model", ServingName: "/models/mock"})
	if err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}
	template, err := s.CreateModelDeploymentTemplate(ctx, &pb.CreateModelDeploymentTemplateRequest{
		Name:    "template-1",
		Version: "v1.0.0",
		Status:  "active",
		ModelId: model.GetId(),
		Spec: &pb.ModelDeploymentTemplateSpec{
			Engine:      &pb.EngineSpec{Type: "vllm", Image: "example/vllm:latest"},
			ModelSource: &pb.ModelSourceSpec{Uri: "org/model"},
			Accelerator: &pb.AcceleratorSpec{Type: "CPU", Count: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateModelDeploymentTemplate() error = %v", err)
	}

	implementation := &fakeDeploymentProvider{}
	deployments := NewDeploymentHandler(s, provider.NewRegistry(implementation))
	deployments.SetGatewayEndpoint(gateway.URL)

	created, err := deployments.CreateDeployment(ctx, &pb.CreateDeploymentRequest{
		Name: "mock-deployment",
		Template: &pb.DeploymentTemplateRef{
			ModelId:    model.GetId(),
			TemplateId: template.GetId(),
		},
		Implementation: &pb.DeploymentImplementationRef{Kind: implementation.Kind()},
	})
	if err != nil {
		t.Fatalf("CreateDeployment() error = %v", err)
	}
	if created.GetStatus() != deploymentstatus.StatusDeploying {
		t.Fatalf("created status = %q", created.GetStatus())
	}

	detail, err := deployments.GetDeployment(ctx, &pb.GetDeploymentRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("GetDeployment() error = %v", err)
	}
	if detail.GetStatus() != deploymentstatus.StatusReady {
		t.Fatalf("detail status = %q, want Ready", detail.GetStatus())
	}
	if detail.GetServingName() != model.GetServingName() {
		t.Fatalf("serving_name = %q", detail.GetServingName())
	}
	if detail.GetDeploymentId() == "" {
		t.Fatal("runtime resource name is empty")
	}
	if detail.GetInferenceUrl() != gateway.URL+"/v1/chat/completions" {
		t.Fatalf("inference_url = %q", detail.GetInferenceUrl())
	}
	if _, err := time.Parse(time.RFC3339, detail.GetCreatedAt()); err != nil {
		t.Fatalf("created_at = %q: %v", detail.GetCreatedAt(), err)
	}

	playground := NewPlaygroundHandler(gateway.URL)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"`+detail.GetServingName()+`","messages":[{"role":"user","content":"Hi"}]}`),
	)
	playground.HandleChatCompletion(recorder, request, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("playground status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Hello from mock") {
		t.Fatalf("playground body = %q, want streamed completion", recorder.Body.String())
	}
	if forwardedModel != detail.GetServingName() {
		t.Fatalf("forwarded model = %q, want %q", forwardedModel, detail.GetServingName())
	}
}
