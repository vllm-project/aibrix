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
	"sync"
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/deployment/provider"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	batchRefreshConcurrency   = 8
	deploymentRollbackTimeout = 30 * time.Second
)

type DeploymentHandler struct {
	pb.UnimplementedDeploymentServiceServer
	store     store.Store
	providers *provider.Registry
}

func NewDeploymentHandler(s store.Store, providers *provider.Registry) *DeploymentHandler {
	return &DeploymentHandler{store: s, providers: providers}
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

func (h *DeploymentHandler) RefreshDeploymentStatus(ctx context.Context, req *pb.RefreshDeploymentStatusRequest) (*pb.Deployment, error) {
	deployment, err := h.store.GetDeployment(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return h.refreshDeploymentStatus(ctx, deployment)
}

func (h *DeploymentHandler) BatchRefreshDeploymentStatuses(ctx context.Context, req *pb.BatchRefreshDeploymentStatusesRequest) (*pb.BatchRefreshDeploymentStatusesResponse, error) {
	ids := req.GetIds()
	deployments := make([]*pb.Deployment, len(ids))
	sem := make(chan struct{}, batchRefreshConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	setErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	for i, id := range ids {
		wg.Add(1)
		go func(index int, deploymentID string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				setErr(ctx.Err())
				return
			}

			deployment, err := h.store.GetDeployment(ctx, deploymentID)
			if err != nil {
				setErr(err)
				return
			}
			refreshed, err := h.refreshDeploymentStatus(ctx, deployment)
			if err != nil {
				setErr(err)
				return
			}
			deployments[index] = refreshed
		}(i, id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return &pb.BatchRefreshDeploymentStatusesResponse{Deployments: deployments}, nil
}

func (h *DeploymentHandler) CreateDeployment(ctx context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	if req.GetTemplate().GetTemplateId() != "" {
		template, err := h.store.GetModelDeploymentTemplate(ctx, req.GetTemplate().GetModelId(), req.GetTemplate().GetTemplateId())
		if err != nil {
			return nil, err
		}
		providerImpl, err := h.providers.Get(req.GetProvider().GetKind())
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
		saved, err := h.store.SaveDeployment(ctx, deployment)
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), deploymentRollbackTimeout)
			defer cancel()
			if cleanupErr := providerImpl.Delete(cleanupCtx, deployment); cleanupErr != nil {
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
	return h.store.CreateDeployment(ctx, req)
}

func (h *DeploymentHandler) DeleteDeployment(ctx context.Context, req *pb.DeleteDeploymentRequest) (*emptypb.Empty, error) {
	deployment, err := h.store.GetDeployment(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if deployment.GetProviderKind() != "" {
		providerImpl, err := h.providers.Get(deployment.GetProviderKind())
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

func (h *DeploymentHandler) refreshDeploymentStatus(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error) {
	if deployment == nil || deployment.GetProviderKind() == "" {
		return deployment, nil
	}
	providerImpl, err := h.providers.Get(deployment.GetProviderKind())
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
	return h.store.SaveDeployment(ctx, refreshed)
}
