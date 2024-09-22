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

package utils

import (
	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsRayClusterReady returns true if a RayCluster is ready; false otherwise.
func IsRayClusterReady(cluster *rayclusterv1.RayCluster) bool {
	return IsRayClusterStateReady(cluster.Status)
}

func IsRayClusterStateReady(status rayclusterv1.RayClusterStatus) bool {
	return status.State == rayclusterv1.Ready
}

func IsRayClusterAvailable(cluster *rayclusterv1.RayCluster, minReadySeconds int32, now metav1.Time) bool {
	// RayCluster doesn't have condition list, it's hard to know it's ready time.
	/*c := GetRayClusterReadyCondition(cluster.Status)
	minReadySecondsDuration := time.Duration(minReadySeconds) * time.Second
	if minReadySeconds == 0 || (!c.LastTransitionTime.IsZero() && c.LastTransitionTime.Add(minReadySecondsDuration).Before(now.Time)) {
		return true
	}*/

	// TODO: always return true at this moment.
	return IsRayClusterReady(cluster)
}
