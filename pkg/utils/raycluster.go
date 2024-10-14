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
	"time"

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsRayClusterReady returns true if a RayCluster is ready; false otherwise.
func IsRayClusterReady(cluster *rayclusterv1.RayCluster) bool {
	return IsRayClusterStateReady(cluster.Status)
}

func IsRayClusterStateReady(status rayclusterv1.RayClusterStatus) bool {
	// IsRayClusterProvisioned indicates whether all Ray Pods are ready for the first time.
	IsRayClusterProvisioned := meta.IsStatusConditionPresentAndEqual(status.Conditions, string(rayclusterv1.RayClusterProvisioned), metav1.ConditionTrue)
	isHeadReady := meta.IsStatusConditionPresentAndEqual(status.Conditions, string(rayclusterv1.HeadPodReady), metav1.ConditionTrue)
	isAllWorkersReady := status.ReadyWorkerReplicas == status.DesiredWorkerReplicas
	return IsRayClusterProvisioned && isHeadReady && isAllWorkersReady
}

func IsRayClusterAvailable(cluster *rayclusterv1.RayCluster, minReadySeconds int32, now metav1.Time) bool {
	if !IsRayClusterReady(cluster) {
		return false
	}

	headPodReadyCond := meta.FindStatusCondition(cluster.Status.Conditions, string(rayclusterv1.HeadPodReady))
	provisionedCond := meta.FindStatusCondition(cluster.Status.Conditions, string(rayclusterv1.RayClusterProvisioned))

	lastTransitionTime := provisionedCond.LastTransitionTime
	if headPodReadyCond.LastTransitionTime.Time.After(provisionedCond.LastTransitionTime.Time) {
		lastTransitionTime = headPodReadyCond.LastTransitionTime
	}

	minReadySecondsDuration := time.Duration(minReadySeconds) * time.Second
	if minReadySeconds == 0 || (!lastTransitionTime.IsZero() && lastTransitionTime.Add(minReadySecondsDuration).Before(now.Time)) {
		return true
	}

	return false
}
