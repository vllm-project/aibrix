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
	"errors"
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

func TestPreserveServedModel_OverlappingAdaptersAndBaseModel(t *testing.T) {
	st := NewForTest()

	p1Ns1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "ns1",
			Labels: map[string]string{
				constants.ModelLabelName: "shared-model",
			},
		},
	}
	p2Ns2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p2",
			Namespace: "ns2",
			Labels: map[string]string{
				constants.ModelLabelName: "host-base-2",
			},
		},
	}
	st.addPod(p1Ns1)
	st.addPod(p2Ns2)

	// Adapter in ns1 with same name as pod's base model "shared-model" on p1.
	adapterNs1 := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-model",
			Namespace: "ns1",
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			BaseModel: ptr.To("base-ns1"),
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: []string{"p1"},
		},
	}
	// Adapter in ns2 with same model name "shared-model" on p2.
	adapterNs2 := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-model",
			Namespace: "ns2",
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			BaseModel: ptr.To("base-ns2"),
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: []string{"p2"},
		},
	}
	st.addModelAdapter(adapterNs2)
	st.addModelAdapter(adapterNs1)

	modelMeta, ok := st.metaModels.Load("shared-model")
	require.True(t, ok)
	// Deterministic resolution by sorted namespace/name ("ns1/shared-model" before "ns2/shared-model").
	assert.Equal(t, "base-ns1", modelMeta.BaseModel)

	// Deleting adapterNs1 must NOT drop ns1/p1 from shared-model because p1Ns1 directly serves "shared-model" via pod label,
	// and BaseModel must deterministically fall back to adapterNs2 ("base-ns2").
	st.deleteModelAdapter(adapterNs1)
	_, hasNs1P1 := modelMeta.Pods.Load("ns1/p1")
	_, hasNs2P2 := modelMeta.Pods.Load("ns2/p2")
	assert.True(t, hasNs1P1, "pod ns1/p1 still serves shared-model directly via pod label")
	assert.True(t, hasNs2P2, "pod ns2/p2 still serves shared-model via adapterNs2")
	assert.Equal(t, "base-ns2", modelMeta.BaseModel, "BaseModel must resolve to remaining active adapter")

	// Deleting p2Ns2 removes the last active edge of adapterNs2, clearing BaseModel while retaining
	// adapterRecord so a recreated p2Ns2 pod automatically re-links to adapterNs2.
	st.deletePod(p2Ns2)
	assert.Empty(t, st.adapterRecords["ns2/shared-model"].edges, "active edges must be cleared when pod is deleted")
	assert.Equal(t, "", modelMeta.BaseModel)

	// Re-adding p2Ns2 re-links it to adapterNs2 and restores BaseModel.
	st.addPod(p2Ns2)
	assert.Equal(t, "base-ns2", modelMeta.BaseModel)

	// Deleting adapterNs2 removes its adapterRecord and clears BaseModel.
	st.deleteModelAdapter(adapterNs2)
	assert.Empty(t, st.adapterRecords, "adapterRecords must be cleaned up when adapter is deleted")
	assert.Equal(t, "", modelMeta.BaseModel)
}

func TestPreserveServedModel_RouterConstructionRetry(t *testing.T) {
	attempts := 0
	st := InitWithModelRouterProvider(NewForTest(), func(modelName string) (types.QueueRouter, error) {
		if modelName != "m-retry" && modelName != "lora-retry" {
			return &stubQueueRouter{id: 99}, nil
		}
		attempts++
		if attempts <= 2 {
			return nil, errors.New("transient router init failure")
		}
		return &stubQueueRouter{id: attempts}, nil
	})

	podV1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "p1",
			Namespace:       "default",
			ResourceVersion: "1",
			Labels: map[string]string{
				constants.ModelLabelName: "m-retry",
			},
		},
	}
	podP2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "p2",
			Namespace:       "default",
			ResourceVersion: "1",
			Labels: map[string]string{
				constants.ModelLabelName: "base-other",
			},
		},
	}
	st.addPod(podV1)
	st.addPod(podP2)

	m, ok := st.metaModels.Load("m-retry")
	require.True(t, ok)
	assert.Nil(t, m.QueueRouter, "first attempt failed so QueueRouter should be nil")
	assert.Equal(t, 1, attempts)

	// Multi-pod ModelAdapter event with 2 instances must attempt router construction only once per event.
	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lora-retry",
			Namespace: "default",
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: []string{"p1", "p2"},
		},
	}
	st.addModelAdapter(adapter)
	assert.Equal(t, 2, attempts, "multi-pod addModelAdapter must attempt router construction only once per event")

	// Next membership event (updatePod on p1) retries router initialization once for m-retry and once for lora-retry.
	podV2 := podV1.DeepCopy()
	podV2.ResourceVersion = "2"
	st.updatePod(podV1, podV2)

	mAfter, ok := st.metaModels.Load("m-retry")
	require.True(t, ok)
	assert.Same(t, m, mAfter)
	require.NotNil(t, mAfter.QueueRouter, "subsequent membership event must retry and populate QueueRouter")
	loraAfter, ok := st.metaModels.Load("lora-retry")
	require.True(t, ok)
	require.NotNil(t, loraAfter.QueueRouter, "subsequent membership event must also retry and populate adapter QueueRouter")
	assert.Equal(t, 4, attempts)

	// A third membership event must keep the non-nil QueueRouter without calling modelRouterProvider again.
	podV3 := podV2.DeepCopy()
	podV3.ResourceVersion = "3"
	st.updatePod(podV2, podV3)
	assert.Equal(t, 4, attempts, "once QueueRouter is non-nil, modelRouterProvider must not be called again")
}
