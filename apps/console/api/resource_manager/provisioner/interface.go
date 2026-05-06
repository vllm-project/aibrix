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

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// Provisioner is the main interface for resource provisioning.
// Implementations should support idempotent operations using IdempotencyKey.
type Provisioner interface {
	// Provision allocates resources according to the given specification.
	// It returns a ProvisionResult containing the provision ID and status.
	// The operation should be idempotent - calling with the same IdempotencyKey
	// should return the same result without creating duplicate resources.
	Provision(ctx context.Context, req *types.ResourceProvision) (*types.ProvisionResult, error)

	// Release deallocates resources for the given provision.
	Release(ctx context.Context, provisionID string) error

	// List retrieves provisions matching the given criteria.
	List(ctx context.Context, opts *types.ListOptions) ([]*types.ProvisionResult, error)

	// Type returns the resource provision type
	Type() types.ResourceProvisionType
}
