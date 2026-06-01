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

package types

// Only used for extending the custom resource group options.
// This SHOULD be left empty; all group options should be placed in
// ResourceGroupSpec directly.
type ExtensionResourceGroupOptions struct {
}

// Only used for extending the custom provision result details.
// This SHOULD be left empty; all provision result details should be placed in
// ProvisionResult directly.
type ExtensionProvisionResultDetails struct {
}
