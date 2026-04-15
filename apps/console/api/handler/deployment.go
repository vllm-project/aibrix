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

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/store"
	"google.golang.org/protobuf/types/known/emptypb"
)

type DeploymentHandler struct {
	pb.UnimplementedDeploymentServiceServer
	store store.Store
}

func NewDeploymentHandler(s store.Store) *DeploymentHandler {
	return &DeploymentHandler{store: s}
}

func (h *DeploymentHandler) ListDeployments(ctx context.Context, req *pb.ListDeploymentsRequest) (*pb.ListDeploymentsResponse, error) {
	deployments, err := h.store.ListDeployments(ctx, req.Search)
	if err != nil {
		return nil, err
	}
	return &pb.ListDeploymentsResponse{Deployments: deployments}, nil
}

func (h *DeploymentHandler) GetDeployment(ctx context.Context, req *pb.GetDeploymentRequest) (*pb.Deployment, error) {
	return h.store.GetDeployment(ctx, req.Id)
}

func (h *DeploymentHandler) CreateDeployment(ctx context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	return h.store.CreateDeployment(ctx, req)
}

func (h *DeploymentHandler) DeleteDeployment(ctx context.Context, req *pb.DeleteDeploymentRequest) (*emptypb.Empty, error) {
	if err := h.store.DeleteDeployment(ctx, req.Id); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
