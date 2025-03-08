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

locals {
  # AIBrix dependency YAML from http retrieval
  aibrix_dependency_yaml = { for index, resource in provider::kubernetes::manifest_decode_multi(data.http.aibrix_depenency.response_body) : "${resource.kind}/${resource.metadata.name}" => yamlencode(resource) }

  # AIBrix core YAML from http retrieval
  aibrix_core_yaml = { for index, resource in provider::kubernetes::manifest_decode_multi(data.http.aibrix_core.response_body) : "${resource.kind}/${resource.metadata.name}" => yamlencode(resource) }
}
