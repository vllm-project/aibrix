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

package cache

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

type stubQueueRouter struct {
	id int
}

func (s *stubQueueRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	return "", nil
}

func (s *stubQueueRouter) Len() int {
	return s.id
}

func TestPreserveServedModel_OnSinglePodUpdate(t *testing.T) {
	routerCalls := 0
	st := InitWithModelRouterProvider(NewForTest(), func(modelName string) (types.QueueRouter, error) {
		routerCalls++
		return &stubQueueRouter{id: routerCalls}, nil
	})

	podV1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "p1",
			Namespace:       "default",
			ResourceVersion: "1",
			Labels: map[string]string{
				constants.ModelLabelName: "m1",
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: "10.0.0.1",
		},
	}
	st.addPod(podV1)

	beforeModel, ok := st.metaModels.Load("m1")
	require.True(t, ok, "model m1 must exist after addPod")
	atomic.StoreInt32(&beforeModel.pendingRequests, 7)
	beforePredictor := beforeModel.OutputPredictor
	beforeRouter := beforeModel.QueueRouter
	require.NotNil(t, beforePredictor)
	require.NotNil(t, beforeRouter)
	assert.Equal(t, 1, routerCalls)

	// In-place update of the single pod serving m1 (e.g. annotation/resourceVersion update).
	podV2 := podV1.DeepCopy()
	podV2.ResourceVersion = "2"
	podV2.Annotations = map[string]string{"updated": "true"}
	st.updatePod(podV1, podV2)

	afterModel, ok := st.metaModels.Load("m1")
	require.True(t, ok, "model m1 must still exist after updatePod")
	assert.Same(t, beforeModel, afterModel, "updatePod on single pod must preserve *Model instance")
	assert.Equal(t, int32(7), atomic.LoadInt32(&afterModel.pendingRequests), "pendingRequests must be preserved across updatePod")
	assert.Same(t, beforePredictor, afterModel.OutputPredictor, "OutputPredictor must be preserved across updatePod")
	assert.Same(t, beforeRouter, afterModel.QueueRouter, "QueueRouter must be preserved across updatePod")
	assert.Equal(t, 1, routerCalls, "modelRouterProvider must not be called again when model stays served")

	storedPod, ok := afterModel.Pods.Load("default/p1")
	require.True(t, ok, "pod default/p1 must remain registered on m1")
	assert.Equal(t, "2", storedPod.ResourceVersion, "metaModel.Pods must hold updated *v1.Pod pointer")
}

func TestPreserveServedModel_OnModelAdapterUpdate(t *testing.T) {
	routerCalls := 0
	st := InitWithModelRouterProvider(NewForTest(), func(modelName string) (types.QueueRouter, error) {
		routerCalls++
		return &stubQueueRouter{id: routerCalls}, nil
	})

	p1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: "base-m",
			},
		},
	}
	p2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p2",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: "base-m",
			},
		},
	}
	st.addPod(p1)
	st.addPod(p2)

	adapterV1 := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lora-1",
			Namespace: "default",
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			BaseModel: ptr.To("base-m"),
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: []string{"p1"},
		},
	}
	st.addModelAdapter(adapterV1)

	beforeModel, ok := st.metaModels.Load("lora-1")
	require.True(t, ok, "adapter model lora-1 must exist")
	atomic.StoreInt32(&beforeModel.pendingRequests, 5)
	beforePredictor := beforeModel.OutputPredictor
	beforeRouter := beforeModel.QueueRouter
	assert.Equal(t, "base-m", beforeModel.BaseModel)

	// Update adapter instances from [p1] to [p2] (rolling replacement) or [p1, p2].
	adapterV2 := adapterV1.DeepCopy()
	adapterV2.Status.Instances = []string{"p2"}
	st.updateModelAdapter(adapterV1, adapterV2)

	afterModel, ok := st.metaModels.Load("lora-1")
	require.True(t, ok, "adapter model lora-1 must remain in metaModels")
	assert.Same(t, beforeModel, afterModel, "updateModelAdapter must preserve *Model instance while model remains served")
	assert.Equal(t, int32(5), atomic.LoadInt32(&afterModel.pendingRequests), "pendingRequests must be preserved across updateModelAdapter")
	assert.Same(t, beforePredictor, afterModel.OutputPredictor, "OutputPredictor must be preserved across updateModelAdapter")
	assert.Same(t, beforeRouter, afterModel.QueueRouter, "QueueRouter must be preserved across updateModelAdapter")
	assert.Equal(t, "base-m", afterModel.BaseModel)
	_, hasP1 := afterModel.Pods.Load("default/p1")
	_, hasP2 := afterModel.Pods.Load("default/p2")
	assert.False(t, hasP1, "removed instance p1 must be unmapped")
	assert.True(t, hasP2, "new instance p2 must be mapped")
}

func TestPreserveServedModel_DropsEmptyModelWhenPodModelChanges(t *testing.T) {
	st := NewForTest()

	podV1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: "m1",
			},
		},
	}
	st.addPod(podV1)

	podV2 := podV1.DeepCopy()
	podV2.Labels[constants.ModelLabelName] = "m2"
	st.updatePod(podV1, podV2)

	_, hasM1 := st.metaModels.Load("m1")
	_, hasM2 := st.metaModels.Load("m2")
	assert.False(t, hasM1, "model m1 must be dropped when its last pod moves to m2")
	assert.True(t, hasM2, "model m2 must be registered")
}

func TestPreserveServedModel_DropsAdapterModelWhenInstancesRemoved(t *testing.T) {
	st := NewForTest()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "default",
			Labels: map[string]string{
				constants.ModelLabelName: "base-m",
			},
		},
	}
	st.addPod(pod)

	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lora-1",
			Namespace: "default",
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: []string{"p1"},
		},
	}
	st.addModelAdapter(adapter)
	_, ok := st.metaModels.Load("lora-1")
	require.True(t, ok)

	updated := adapter.DeepCopy()
	updated.Status.Instances = nil
	st.updateModelAdapter(adapter, updated)

	_, ok = st.metaModels.Load("lora-1")
	assert.False(t, ok, "adapter model must be dropped when all instances are removed")
}
