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

package pd

import v1 "k8s.io/api/core/v1"

// Pod labels the PD router and the encode stage group pods by.
const (
	RoleSetLabel       = "roleset-name"
	RoleLabel          = "role-name"
	PodGroupIndexLabel = "stormservice.orchestration.aibrix.ai/pod-group-index"
)

// IsPodWithHTTPServer checks if a pod should be selected for routing.
// In multi-node tensor parallelism setups (e.g., TP=16 with node_rank=0 and node_rank=1),
// only pods with stormservice.orchestration.aibrix.ai/pod-group-index="0" (corresponding to node_rank=0) run the HTTP server.
// Pods without the label are also selected for backward compatibility.
func IsPodWithHTTPServer(pod *v1.Pod) bool {
	podGroupIndex, exists := pod.Labels[PodGroupIndexLabel]
	if !exists {
		// No PodGroupIndex label means single-node or old setup - include it
		return true
	}
	// Only include pods from node_rank=0 which have the HTTP server
	return podGroupIndex == "0"
}
