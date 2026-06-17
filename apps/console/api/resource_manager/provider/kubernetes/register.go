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

package kubernetes

import (
	"github.com/vllm-project/aibrix/apps/console/api/error_injection"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

// init self-registers the Kubernetes provider. Unlike cloud providers
// (Lambda Cloud, RunPod) that require API keys from the environment,
// Kubernetes uses in-cluster config or kubeconfig, so registration
// always succeeds and errors surface only when the provisioner/catalog
// is actually constructed with missing or invalid credentials.
func init() {
	provisioner.Register(types.ResourceProvisionTypeKubernetes, func(s store.Store, injector error_injection.Injector) (provisioner.Provisioner, error) {
		return newProvisioner(s, injector)
	})
	catalog.Register(types.ResourceProvisionTypeKubernetes, func() (catalog.Catalog, error) {
		return newCatalog()
	})
}
