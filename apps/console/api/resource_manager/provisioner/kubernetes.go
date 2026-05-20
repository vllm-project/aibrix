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

package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/utils/lru"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/clientset"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

const defaultK8sClientsetCacheSize = 128

// K8sProvisioner implements provisioner.Provisioner for Kubernetes.
// It creates provision results without actually creating pods.
type K8sProvisioner struct {
	clientsetCache *lru.Cache
	store          store.Store
	mu             sync.RWMutex
}

// NewK8sProvisioner creates a new Kubernetes provisioner.
func NewK8sProvisioner(s store.Store) (Provisioner, error) {
	return &K8sProvisioner{
		clientsetCache: lru.New(defaultK8sClientsetCacheSize),
		store:          s,
	}, nil
}

// Type returns the provisioner type.
func (p *K8sProvisioner) Type() types.ResourceProvisionType {
	return types.ResourceProvisionTypeKubernetes
}

// Provision creates a provision result without actually creating pods.
func (p *K8sProvisioner) Provision(ctx context.Context, req *types.ResourceProvision) (*types.ProvisionResult, error) {
	if req == nil {
		return nil, types.ErrInvalidArgs
	}

	if req.Spec.Credential.Provider == "" {
		req.Spec.Credential.Provider = types.ResourceProvisionTypeKubernetes
	}
	if req.Spec.Credential.Provider != types.ResourceProvisionTypeKubernetes {
		return nil, types.ErrInvalidArgs
	}

	existing, err := p.store.GetProvisionByIdempotencyKey(ctx, req.IdempotencyKey)
	if err == nil && existing != nil {
		return existing, nil
	}
	if err != nil && status.Code(err) != codes.NotFound {
		return nil, fmt.Errorf("get provision by idempotency key: %w", err)
	}

	k8sClientset, err := p.getOrCreateClientset(&req.Spec.Credential)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes clientset: %w", err)
	}

	provisionID := uuid.New().String()

	var regionSpec types.RegionSpec
	if primary := k8sClientset.Primary(); primary != nil {
		regionSpec = primary.Region
	}
	regionBytes, err := json.Marshal(regionSpec)
	if err != nil {
		return nil, fmt.Errorf("marshal region spec: %w", err)
	}

	now := time.Now()
	result := &types.ProvisionResult{
		ProvisionID:    provisionID,
		IdempotencyKey: req.IdempotencyKey,
		Status:         types.ProvisionStatusRunning,
		Region:         string(regionBytes),
		CreatedAt:      now,
		UpdatedAt:      now,
		Kubernetes:     &types.KubernetesProvisionDetail{},
	}

	if err := p.store.UpsertProvision(ctx, result); err != nil {
		return nil, fmt.Errorf("upsert provision: %w", err)
	}

	return result, nil
}

func (p *K8sProvisioner) getOrCreateClientset(credential *types.ResourceCredential) (*types.KubernetesClientset, error) {
	cacheKey, normalizedCredential := normalizeK8sCredentialForCache(credential)

	p.mu.RLock()
	cached, ok := p.clientsetCache.Get(cacheKey)
	p.mu.RUnlock()
	if ok && cached != nil {
		if clientset, ok := cached.(*types.KubernetesClientset); ok && clientset != nil {
			return clientset, nil
		}
	}

	resourceClientset, err := clientset.NewClientset(normalizedCredential)
	if err != nil {
		return nil, err
	}
	if resourceClientset.Kubernetes == nil {
		return nil, types.ErrInvalidCredential
	}

	p.mu.Lock()
	if existing, ok := p.clientsetCache.Get(cacheKey); ok && existing != nil {
		if clientset, ok := existing.(*types.KubernetesClientset); ok && clientset != nil {
			p.mu.Unlock()
			return clientset, nil
		}
	}
	p.clientsetCache.Add(cacheKey, resourceClientset.Kubernetes)
	p.mu.Unlock()

	return resourceClientset.Kubernetes, nil
}

func normalizeK8sCredentialForCache(credential *types.ResourceCredential) (string, *types.ResourceCredential) {
	normalized := types.ResourceCredential{Provider: types.ResourceProvisionTypeKubernetes}
	if credential != nil {
		normalized = *credential
		if normalized.Provider == "" {
			normalized.Provider = types.ResourceProvisionTypeKubernetes
		}
	}
	if normalized.Kubernetes == nil {
		normalized.Kubernetes = &types.KubernetesCredential{}
	}

	cacheKey := "k8s||"
	if normalized.Kubernetes.Kubeconfig != nil {
		cacheKey += *normalized.Kubernetes.Kubeconfig
	}
	cacheKey += "|"
	if normalized.Kubernetes.Context != nil {
		cacheKey += *normalized.Kubernetes.Context
	}
	cacheKey += "|"
	if normalized.Kubernetes.Namespace != nil {
		cacheKey += *normalized.Kubernetes.Namespace
	}
	cacheKey += "|"
	if normalized.Kubernetes.ServiceAccountName != nil {
		cacheKey += *normalized.Kubernetes.ServiceAccountName
	}

	return cacheKey, &normalized
}

// Release marks the provision as released.
func (p *K8sProvisioner) Release(ctx context.Context, provisionID string) error {
	exists, err := p.store.ExistsProvision(ctx, provisionID)
	if err != nil {
		return fmt.Errorf("check provision exists: %w", err)
	}
	if !exists {
		return types.ErrProvisionNotFound
	}

	return p.store.UpdateProvisionStatus(ctx, provisionID, types.ProvisionStatusReleased)
}

// List retrieves provisions matching the given criteria.
func (p *K8sProvisioner) List(ctx context.Context, opts *types.ListOptions) ([]*types.ProvisionResult, error) {
	if opts == nil {
		opts = &types.ListOptions{}
	}

	return p.store.ListProvisions(ctx, opts)
}
