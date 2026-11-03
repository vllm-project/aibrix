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

package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readyModelPod(name, model string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{constants.ModelLabelName: model},
		},
		Status: v1.PodStatus{
			PodIP:      "10.0.0.1",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func TestListModelsWithReadyPods(t *testing.T) {
	c := NewForTest()
	first := readyModelPod("first", "ready-model")
	second := readyModelPod("second", "ready-model")
	unready := readyModelPod("unready", "unready-model")
	unready.Status.Conditions[0].Status = v1.ConditionFalse
	draining := readyModelPod("draining", "draining-model")
	draining.Annotations = map[string]string{constants.PodDrainingAnnotationKey: "true"}
	noAddress := readyModelPod("no-address", "no-address-model")
	noAddress.Status.PodIP = ""
	terminating := readyModelPod("terminating", "terminating-model")
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	for _, pod := range []*v1.Pod{first, second, unready, draining, noAddress, terminating} {
		c.addPod(pod)
	}

	assert.ElementsMatch(t, []string{"ready-model"}, c.ListModelsWithReadyPods())
	assert.Len(t, c.ListModels(), 5, "known model listing keeps its existing behavior")

	firstNotReady := first.DeepCopy()
	firstNotReady.Status.Conditions[0].Status = v1.ConditionFalse
	c.updatePod(first, firstNotReady)
	assert.Equal(t, []string{"ready-model"}, c.ListModelsWithReadyPods(), "one ready replica keeps the name visible")

	secondNotReady := second.DeepCopy()
	secondNotReady.Status.Conditions[0].Status = v1.ConditionUnknown
	c.updatePod(second, secondNotReady)
	assert.Empty(t, c.ListModelsWithReadyPods(), "the last ready replica removes the name")

	c.updatePod(firstNotReady, first)
	assert.Equal(t, []string{"ready-model"}, c.ListModelsWithReadyPods())
	c.deletePod(first)
	assert.Empty(t, c.ListModelsWithReadyPods())
}

func TestDisabledModelClaimPodBindingsDoNotAddModels(t *testing.T) {
	c := NewForTest()
	c.disableModelClaimBindings = true

	claimOnly := warmModelClaimPod("claim-only", "default", map[string]int{"claim-model": 9001})
	c.addPod(claimOnly)
	assert.False(t, c.HasModel("claim-model"))
	assert.Empty(t, c.ListModelsWithReadyPods())

	base := readyModelPod("base", "base-model")
	base.Annotations = claimOnly.Annotations
	c.addPod(base)
	assert.Equal(t, []string{"base-model"}, c.ListModelsWithReadyPods())
	assert.False(t, c.HasModel("claim-model"))

	updated := base.DeepCopy()
	updated.Annotations[constants.ModelClaimPodAnnotationPrefix+"new"] = `{"model":"another-claim","port":9002}`
	c.updatePod(base, updated)
	assert.False(t, c.HasModel("another-claim"))
	c.deletePod(updated)
	assert.Empty(t, c.ListModels())
}

func TestListModelsWithReadyPodsIncludesAdapterOnReadyPod(t *testing.T) {
	c := NewForTest()
	pod := readyModelPod("base", "base-model")
	c.addPod(pod)
	c.addModelAdapter(&modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-model", Namespace: "default"},
		Status:     modelv1alpha1.ModelAdapterStatus{Instances: []string{pod.Name}},
	})

	assert.Equal(t, []string{"adapter-model", "base-model"}, c.ListModelsWithReadyPods())

	unready := pod.DeepCopy()
	unready.Status.Conditions[0].Status = v1.ConditionFalse
	c.updatePod(pod, unready)
	assert.Empty(t, c.ListModelsWithReadyPods())
}
