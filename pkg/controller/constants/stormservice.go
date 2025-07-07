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
	GodelPodGroupNameAnnotationKey = "godel.bytedance.com/pod-group-name"

	RoleSetNameLabelKey          = "roleset-name"
	StormServiceNameLabelKey     = "storm-service-name"
	StormServiceRevisionLabelKey = "storm-service-revision"
	RoleNameLabelKey             = "role-name"
	RoleTemplateHashLabelKey     = "role-template-hash"

	RoleSetIndexAnnotationKey     = "stormservice.orchestration.aibrix.ai/roleset-index"
	RoleSetRevisionAnnotationKey  = "stormservice.orchestration.aibrix.ai/revision"
	RoleReplicaIndexAnnotationKey = "stormservice.orchestration.aibrix.ai/role-replica-index"

	StormServiceNameEnvKey = "STORM_SERVICE_NAME"
	RoleSetNameEnvKey      = "ROLESET_NAME"
	RoleSetIndexEnvKey     = "ROLESET_INDEX"
	RoleNameEnvKey         = "ROLE_NAME"
	RoleReplicaIndexEnvKey = "ROLE_REPLICA_INDEX"
)
