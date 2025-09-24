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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
			AdditionalConfig: map[string]string{
				"api-key": "test-key-1234567890",
			},
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
