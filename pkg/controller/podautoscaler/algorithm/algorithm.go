/*
Copyright 2024 The Aibrix Team.

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

package algorithm

import "github.com/aibrix/aibrix/pkg/controller/podautoscaler/common"

type ScalingAlgorithm interface {
	// ComputeTargetReplicas calculates the number of replicas needed based on current metrics
	// and the provided scaling specifications.
	//
	// Parameters:
	// currentPodCount - the current number of ready pods
	// context - an interface that provides access to scaling parameters like target values and tolerances
	//
	// Returns:
	// int32 - the calculated target number of replicas
	ComputeTargetReplicas(currentPodCount float64, context common.ScalingContext) int32
}
