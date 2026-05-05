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
)

type ModelHandler struct {
	pb.UnimplementedModelServiceServer
	store store.Store
}

func NewModelHandler(s store.Store) *ModelHandler {
	return &ModelHandler{store: s}
}

func (h *ModelHandler) ListModels(ctx context.Context, req *pb.ListModelsRequest) (*pb.ListModelsResponse, error) {
	models, err := h.store.ListModels(ctx, req.Search, req.Category)
	if err != nil {
		return nil, err
	}
	return &pb.ListModelsResponse{Models: models}, nil
}

func (h *ModelHandler) GetModel(ctx context.Context, req *pb.GetModelRequest) (*pb.Model, error) {
	return h.store.GetModel(ctx, req.Id)
}
