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
	"context"
	"testing"

	"github.com/vllm-project/aibrix/apps/console/api/deployment/provider"
	deploymentstatus "github.com/vllm-project/aibrix/apps/console/api/deployment/status"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	"github.com/vllm-project/aibrix/apps/console/api/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeDeploymentProvider struct {
	observeCalls int
	deleteCalls  int
}

func (p *fakeDeploymentProvider) Kind() string { return "fake" }

func (p *fakeDeploymentProvider) Validate(context.Context, *pb.ModelDeploymentTemplate, *pb.CreateDeploymentRequest) error {
	return nil
}

func (p *fakeDeploymentProvider) Create(_ context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	return &pb.Deployment{
		Id:                 "deployment-provider-backed",
		Name:               req.GetName(),
		DeploymentId:       "runtime-resource",
		Replicas:           "1[1]",
		Status:             deploymentstatus.StatusDeploying,
		TemplateId:         template.GetId(),
		TemplateVersion:    template.GetVersion(),
		ImplementationKind: p.Kind(),
	}, nil
}

func (p *fakeDeploymentProvider) Observe(_ context.Context, deployment *pb.Deployment) (*provider.ObservedStatus, error) {
	p.observeCalls++
	return &provider.ObservedStatus{
		Status:   deploymentstatus.StatusReady,
		Replicas: deployment.GetReplicas(),
	}, nil
}

func (p *fakeDeploymentProvider) Update(_ context.Context, deployment *pb.Deployment) (*pb.Deployment, error) {
	return deployment, nil
}

func (p *fakeDeploymentProvider) Delete(context.Context, *pb.Deployment) error {
	p.deleteCalls++
	return nil
}

func deploymentContext(email string) context.Context {
	return metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(middleware.MetadataUserEmail, email),
	)
}

func TestDeploymentHandlerTemplateLifecycle(t *testing.T) {
	ctx := deploymentContext("owner@example.com")
	s := store.NewMemoryStore(nil)
	t.Cleanup(func() { _ = s.Close() })

	model, err := s.CreateModel(ctx, &pb.Model{Id: "model-1", Name: "Test Model"})
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
	handler := NewDeploymentHandler(s, provider.NewRegistry(implementation))
	created, err := handler.CreateDeployment(ctx, &pb.CreateDeploymentRequest{
		Name: "test-deployment",
		Template: &pb.DeploymentTemplateRef{
			ModelId:    model.GetId(),
			TemplateId: template.GetId(),
		},
		Implementation: &pb.DeploymentImplementationRef{Kind: implementation.Kind()},
	})
	if err != nil {
		t.Fatalf("CreateDeployment() error = %v", err)
	}
	if created.GetBaseModel() != model.GetName() || created.GetBaseModelId() != model.GetId() {
		t.Fatalf("model traceability = %q/%q", created.GetBaseModel(), created.GetBaseModelId())
	}
	if created.GetCreatedBy() != "owner@example.com" {
		t.Fatalf("created_by = %q", created.GetCreatedBy())
	}

	listed, err := handler.ListDeployments(ctx, &pb.ListDeploymentsRequest{})
	if err != nil {
		t.Fatalf("ListDeployments() error = %v", err)
	}
	if implementation.observeCalls != 0 {
		t.Fatalf("list unexpectedly called provider %d times", implementation.observeCalls)
	}
	if got := listed.GetDeployments()[0].GetStatus(); got != deploymentstatus.StatusDeploying {
		t.Fatalf("snapshot status = %q", got)
	}

	detail, err := handler.GetDeployment(ctx, &pb.GetDeploymentRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("GetDeployment() error = %v", err)
	}
	if detail.GetStatus() != deploymentstatus.StatusReady || implementation.observeCalls != 1 {
		t.Fatalf("detail status = %q, observe calls = %d", detail.GetStatus(), implementation.observeCalls)
	}

	_, err = handler.DeleteDeployment(
		deploymentContext("other@example.com"),
		&pb.DeleteDeploymentRequest{Id: created.GetId()},
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("DeleteDeployment() code = %v, want PermissionDenied", status.Code(err))
	}
	if implementation.deleteCalls != 0 {
		t.Fatalf("unauthorized delete reached provider")
	}

	if _, err := handler.DeleteDeployment(ctx, &pb.DeleteDeploymentRequest{Id: created.GetId()}); err != nil {
		t.Fatalf("owner DeleteDeployment() error = %v", err)
	}
	if implementation.deleteCalls != 1 {
		t.Fatalf("provider delete calls = %d", implementation.deleteCalls)
	}
}
