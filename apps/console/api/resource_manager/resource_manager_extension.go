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
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// Custom providers can override getResourceManagerExtensionArgs to pass provider-specific arguments.
// Do not modify this method.
func getResourceManagerExtensionArgs(provider types.ResourceProvisionType) ([]interface{}, error) {
	return []interface{}{}, nil
}
