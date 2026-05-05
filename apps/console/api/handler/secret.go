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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type SecretHandler struct {
	pb.UnimplementedSecretServiceServer
	store store.Store
}

func NewSecretHandler(s store.Store) *SecretHandler {
	return &SecretHandler{store: s}
}

func (h *SecretHandler) ListSecrets(ctx context.Context, req *pb.ListSecretsRequest) (*pb.ListSecretsResponse, error) {
	secrets, err := h.store.ListSecrets(ctx, req.Search)
	if err != nil {
		return nil, err
	}
	return &pb.ListSecretsResponse{Secrets: secrets}, nil
}

func (h *SecretHandler) CreateSecret(ctx context.Context, req *pb.CreateSecretRequest) (*pb.Secret, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	return h.store.CreateSecret(ctx, req.Name, req.Value)
}

func (h *SecretHandler) DeleteSecret(ctx context.Context, req *pb.DeleteSecretRequest) (*emptypb.Empty, error) {
	if err := h.store.DeleteSecret(ctx, req.Id); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
