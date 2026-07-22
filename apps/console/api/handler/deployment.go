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
	"strings"
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/deployment/provider"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	deploymentRollbackTimeout = 30 * time.Second
)

type DeploymentHandler struct {
	pb.UnimplementedDeploymentServiceServer
	store     store.Store
	providers *provider.Registry
}

func NewDeploymentHandler(s store.Store, registries ...*provider.Registry) *DeploymentHandler {
	var registry *provider.Registry
	if len(registries) > 0 {
		registry = registries[0]
	}
	return &DeploymentHandler{store: s, providers: registry}
}

func (h *DeploymentHandler) ListDeployments(ctx context.Context, req *pb.ListDeploymentsRequest) (*pb.ListDeploymentsResponse, error) {
	deployments, err := h.store.ListDeployments(ctx, req.Search)
	if err != nil {
		return nil, err
	}
	return &pb.ListDeploymentsResponse{Deployments: deployments}, nil
}

func (h *DeploymentHandler) GetDeployment(ctx context.Context, req *pb.GetDeploymentRequest) (*pb.Deployment, error) {
	deployment, err := h.store.GetDeployment(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return h.refreshDeploymentStatus(ctx, deployment)
}

func (h *DeploymentHandler) CreateDeployment(ctx context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	if req.GetTemplate() != nil {
		if req.GetTemplate().GetModelId() == "" || req.GetTemplate().GetTemplateId() == "" {
			return nil, status.Error(codes.InvalidArgument, "template.model_id and template.template_id are required")
		}
		if h.providers == nil {
			return nil, status.Error(codes.FailedPrecondition, "deployment implementations are not configured")
		}
		template, err := h.store.GetModelDeploymentTemplate(ctx, req.GetTemplate().GetModelId(), req.GetTemplate().GetTemplateId())
		if err != nil {
			return nil, err
		}
		if template.GetStatus() != "active" {
			return nil, status.Errorf(codes.FailedPrecondition, "deployment template %q is not active", template.GetName())
		}
		model, err := h.store.GetModel(ctx, template.GetModelId())
		if err != nil {
			return nil, err
		}
		if req.GetImplementation().GetProfile() != "" {
			return nil, status.Error(codes.InvalidArgument, "deployment implementation profiles are not supported")
		}
		providerImpl, err := h.providers.Get(req.GetImplementation().GetKind())
		if err != nil {
			return nil, err
		}
		if validateErr := providerImpl.Validate(ctx, template, req); validateErr != nil {
			return nil, validateErr
		}
		deployment, err := providerImpl.Create(ctx, template, req)
		if err != nil {
			return nil, err
		}
		deployment.BaseModel = model.GetName()
		deployment.BaseModelId = model.GetId()
		deployment.CreatedBy = currentUserEmail(ctx)
		saved, err := h.store.SaveDeployment(ctx, deployment)
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), deploymentRollbackTimeout)
			cleanupErr := providerImpl.Delete(cleanupCtx, deployment)
			cancel()
			if cleanupErr != nil {
				code := status.Code(err)
				if code == codes.OK {
					code = codes.Internal
				}
				return nil, status.Errorf(code, "%v; rollback failed: %v", err, cleanupErr)
			}
			return nil, err
		}
		return saved, nil
	}
	if req.GetImplementation() != nil || req.GetOverrides() != nil {
		return nil, status.Error(codes.InvalidArgument, "template is required when implementation or overrides are set")
	}
	return h.store.CreateDeployment(ctx, req)
}

func (h *DeploymentHandler) DeleteDeployment(ctx context.Context, req *pb.DeleteDeploymentRequest) (*emptypb.Empty, error) {
	deployment, err := h.store.GetDeployment(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if err := requireDeploymentOwner(ctx, deployment, "delete this deployment"); err != nil {
		return nil, err
	}
	if deployment.GetImplementationKind() != "" {
		if h.providers == nil {
			return nil, status.Error(codes.FailedPrecondition, "deployment implementations are not configured")
		}
		providerImpl, err := h.providers.Get(deployment.GetImplementationKind())
		if err != nil {
			return nil, err
		}
		if err := providerImpl.Delete(ctx, deployment); err != nil {
			return nil, err
		}
	}
	if err := h.store.DeleteDeployment(ctx, req.Id); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func requireDeploymentOwner(ctx context.Context, deployment *pb.Deployment, action string) error {
	if deployment == nil || deployment.GetCreatedBy() == "" {
		return nil
	}
	viewer := currentUserEmail(ctx)
	if viewer == "" || !strings.EqualFold(viewer, deployment.GetCreatedBy()) {
		return status.Errorf(codes.PermissionDenied, "only the deployment owner can %s", action)
	}
	return nil
}

func (h *DeploymentHandler) refreshDeploymentStatus(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error) {
	if deployment == nil || deployment.GetImplementationKind() == "" {
		return deployment, nil
	}
	if h.providers == nil {
		return nil, status.Error(codes.FailedPrecondition, "deployment implementations are not configured")
	}
	providerImpl, err := h.providers.Get(deployment.GetImplementationKind())
	if err != nil {
		return nil, err
	}
	observed, err := providerImpl.Observe(ctx, deployment)
	if err != nil {
		return nil, err
	}
	refreshed := provider.ApplyObservedStatus(deployment, observed)
	if refreshed == nil || refreshed.GetId() == "" {
		return refreshed, nil
	}
	return h.store.UpdateDeploymentStatus(ctx, refreshed)
}
