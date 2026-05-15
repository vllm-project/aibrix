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

package resource_manager

import (
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

type ResourceManager struct {
	Provider    types.ResourceProvisionType
	Provisioner provisioner.Provisioner
	Catalog     catalog.Catalog
	Store       store.Store
}

func NewResourceManager(provider types.ResourceProvisionType, store store.Store) (*ResourceManager, error) {
	provisioner, err := provisioner.NewProvisioner(provider, store)
	if err != nil {
		return nil, err
	}

	catalog, err := catalog.NewCatalog(provider)
	if err != nil {
		return nil, err
	}

	return &ResourceManager{
		Provider:    provider,
		Provisioner: provisioner,
		Catalog:     catalog,
		Store:       store,
	}, nil
}
