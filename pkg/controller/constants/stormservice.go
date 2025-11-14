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

package constants

const (
	GodelPodGroupNameAnnotationKey   = "godel.bytedance.com/pod-group-name"
	CoschedulingPodGroupNameLabelKey = "scheduling.x-k8s.io/pod-group"
	VolcanoPodGroupNameAnnotationKey = "scheduling.volcano.sh/group-name"

	RoleSetNameLabelKey          = "roleset-name"
	StormServiceNameLabelKey     = "storm-service-name"
	StormServiceRevisionLabelKey = "storm-service-revision"
	RoleNameLabelKey             = "role-name"
	RoleTemplateHashLabelKey     = "role-template-hash"
	RoleReplicaIndexLabelKey     = "stormservice.orchestration.aibrix.ai/role-replica-index"
	PodSetNameLabelKey           = "stormservice.orchestration.aibrix.ai/podset-name"
	PodGroupIndexLabelKey        = "stormservice.orchestration.aibrix.ai/pod-group-index"
	// RoleRevisionLabelKey tracks the ControllerRevision number for a specific role (on pods)
	RoleRevisionLabelKey = "stormservice.orchestration.aibrix.ai/role-revision"
	// RoleRevisionNameLabelKey tracks the ControllerRevision name for a specific role (on pods)
	RoleRevisionNameLabelKey = "stormservice.orchestration.aibrix.ai/role-revision-name"

	RoleSetIndexAnnotationKey    = "stormservice.orchestration.aibrix.ai/roleset-index"
	RoleSetRevisionAnnotationKey = "stormservice.orchestration.aibrix.ai/revision"
	// RoleReplicaIndexAnnotationKey is originally used, to support label filter rank 0 pod, we add label support but keep this annotation for backward compatibility.
	RoleReplicaIndexAnnotationKey = "stormservice.orchestration.aibrix.ai/role-replica-index"
	// RoleRevisionAnnotationPrefix is used to store per-role revision info in RoleSet annotations
	// Example: "stormservice.orchestration.aibrix.ai/role-revision.decode" = "3"
	RoleRevisionAnnotationPrefix     = "stormservice.orchestration.aibrix.ai/role-revision"
	RoleRevisionNameAnnotationPrefix = "stormservice.orchestration.aibrix.ai/role-revision-name"

	StormServiceNameEnvKey = "STORM_SERVICE_NAME"
	RoleSetNameEnvKey      = "ROLESET_NAME"
	RoleSetIndexEnvKey     = "ROLESET_INDEX"
	RoleNameEnvKey         = "ROLE_NAME"
	RoleReplicaIndexEnvKey = "ROLE_REPLICA_INDEX"
	RoleTemplateHashEnvKey = "ROLE_TEMPLATE_HASH"
	PodSetNameEnvKey       = "PODSET_NAME"
	PodSetIndexEnvKey      = "POD_GROUP_INDEX"
	PodSetSizeEnvKey       = "POD_GROUP_SIZE"
)
