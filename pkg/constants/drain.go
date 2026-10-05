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

package constants

const (
	// PodDrainingAnnotationKey marks a pod that has entered controller-managed drain.
	// RoleSet and PodSet controllers write the string value "true" before deleting
	// a live serving pod. Gateway routing and controller deletion helpers read this
	// annotation; any value other than "true" is treated as not draining.
	PodDrainingAnnotationKey = "aibrix.ai/draining"

	// PodDrainStartTimeAnnotationKey stores the RFC3339 UTC time when drain began.
	// Controllers use it to decide whether the configured drain timeout has expired.
	PodDrainStartTimeAnnotationKey = "aibrix.ai/drain-start-time"

	// PodDrainReasonAnnotationKey records why the controller started drain.
	// It is diagnostic metadata only and is not used for routing decisions.
	PodDrainReasonAnnotationKey = "aibrix.ai/drain-reason"

	// PodDrainTargetActionAnnotationKey records the action the controller intends
	// after drain. The drain helper validates this for delete intent so stale or
	// foreign drain states do not cause immediate deletion.
	PodDrainTargetActionAnnotationKey = "aibrix.ai/drain-target-action"

	// PodDrainReasonScaleIn means the pod was selected because desired replicas shrank.
	PodDrainReasonScaleIn = "scale-in"

	// PodDrainReasonRollout means the pod was selected during rollout or replacement.
	PodDrainReasonRollout = "rollout"

	// PodDrainTargetActionDelete is the only drain target action supported by the
	// current RoleSet/PodSet delete path.
	PodDrainTargetActionDelete = "delete"
)
