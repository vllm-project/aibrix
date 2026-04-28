/*
Copyright 2026 The Aibrix Team.

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

	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

type ModelDeploymentTemplateHandler struct {
	pb.UnimplementedModelDeploymentTemplateServiceServer
	store store.Store
}

func NewModelDeploymentTemplateHandler(s store.Store) *ModelDeploymentTemplateHandler {
	return &ModelDeploymentTemplateHandler{store: s}
}

func (h *ModelDeploymentTemplateHandler) ListModelDeploymentTemplates(ctx context.Context, req *pb.ListModelDeploymentTemplatesRequest) (*pb.ListModelDeploymentTemplatesResponse, error) {
	templates, err := h.store.ListModelDeploymentTemplates(ctx, req.GetModelId(), req.GetStatus(), req.GetName())
	if err != nil {
		return nil, err
	}
	return &pb.ListModelDeploymentTemplatesResponse{Templates: templates}, nil
}

func (h *ModelDeploymentTemplateHandler) ResolveModelDeploymentTemplate(ctx context.Context, req *pb.ResolveModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	return h.store.ResolveModelDeploymentTemplate(ctx, req.GetModelId(), req.GetName(), req.GetVersion())
}

func (h *ModelDeploymentTemplateHandler) GetModelDeploymentTemplate(ctx context.Context, req *pb.GetModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	return h.store.GetModelDeploymentTemplate(ctx, req.GetModelId(), req.GetId())
}

func (h *ModelDeploymentTemplateHandler) CreateModelDeploymentTemplate(ctx context.Context, req *pb.CreateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	return h.store.CreateModelDeploymentTemplate(ctx, req)
}

func (h *ModelDeploymentTemplateHandler) UpdateModelDeploymentTemplate(ctx context.Context, req *pb.UpdateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	return h.store.UpdateModelDeploymentTemplate(ctx, req)
}

func (h *ModelDeploymentTemplateHandler) DeleteModelDeploymentTemplate(ctx context.Context, req *pb.DeleteModelDeploymentTemplateRequest) (*emptypb.Empty, error) {
	if err := h.store.DeleteModelDeploymentTemplate(ctx, req.GetModelId(), req.GetId()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
