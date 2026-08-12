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

package catalog

import (
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// Factory constructs a Catalog for a provider. Providers self-register their
// factory from init() (see resource_manager/provider/*), so the dependency
// points provider -> catalog only, never the reverse.
type Factory func() (Catalog, error)

var registry = map[types.ResourceProvisionType]Factory{}

// Register adds a provider catalog factory. Intended to be called from a
// provider package's init(); last writer wins.
func Register(provider types.ResourceProvisionType, f Factory) {
	registry[provider] = f
}

func lookup(provider types.ResourceProvisionType) (Factory, bool) {
	f, ok := registry[provider]
	return f, ok
}
