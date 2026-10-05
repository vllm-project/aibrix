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

package lambdacloud

import (
	"github.com/vllm-project/aibrix/apps/console/api/error_injection"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

// init self-registers the Lambda Cloud provider. The service-level account is
// read from the environment; a missing API key surfaces as an error when the
// provisioner/catalog is constructed (selecting Lambda without a key fails).
func init() {
	provisioner.Register(types.ResourceProvisionTypeLambdaCloud, func(s store.Store, injector error_injection.Injector) (provisioner.Provisioner, error) {
		return newProvisioner(s, ConfigFromEnv(), injector)
	})
	catalog.Register(types.ResourceProvisionTypeLambdaCloud, func() (catalog.Catalog, error) {
		return newCatalog(ConfigFromEnv())
	})
}
