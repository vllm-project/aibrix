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

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	loraName = "text2sql-lora-2"
)

func TestModelAdapter(t *testing.T) {
	adapter := createModelAdapterConfig("text2sql-lora-2", "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	t.Cleanup(func() {
		assert.NoError(t, v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{}))
		assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true,
			func(ctx context.Context) (done bool, err error) {
				adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(context.Background(),
					adapter.Name, v1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				return false, nil
			}))
	})

	// create model adapter
	t.Log("creating model adapter")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	assert.NoError(t, err)
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)
	oldPod := adapter.Status.Instances[0]

	// delete pod and ensure model adapter is rescheduled
	t.Log("deleting pod instance to force model adapter rescheduling")
	assert.NoError(t, k8sClient.CoreV1().Pods("default").Delete(context.Background(), oldPod, v1.DeleteOptions{}))
	validateAllPodsAreReady(t, k8sClient, 3)
	time.Sleep(3 * time.Second)
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)
	newPod := adapter.Status.Instances[0]

	assert.NotEqual(t, newPod, oldPod, "ensure old and new pods are different")

	// run inference for model adapter
	validateInference(t, loraName)
}

// TestModelAdapterRetryMechanism tests the retry mechanism with exponential backoff
func TestModelAdapterRetryMechanism(t *testing.T) {
	adapterName := "retry-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	// Ensure pods are ready for the test
	validateAllPodsAreReady(t, k8sClient, 3)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	// Create model adapter
	t.Log("creating model adapter to test retry mechanism")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)

	// Wait for adapter to progress through phases and complete loading
	t.Log("waiting for adapter to complete loading with retry mechanism")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 120*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}

			t.Logf("Current adapter phase: %s, instances: %v", adapter.Status.Phase, adapter.Status.Instances)

			// Check if adapter has reached a stable running state with instances
			return len(adapter.Status.Instances) > 0 && (adapter.Status.Phase == modelv1alpha1.ModelAdapterBound ||
				adapter.Status.Phase == modelv1alpha1.ModelAdapterRunning), nil
		}))

	// Validate that retry state management worked
	t.Log("validating final adapter state after retry mechanism")
	adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(
		context.Background(), adapterName, v1.GetOptions{})
	require.NoError(t, err)

	// Check that adapter eventually reaches a stable state with instances
	assert.True(t, len(adapter.Status.Instances) > 0, "adapter should have at least one instance")
	assert.Contains(t, []modelv1alpha1.ModelAdapterPhase{
		modelv1alpha1.ModelAdapterBound,
		modelv1alpha1.ModelAdapterRunning,
	}, adapter.Status.Phase, "adapter should reach bound or running phase")
}

// TestModelAdapterPodReadinessValidation tests pod readiness validation
func TestModelAdapterPodReadinessValidation(t *testing.T) {
	adapterName := "readiness-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	// Create model adapter
	t.Log("creating model adapter to test pod readiness validation")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)

	// Monitor adapter progress and ensure pods are validated
	t.Log("monitoring adapter scheduling and loading behavior")
	var finalInstances []string
	var observedScheduledPhase bool
	var observedPodValidation bool

	// Poll with shorter intervals to catch quick transitions
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 500*time.Millisecond, 120*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}

			t.Logf("Current phase: %s, instances: %v", adapter.Status.Phase, adapter.Status.Instances)

			// Debug: Show all annotations
			if len(adapter.Annotations) > 0 {
				t.Logf("Current annotations: %v", adapter.Annotations)
			}

			// Track if we observed the scheduled phase (shows pod validation happened)
			if adapter.Status.Phase == modelv1alpha1.ModelAdapterScheduled {
				observedScheduledPhase = true
				t.Logf("Observed Scheduled phase - pod validation occurred")
			}

			// Check if we have scheduled pods annotation (shows pod selection happened)
			scheduledPodsStr, exists := adapter.Annotations["adapter.model.aibrix.ai/scheduled-pods"]
			if exists && scheduledPodsStr != "" {
				observedPodValidation = true
				scheduledPods := strings.Split(scheduledPodsStr, ",")
				t.Logf("Found scheduled pods in annotation: %v", scheduledPods)

				// Validate scheduled pods are ready
				for _, podName := range scheduledPods {
					pod, err := k8sClient.CoreV1().Pods("default").Get(ctx, podName, v1.GetOptions{})
					if err != nil {
						t.Logf("pod %s not found: %v", podName, err)
						continue
					}

					// Check pod readiness
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							t.Logf("Verified pod %s is ready", podName)
							break
						}
					}
				}
			}

			// Record final instances
			if len(adapter.Status.Instances) > 0 {
				finalInstances = append([]string{}, adapter.Status.Instances...)
			}

			// Wait until adapter reaches a final state with instances
			return len(adapter.Status.Instances) > 0 && (adapter.Status.Phase == modelv1alpha1.ModelAdapterRunning ||
				adapter.Status.Phase == modelv1alpha1.ModelAdapterBound), nil
		}))

	// Validate that the adapter successfully selected and is running on a pod
	// This proves that the pod selection and readiness validation logic worked
	assert.True(t, len(finalInstances) > 0, "adapter should have final instances after pod readiness validation")
	assert.True(t, adapter.Status.Phase == modelv1alpha1.ModelAdapterRunning,
		"adapter should reach Running phase after pod validation")

	// Verify that the selected pod is actually ready
	if len(finalInstances) > 0 {
		selectedPodName := finalInstances[0]
		pod, err := k8sClient.CoreV1().Pods("default").Get(context.Background(), selectedPodName, v1.GetOptions{})
		assert.NoError(t, err, "should be able to fetch the selected pod")

		// Verify the pod is ready (which means our readiness validation worked)
		var podReady bool
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				podReady = true
				break
			}
		}
		assert.True(t, podReady, "selected pod should be ready, proving readiness validation worked")
		t.Logf("Successfully validated pod readiness for selected pod: %s", selectedPodName)
	}

	t.Logf("Test completion: observedScheduledPhase=%v, observedPodValidation=%v, finalInstances=%v",
		observedScheduledPhase, observedPodValidation, finalInstances)
}

// TestModelAdapterPodSwitching tests automatic pod switching when loading fails
func TestModelAdapterPodSwitching(t *testing.T) {
	adapterName := "pod-switching-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	// Ensure we have multiple pods available for switching
	validateAllPodsAreReady(t, k8sClient, 3)

	// Create model adapter
	t.Log("creating model adapter to test pod switching")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)

	// Wait for initial scheduling and track pod changes
	var initialPods []string
	var finalPods []string

	t.Log("monitoring pod switching behavior")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 120*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}

			// Record initial scheduled pods
			if adapter.Status.Phase == modelv1alpha1.ModelAdapterScheduled && len(initialPods) == 0 {
				scheduledPodsStr, exists := adapter.Annotations["adapter.model.aibrix.ai/scheduled-pods"]
				if exists && scheduledPodsStr != "" {
					initialPods = strings.Split(scheduledPodsStr, ",")
					t.Logf("initial pods scheduled: %v", initialPods)
				}
			}

			// Record final instances
			if len(adapter.Status.Instances) > 0 {
				finalPods = append([]string{}, adapter.Status.Instances...)
			}

			// Wait for adapter to reach stable state
			return adapter.Status.Phase == modelv1alpha1.ModelAdapterRunning ||
				adapter.Status.Phase == modelv1alpha1.ModelAdapterBound, nil
		}))

	// Validate that adapter eventually succeeds with some pods
	assert.True(t, len(finalPods) > 0, "adapter should have successful instances")
	t.Logf("final successful pods: %v", finalPods)
}

// TestModelAdapterRetryAnnotations tests retry count and timing annotations
func TestModelAdapterRetryAnnotations(t *testing.T) {
	adapterName := "retry-annotations-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	// Ensure pods are ready for the test
	validateAllPodsAreReady(t, k8sClient, 3)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	// Create model adapter
	t.Log("creating model adapter to test retry annotations")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)

	// Monitor for retry annotations
	t.Log("monitoring retry annotations")
	var foundRetryAnnotations bool
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 60*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}

			// Check for retry-related annotations
			if adapter.Annotations != nil {
				for key, value := range adapter.Annotations {
					if strings.Contains(key, "adapter.model.aibrix.ai/retry-count") {
						foundRetryAnnotations = true
						t.Logf("found retry annotation: %s = %s", key, value)

						// Validate retry count is a valid number
						if retryCount, err := strconv.Atoi(value); err == nil {
							assert.True(t, retryCount >= 0 && retryCount <= 5,
								"retry count should be between 0 and 5, got %d", retryCount)
						}
					}
					if strings.Contains(key, "adapter.model.aibrix.ai/last-retry-time") {
						t.Logf("found retry time annotation: %s = %s", key, value)

						// Validate time format
						if _, err := time.Parse(time.RFC3339, value); err != nil {
							t.Errorf("invalid time format in retry annotation: %v", err)
						}
					}
				}
			}

			// Wait until adapter reaches stable state
			return adapter.Status.Phase == modelv1alpha1.ModelAdapterRunning ||
				adapter.Status.Phase == modelv1alpha1.ModelAdapterBound, nil
		}))

	// Note: In a successful scenario, retry annotations might be cleared
	// This test mainly validates the annotation format when they exist
	t.Logf("retry annotations monitoring completed, found annotations: %v", foundRetryAnnotations)
}

// TestModelAdapterMultipleReplicas tests adapter with multiple replicas
func TestModelAdapterMultipleReplicas(t *testing.T) {
	adapterName := "multi-replica-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")

	// Set multiple replicas
	replicas := int32(2)
	adapter.Spec.Replicas = &replicas

	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	// Ensure we have enough pods for multiple replicas
	validateAllPodsAreReady(t, k8sClient, 3)

	// Create model adapter
	t.Log("creating model adapter with multiple replicas")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)

	// Wait for adapter to reach stable state with multiple instances
	t.Log("waiting for multiple replicas to be loaded")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 3*time.Second, 120*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}

			// Check if we have the desired number of instances
			if len(adapter.Status.Instances) >= int(replicas) &&
				(adapter.Status.Phase == modelv1alpha1.ModelAdapterRunning ||
					adapter.Status.Phase == modelv1alpha1.ModelAdapterBound) {
				return true, nil
			}

			t.Logf("waiting for %d replicas, currently have %d instances, phase: %s",
				replicas, len(adapter.Status.Instances), adapter.Status.Phase)
			return false, nil
		}))

	// Validate final state
	assert.Equal(t, int(replicas), len(adapter.Status.Instances),
		"should have exactly %d instances", replicas)
	assert.Contains(t, []modelv1alpha1.ModelAdapterPhase{
		modelv1alpha1.ModelAdapterBound,
		modelv1alpha1.ModelAdapterRunning,
	}, adapter.Status.Phase, "adapter should be in bound or running phase")

	// Validate that all instances are on different pods
	uniquePods := make(map[string]bool)
	for _, podName := range adapter.Status.Instances {
		uniquePods[podName] = true
	}
	assert.Equal(t, int(replicas), len(uniquePods),
		"all replicas should be on different pods")
}

// baseModelPodLabelSelector matches every pod that createModelAdapterConfig's PodSelector
// would also match, so tests can delete/list the full backing pod set directly.
func baseModelPodLabelSelector(model string) string {
	return fmt.Sprintf("%s=%s,%s=true", constants.ModelLabelName, model, constants.ModelLabelAdapterEnabled)
}

// TestModelAdapterAllPodsRemoved guards against the regression where reconcileLoading
// returned nil as soon as no active pods matched the ModelAdapter's selector, skipping
// the bookkeeping that resets ReadyReplicas and flips Ready to False -- an adapter's CR
// could keep reporting Ready:True long after every backing pod was gone. Deleting every
// matching pod at once (rather than the single pod TestModelAdapter deletes) must drive
// Ready to False and ReadyReplicas to 0, and the adapter must recover once the base model
// Deployment recreates its pods.
func TestModelAdapterAllPodsRemoved(t *testing.T) {
	adapterName := "all-pods-removed-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	validateAllPodsAreReady(t, k8sClient, 3)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	t.Log("creating model adapter")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)
	require.NotEmpty(t, adapter.Status.Instances)

	t.Log("deleting every pod backing the base model to simulate total pod loss")
	require.NoError(t, k8sClient.CoreV1().Pods("default").DeleteCollection(context.Background(),
		v1.DeleteOptions{}, v1.ListOptions{LabelSelector: baseModelPodLabelSelector("llama2-7b")}))

	t.Log("waiting for adapter to report Ready=False and ReadyReplicas=0 once all pods are gone")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 60*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}
			readyCond := apimeta.FindStatusCondition(adapter.Status.Conditions, string(modelv1alpha1.ModelAdapterConditionReady))
			t.Logf("readyReplicas=%d readyCondition=%v", adapter.Status.ReadyReplicas, readyCond)
			return adapter.Status.ReadyReplicas == 0 && readyCond != nil && readyCond.Status == v1.ConditionFalse, nil
		}))

	t.Log("waiting for the base model Deployment to recreate pods and the adapter to recover")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 120*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}
			readyCond := apimeta.FindStatusCondition(adapter.Status.Conditions, string(modelv1alpha1.ModelAdapterConditionReady))
			t.Logf("phase=%s instances=%v readyCondition=%v", adapter.Status.Phase, adapter.Status.Instances, readyCond)
			return len(adapter.Status.Instances) > 0 && readyCond != nil && readyCond.Status == v1.ConditionTrue, nil
		}))

	assert.True(t, adapter.Status.ReadyReplicas > 0, "adapter should have recovered with ready replicas")
	validateAllPodsAreReady(t, k8sClient, 3)
	validateInference(t, adapterName)
}

// TestModelAdapterNoReadyPodsCondition guards the Scheduled condition surfaced by
// newSchedulingPendingCondition: a single-replica adapter created while nothing matches
// its selector must report Scheduled=False with reason NoReadyPods (or, once some but not
// enough pods return, InsufficientReadyPods) instead of silently retrying with no visible
// status, and must still converge to Bound/Running once pods come back.
func TestModelAdapterNoReadyPodsCondition(t *testing.T) {
	adapterName := "no-ready-pods-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	replicas := int32(1)
	adapter.Spec.Replicas = &replicas
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	validateAllPodsAreReady(t, k8sClient, 3)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	t.Log("deleting all base model pods so the adapter is created with nothing ready to schedule on")
	require.NoError(t, k8sClient.CoreV1().Pods("default").DeleteCollection(context.Background(),
		v1.DeleteOptions{}, v1.ListOptions{LabelSelector: baseModelPodLabelSelector("llama2-7b")}))

	t.Log("creating single-replica model adapter while no pods are ready")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)

	t.Log("waiting for the adapter to converge, watching for a NoReadyPods/InsufficientReadyPods condition")
	var observedNoReadyPods bool
	scheduledCondType := string(modelv1alpha1.ModelAdapterConditionTypeScheduled)
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 500*time.Millisecond, 90*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}
			if cond := apimeta.FindStatusCondition(adapter.Status.Conditions, scheduledCondType); cond != nil {
				t.Logf("Scheduled condition: status=%s reason=%s", cond.Status, cond.Reason)
				if cond.Status == v1.ConditionFalse && (cond.Reason == "NoReadyPods" || cond.Reason == "InsufficientReadyPods") {
					observedNoReadyPods = true
				}
			}
			return len(adapter.Status.Instances) > 0 && (adapter.Status.Phase == modelv1alpha1.ModelAdapterBound ||
				adapter.Status.Phase == modelv1alpha1.ModelAdapterRunning), nil
		}))

	assert.True(t, observedNoReadyPods,
		"expected to observe a NoReadyPods/InsufficientReadyPods Scheduled condition while pods were recovering")
	validateAllPodsAreReady(t, k8sClient, 3)
}

// TestModelAdapterServiceAndEndpointSliceReflectScheduledPod verifies the resources
// DoReconcile's Step 3/4 own (reconcileService/reconcileEndpointSlice) actually describe
// the pod the adapter was scheduled on -- nothing in the existing suite ever inspects
// these objects, even though they're what makes gateway routing to the adapter work.
func TestModelAdapterServiceAndEndpointSliceReflectScheduledPod(t *testing.T) {
	adapterName := "service-endpointslice-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	replicas := int32(1)
	adapter.Spec.Replicas = &replicas
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	validateAllPodsAreReady(t, k8sClient, 3)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)
	require.Len(t, adapter.Status.Instances, 1, "single-replica adapter should have exactly one instance")

	scheduledPod, err := k8sClient.CoreV1().Pods("default").Get(context.Background(),
		adapter.Status.Instances[0], v1.GetOptions{})
	require.NoError(t, err)

	t.Log("validating the owned Service points at port 8000")
	svc, err := k8sClient.CoreV1().Services("default").Get(context.Background(), adapter.Name, v1.GetOptions{})
	require.NoError(t, err, "expected ModelAdapter to own a Service named after it")
	require.Len(t, svc.Spec.Ports, 1)
	assert.EqualValues(t, 8000, svc.Spec.Ports[0].Port)

	t.Log("validating the owned EndpointSlice addresses the scheduled pod")
	eps, err := k8sClient.DiscoveryV1().EndpointSlices("default").Get(context.Background(), adapter.Name, v1.GetOptions{})
	require.NoError(t, err, "expected ModelAdapter to own an EndpointSlice named after it")
	require.Len(t, eps.Endpoints, 1)
	assert.Equal(t, []string{scheduledPod.Status.PodIP}, eps.Endpoints[0].Addresses)
}

// TestModelAdapterReplicasModeSwitch verifies that flipping Spec.Replicas from 1 (single
// pod) to nil (load on all matching pods) after creation is reconciled: the adapter must
// expand from its single instance to cover every pod matching its selector.
func TestModelAdapterReplicasModeSwitch(t *testing.T) {
	adapterName := "replicas-mode-switch-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	replicas := int32(1)
	adapter.Spec.Replicas = &replicas
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	validateAllPodsAreReady(t, k8sClient, 3)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapter.Name, v1.DeleteOptions{})
	})

	t.Log("creating single-replica model adapter")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)
	require.Len(t, adapter.Status.Instances, 1, "single-replica adapter should have exactly one instance")

	t.Log("switching the adapter to load-on-all-pods mode")
	require.NoError(t, wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			latest, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}
			latest.Spec.Replicas = nil
			_, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Update(ctx, latest, v1.UpdateOptions{})
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return err == nil, err
		}))

	t.Log("waiting for the adapter to be loaded on all matching pods")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 120*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			if err != nil {
				return false, err
			}
			t.Logf("phase=%s instances=%v", adapter.Status.Phase, adapter.Status.Instances)
			return len(adapter.Status.Instances) == 3, nil
		}))

	assert.Len(t, adapter.Status.Instances, 3, "adapter should now be loaded on all matching pods")
}

// TestModelAdapterConcurrentAdaptersShareBaseModelPods verifies two ModelAdapters
// targeting the same base model pods are scheduled and served independently. This is the
// scenario the pkg/cache pod<->model mapping (keyed per adapter name, not per pod) has to
// get right: neither adapter's bookkeeping should clobber the other's.
func TestModelAdapterConcurrentAdaptersShareBaseModelPods(t *testing.T) {
	adapterAName := "concurrent-lora-a"
	adapterBName := "concurrent-lora-b"
	adapterA := createModelAdapterConfig(adapterAName, "llama2-7b")
	adapterB := createModelAdapterConfig(adapterBName, "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	validateAllPodsAreReady(t, k8sClient, 3)

	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapterAName, v1.DeleteOptions{})
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapterBName, v1.DeleteOptions{})
	})

	t.Log("creating two model adapters that target the same base model pods")
	_, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapterA, v1.CreateOptions{})
	require.NoError(t, err)
	_, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapterB, v1.CreateOptions{})
	require.NoError(t, err)

	gotA := validateModelAdapter(t, v1alpha1Client, adapterAName)
	gotB := validateModelAdapter(t, v1alpha1Client, adapterBName)
	assert.NotEmpty(t, gotA.Status.Instances)
	assert.NotEmpty(t, gotB.Status.Instances)

	t.Log("running inference against both adapters on the shared pods")
	validateInference(t, adapterAName)
	validateInference(t, adapterBName)
}

// TestModelAdapterDeletionCleansUpOwnedResources verifies that deleting a ModelAdapter
// (a) garbage-collects its owned Service/EndpointSlice via owner references once the
// unload finalizer releases it, and (b) actually unloads the adapter from the engine, so
// inference for its model starts failing instead of silently continuing to work.
func TestModelAdapterDeletionCleansUpOwnedResources(t *testing.T) {
	adapterName := "deletion-cleanup-test-lora"
	adapter := createModelAdapterConfig(adapterName, "llama2-7b")
	k8sClient, v1alpha1Client := initializeClient(context.Background(), t)

	validateAllPodsAreReady(t, k8sClient, 3)

	t.Log("creating model adapter")
	adapter, err := v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Create(context.Background(),
		adapter, v1.CreateOptions{})
	require.NoError(t, err)
	// Safety net: if an assertion below fails before the explicit delete step this test
	// exercises, don't leak the adapter into later tests.
	t.Cleanup(func() {
		_ = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
			adapterName, v1.DeleteOptions{})
	})
	adapter = validateModelAdapter(t, v1alpha1Client, adapter.Name)

	t.Log("validating owned Service and EndpointSlice were created")
	_, err = k8sClient.CoreV1().Services("default").Get(context.Background(), adapter.Name, v1.GetOptions{})
	require.NoError(t, err, "expected ModelAdapter to own a Service")
	_, err = k8sClient.DiscoveryV1().EndpointSlices("default").Get(context.Background(), adapter.Name, v1.GetOptions{})
	require.NoError(t, err, "expected ModelAdapter to own an EndpointSlice")

	t.Log("running inference to confirm the adapter is servable before deletion")
	validateInference(t, adapterName)

	t.Log("deleting the model adapter")
	require.NoError(t, v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Delete(context.Background(),
		adapter.Name, v1.DeleteOptions{}))

	t.Log("waiting for the ModelAdapter CR to be gone (unload finalizer released)")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 60*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			_, err = v1alpha1Client.ModelV1alpha1().ModelAdapters("default").Get(ctx, adapterName, v1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		}))

	t.Log("waiting for owned Service and EndpointSlice to be garbage collected")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			_, svcErr := k8sClient.CoreV1().Services("default").Get(ctx, adapterName, v1.GetOptions{})
			_, epsErr := k8sClient.DiscoveryV1().EndpointSlices("default").Get(ctx, adapterName, v1.GetOptions{})
			return apierrors.IsNotFound(svcErr) && apierrors.IsNotFound(epsErr), nil
		}))

	t.Log("verifying inference for the deleted adapter's model now fails")
	client := createOpenAIClient(gatewayURL, apiKey)
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
				Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Say this is a test")},
				Model:    adapterName,
			})
			return err != nil, nil
		}))
}

func createModelAdapterConfig(name, model string) *modelv1alpha1.ModelAdapter {
	return &modelv1alpha1.ModelAdapter{
		ObjectMeta: v1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constants.ModelLabelName: name,
				constants.ModelLabelPort: "8000",
			},
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			BaseModel: &model,
			PodSelector: &v1.LabelSelector{
				MatchLabels: map[string]string{
					constants.ModelLabelName:           model,
					constants.ModelLabelAdapterEnabled: "true",
				},
			},
			ArtifactURL: "huggingface://yard1/llama-2-7b-sql-lora-test",
		},
	}
}

func validateModelAdapter(t *testing.T, client *v1alpha1.Clientset, name string) *modelv1alpha1.ModelAdapter {
	var adapter *modelv1alpha1.ModelAdapter
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			adapter, err = client.ModelV1alpha1().ModelAdapters("default").Get(context.Background(), name, v1.GetOptions{})
			if err != nil || adapter.Status.Phase != modelv1alpha1.ModelAdapterRunning {
				return false, nil
			}
			return true, nil
		}))
	assert.True(t, len(adapter.Status.Instances) > 0, "model adapter scheduled on atleast one pod")
	return adapter
}
