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
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/types"
)

// A status update of the only pod serving a model is a delete plus a re-add
// inside updatePod. It must keep the model's pending count and output predictor.
func TestUpdatePodKeepsModelOfOnlyReplica(t *testing.T) {
	const (
		modelName = "test-model"
		namespace = "default"
		podName   = "decode-0"
		requestID = "request-1"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	pod := requestTrackerTestPod(podName, namespace, modelName, podIP)
	cache.addPod(pod)

	routingCtx := types.NewRoutingContext(context.Background(), "least-request", modelName, "", requestID, "")
	routingCtx.SetTargetPod(pod)
	traceTerm := cache.AddRequestCount(routingCtx, requestID, modelName)

	before, ok := cache.metaModels.Load(modelName)
	assert.True(t, ok)
	assert.Equal(t, int32(1), atomic.LoadInt32(&before.pendingRequests))

	cache.updatePod(pod, requestTrackerTestPod(podName, namespace, modelName, podIP))

	after, ok := cache.metaModels.Load(modelName)
	assert.True(t, ok)
	assert.Same(t, before, after, "a status update must not rebuild the model")
	assert.Equal(t, 1, after.Pods.Len())

	cache.DoneRequestCount(routingCtx, requestID, modelName, traceTerm)
	assert.Equal(t, int32(0), atomic.LoadInt32(&after.pendingRequests),
		"the in-flight request must be released from the model it was counted on")
}

// An update that moves the only pod to another model still drops the old model.
func TestUpdatePodDropsModelItNoLongerServes(t *testing.T) {
	const (
		namespace = "default"
		podName   = "decode-0"
		podIP     = "10.0.0.1"
	)

	cache := NewForTest()
	oldPod := requestTrackerTestPod(podName, namespace, "model-a", podIP)
	cache.addPod(oldPod)

	cache.updatePod(oldPod, requestTrackerTestPod(podName, namespace, "model-b", podIP))

	_, ok := cache.metaModels.Load("model-a")
	assert.False(t, ok, "a model with no pod must be removed")
	meta, ok := cache.metaModels.Load("model-b")
	assert.True(t, ok)
	assert.Equal(t, 1, meta.Pods.Len())
}

// An adapter update that leaves its instance list unchanged swaps the mappings in
// place. It must keep the adapter model and its pending count.
func TestUpdateModelAdapterKeepsModelWithUnchangedInstances(t *testing.T) {
	cache := NewForTest()
	pod := requestTrackerTestPod("decode-0", "default", "base-model", "10.0.0.1")
	cache.addPod(pod)

	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-model", Namespace: "default"},
		Status:     modelv1alpha1.ModelAdapterStatus{Instances: []string{pod.Name}},
	}
	cache.addModelAdapter(adapter)
	before, ok := cache.metaModels.Load("adapter-model")
	assert.True(t, ok)
	atomic.StoreInt32(&before.pendingRequests, 3)

	cache.updateModelAdapter(adapter, adapter.DeepCopy())

	after, ok := cache.metaModels.Load("adapter-model")
	assert.True(t, ok)
	assert.Same(t, before, after, "an unchanged instance list must not rebuild the adapter model")
	assert.Equal(t, int32(3), atomic.LoadInt32(&after.pendingRequests))
	assert.Equal(t, 1, after.Pods.Len())
}

// An adapter update that removes its last instance still drops the adapter model.
func TestUpdateModelAdapterDropsModelWithoutInstances(t *testing.T) {
	cache := NewForTest()
	pod := requestTrackerTestPod("decode-0", "default", "base-model", "10.0.0.1")
	cache.addPod(pod)

	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-model", Namespace: "default"},
		Status:     modelv1alpha1.ModelAdapterStatus{Instances: []string{pod.Name}},
	}
	cache.addModelAdapter(adapter)
	_, ok := cache.metaModels.Load("adapter-model")
	assert.True(t, ok)

	updated := adapter.DeepCopy()
	updated.Status.Instances = nil
	cache.updateModelAdapter(adapter, updated)

	_, ok = cache.metaModels.Load("adapter-model")
	assert.False(t, ok, "a model with no instance must be removed")
}
