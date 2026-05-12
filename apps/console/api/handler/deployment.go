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

	"github.com/vllm-project/aibrix/apps/console/api/deployment/driver"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/store"
	"google.golang.org/protobuf/types/known/emptypb"
)

type DeploymentHandler struct {
	pb.UnimplementedDeploymentServiceServer
	store   store.Store
	drivers *driver.Registry
}

func NewDeploymentHandler(s store.Store, drivers *driver.Registry) *DeploymentHandler {
	return &DeploymentHandler{store: s, drivers: drivers}
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
	refreshed, err := h.readThroughProvider(ctx, deployment)
	if err != nil {
		return nil, err
	}
	if refreshed == nil || refreshed.GetId() == "" {
		return refreshed, nil
	}
	saved, err := h.store.SaveDeployment(ctx, refreshed)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

func (h *DeploymentHandler) CreateDeployment(ctx context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	if req.GetTemplate().GetTemplateId() != "" {
		template, err := h.store.GetModelDeploymentTemplate(ctx, req.GetTemplate().GetModelId(), req.GetTemplate().GetTemplateId())
		if err != nil {
			return nil, err
		}
		driverImpl, err := h.drivers.Get(req.GetImplementation().GetKind())
		if err != nil {
			return nil, err
		}
		if validateErr := driverImpl.Validate(ctx, template, req); validateErr != nil {
			return nil, validateErr
		}
		deployment, err := driverImpl.Create(ctx, template, req)
		if err != nil {
			return nil, err
		}
		return h.store.SaveDeployment(ctx, deployment)
	}
	return h.store.CreateDeployment(ctx, req)
}

func (h *DeploymentHandler) DeleteDeployment(ctx context.Context, req *pb.DeleteDeploymentRequest) (*emptypb.Empty, error) {
	deployment, err := h.store.GetDeployment(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if deployment.GetImplementationKind() != "" {
		driverImpl, err := h.drivers.Get(deployment.GetImplementationKind())
		if err != nil {
			return nil, err
		}
		if err := driverImpl.Delete(ctx, deployment); err != nil {
			return nil, err
		}
	}
	if err := h.store.DeleteDeployment(ctx, req.Id); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (h *DeploymentHandler) readThroughProvider(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error) {
	if deployment == nil || deployment.GetImplementationKind() == "" {
		return deployment, nil
	}
	driverImpl, err := h.drivers.Get(deployment.GetImplementationKind())
	if err != nil {
		return nil, err
	}
	return driverImpl.Get(ctx, deployment)
}
