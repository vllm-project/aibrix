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

package podautoscaler

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	podutils "github.com/vllm-project/aibrix/pkg/utils"
)

// GetReadyPodsCount counts the number of ready pods matching the given selector
func GetReadyPodsCount(ctx context.Context, client client.Client, namespace string, selector labels.Selector) (int, error) {
	podList, err := podutils.GetPodListByLabelSelector(ctx, client, namespace, selector)
	if err != nil {
		return 0, fmt.Errorf("failed to list pods: %w", err)
	}

	readyCount := 0
	for _, pod := range podList.Items {
		if isPodReady(pod) {
			readyCount++
		}
	}
	return readyCount, nil
}

// isPodReady checks if a pod is ready
func isPodReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// replicaStateFromPods splits currentReplicas into ready and pending. When the
// replicas are RoleSets (scalesRoleSets), a RoleSet is ready once all of its
// selected pods are, so a RoleSet still starting counts as pending.
func replicaStateFromPods(pods []corev1.Pod, currentReplicas int32, roleSets bool) ReplicaState {
	var readyReplicas int32
	roleSetReady := map[string]bool{}
	for _, pod := range pods {
		if !isPodLive(pod) {
			continue
		}
		if roleSet := pod.Labels[constants.RoleSetNameLabelKey]; roleSets && roleSet != "" {
			ready, seen := roleSetReady[roleSet]
			roleSetReady[roleSet] = isPodReady(pod) && (ready || !seen)
			continue
		}
		if isPodReady(pod) {
			readyReplicas++
		}
	}
	for _, ready := range roleSetReady {
		if ready {
			readyReplicas++
		}
	}

	pendingReplicas := currentReplicas - readyReplicas
	if pendingReplicas < 0 {
		pendingReplicas = 0
	}

	return ReplicaState{
		ReadyReplicas:   currentReplicas - pendingReplicas,
		PendingReplicas: pendingReplicas,
	}
}

func isPodLive(pod corev1.Pod) bool {
	return pod.DeletionTimestamp == nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// scalesRoleSets reports whether the scale target's replicas are RoleSets: a
// StormService scaled through spec.replicas (any StormService without a
// subTargetSelector, and a replica-mode one with it).
func scalesRoleSets(pa *autoscalingv1alpha1.PodAutoscaler, scale *unstructured.Unstructured) (bool, error) {
	if scale.GetAPIVersion() != orchestrationv1alpha1.GroupVersion.String() || scale.GetKind() != StormService {
		return false, nil
	}
	if pa.Spec.SubTargetSelector == nil {
		return true, nil
	}
	ss := &orchestrationv1alpha1.StormService{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(scale.Object, ss); err != nil {
		return false, fmt.Errorf("failed to read StormService %s: %w", scale.GetName(), err)
	}
	return stormServiceScalingMode(pa, ss) == orchestrationv1alpha1.StormServiceReplicaMode, nil
}

// podsPerReplica is how many of the selected pods make up one replica of the
// scale target: 1, or the selected pods per RoleSet when the replicas are
// RoleSets, so that a per-pod metric is sized in RoleSets rather than pods.
func podsPerReplica(roleSets bool, pods []corev1.Pod) float64 {
	if !roleSets {
		return 1
	}
	selected := 0
	names := map[string]struct{}{}
	for _, pod := range pods {
		roleSet := pod.Labels[constants.RoleSetNameLabelKey]
		if roleSet == "" || !isPodLive(pod) {
			continue
		}
		selected++
		names[roleSet] = struct{}{}
	}
	if len(names) == 0 {
		return 1
	}
	return float64(selected) / float64(len(names))
}

func minFloat64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func boolToCond(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

type ValidationResult struct {
	Valid   bool
	Reason  string
	Message string
}

func invalid(reason, msg string) ValidationResult {
	return ValidationResult{Valid: false, Reason: reason, Message: msg}
}

func validOK() ValidationResult { return ValidationResult{Valid: true} }
