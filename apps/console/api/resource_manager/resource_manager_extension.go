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

package resource_manager

// This file is the downstream overlay point for registering closed-source
// providers. Downstream replaces it with blank imports of provider
// packages whose init() self-registers them into the provisioner/catalog
// registries, e.g.:
//
//	import _ "github.com/vllm-project/aibrix/apps/console/api/resource_manager/provider/internal"
//
// Upstream it intentionally does nothing; built-in providers are linked from
// resource_manager.go.
