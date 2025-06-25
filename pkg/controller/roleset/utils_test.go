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

package roleset

import (
	"testing"

	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"

	"github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestGetReadyReplicaCountForRole(t *testing.T) {
	readyPod := makeReadyPod("ready1")
	notReadyPod := makeNotReadyPod("notready1")
	pods := []*corev1.Pod{readyPod, readyPod, notReadyPod}
	count := GetReadyReplicaCountForRole(pods)
	assert.Equal(t, int32(2), count)
}

func TestSetAndRemoveRoleSetCondition(t *testing.T) {
	status := &v1alpha1.RoleSetStatus{}
	now := metav1.Now()

	cond := v1alpha1.Condition{
		Type:               v1alpha1.RoleSetReady,
		Status:             corev1.ConditionTrue,
		Reason:             "AllReplicasReady",
		LastTransitionTime: &now,
	}

	SetRoleSetCondition(status, cond)
	assert.Len(t, status.Conditions, 1)

	// Duplicate condition (same status and reason) shouldn't be added
	SetRoleSetCondition(status, cond)
	assert.Len(t, status.Conditions, 1)

	// Remove condition
	RemoveRoleSetCondition(status, v1alpha1.RoleSetReady)
	assert.Len(t, status.Conditions, 0)
}

func TestMaxSurgeAndUnavailable(t *testing.T) {
	replicas := int32(10)
	surge := intstrutil.FromString("20%") // should be 2
	unavail := intstrutil.FromInt(3)      // 3

	role := &v1alpha1.RoleSpec{
		Replicas: &replicas,
		UpdateStrategy: v1alpha1.RoleUpdateStrategy{
			MaxSurge:       &surge,
			MaxUnavailable: &unavail,
		},
	}

	assert.Equal(t, int32(2), MaxSurge(role))
	assert.Equal(t, int32(3), MaxUnavailable(role))
}

func makeReadyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
		},
	}
}

func makeNotReadyPod(name string) *corev1.Pod {
	pod := makeReadyPod(name)
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	return pod
}
