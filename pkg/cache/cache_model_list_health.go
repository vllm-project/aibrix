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

package cache

import (
	"context"

	"github.com/vllm-project/aibrix/pkg/cache/discovery"
)

// ListModelsVerified returns the selected list only after required discovery
// sources match a recent Kubernetes snapshot. Static discovery has no
// continuing watch and uses its loaded configuration directly.
func (c *Store) ListModelsVerified(ctx context.Context, readyPods bool) ([]string, error) {
	list := c.ListModels
	if readyPods {
		list = c.ListModelsWithReadyPods
	}
	if c.modelListHealth == nil {
		return list(), nil
	}
	if err := c.modelListHealth.EnsureVerified(ctx); err != nil {
		return nil, err
	}
	models, ok := c.modelListHealth.ModelsIfHealthy(list)
	if !ok {
		return nil, discovery.ErrModelListDiscoveryUnavailable
	}
	return models, nil
}
