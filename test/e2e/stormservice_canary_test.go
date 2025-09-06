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

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

const (
	canaryTestStormServiceName = "canary-test-storm"
	canaryTestTimeout          = 300 * time.Second
	canaryPollInterval         = 5 * time.Second
	initialBusyboxImage        = "busybox:1.35"
	updatedBusyboxImage        = "busybox:1.36"
)

// TestStormServiceCanaryReplicaMode tests canary deployment in replica mode
func TestStormServiceCanaryReplicaMode(t *testing.T) {
	ctx := context.Background()
	k8sClient, v1alpha1Client := initializeClient(ctx, t)

	// Create StormService with canary configuration - replica mode
	stormService := createCanaryReplicaModeStormService()

	t.Cleanup(func() {
		_ = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Delete(
			ctx, stormService.Name, metav1.DeleteOptions{})
		waitForStormServiceDeletion(t, v1alpha1Client, stormService.Name)
	})

	// Create the StormService
	t.Log("creating StormService with canary configuration (replica mode)")
	createdStorm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Create(
		ctx, stormService, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for initial deployment to complete
	t.Log("waiting for initial deployment to complete")
	err = waitForStormServiceReady(ctx, t, k8sClient, v1alpha1Client, createdStorm, 5*time.Minute)
	require.NoError(t, err)

	// Fetch the updated StormService after ready check
	storm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(ctx, createdStorm.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, int32(2), *storm.Spec.Replicas, "should have 2 replicas")

	// Trigger canary update by changing image
	t.Log("triggering canary update by changing container image")
	t.Logf("changing image from %s to %s",
		storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image, updatedBusyboxImage)

	// Store original image for verification
	originalImage := storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image

	// Update both roles to ensure consistent change
	storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = updatedBusyboxImage
	if len(storm.Spec.Template.Spec.Roles) > 1 {
		storm.Spec.Template.Spec.Roles[1].Template.Spec.Containers[0].Image = updatedBusyboxImage
	}
	updatedStorm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Update(
		ctx, storm, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Logf("update applied, new generation: %d, image changed: %s -> %s",
		updatedStorm.Generation, originalImage, updatedStorm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image)

	// Wait for controller to detect the change and create new revision
	originalRevision := storm.Status.CurrentRevision
	waitForRevisionChange(t, v1alpha1Client, updatedStorm.Name, originalRevision)

	// Monitor canary progression through steps
	t.Log("monitoring canary deployment progression")

	// Step 1: Should reach 33% weight
	waitForCanaryWeight(t, v1alpha1Client, updatedStorm.Name, 33, "first canary step")

	// Wait for automatic pause to complete (30s duration)
	time.Sleep(35 * time.Second)

	// Step 2: Should advance to 66% weight
	waitForCanaryWeight(t, v1alpha1Client, updatedStorm.Name, 66, "second canary step")

	// Step 3: Manual pause - should be waiting for confirmation
	waitForCanaryManualPause(t, v1alpha1Client, updatedStorm.Name)

	// Resume canary using new pause condition approach
	t.Log("resuming canary deployment from manual pause by removing pause condition")
	storm, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(
		ctx, updatedStorm.Name, metav1.GetOptions{})
	require.NoError(t, err)

	t.Logf("before resume: canary step=%d, phase=%s, weight=%d%%, pauseConditions=%d",
		storm.Status.CanaryStatus.CurrentStep,
		storm.Status.CanaryStatus.Phase, storm.Status.CanaryStatus.CurrentWeight,
		len(storm.Status.CanaryStatus.PauseConditions))

	// Resume by removing CanaryPauseStep pause condition
	if len(storm.Status.CanaryStatus.PauseConditions) > 0 {
		// Clear all pause conditions to resume
		storm.Status.CanaryStatus.PauseConditions = []orchestrationv1alpha1.PauseCondition{}

		// Use status subresource to update pause conditions
		resumedStorm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").UpdateStatus(
			ctx, storm, metav1.UpdateOptions{})
		require.NoError(t, err)
		t.Logf("resume applied: cleared pause conditions, generation=%d", resumedStorm.Generation)
	} else {
		t.Log("no pause conditions found, no resume needed")
	}

	// Wait for controller to process the resume
	waitForCanaryResume(t, v1alpha1Client, updatedStorm.Name)

	// Step 4: Should complete at 100% weight
	waitForCanaryCompletion(t, v1alpha1Client, updatedStorm.Name)

	t.Log("canary deployment completed successfully")
}

// TestStormServiceCanaryPooledMode tests canary deployment in pooled mode
func TestStormServiceCanaryPooledMode(t *testing.T) {
	ctx := context.Background()
	k8sClient, v1alpha1Client := initializeClient(ctx, t)

	// Create StormService with canary configuration - pooled mode
	stormService := createCanaryPooledModeStormService()

	t.Cleanup(func() {
		_ = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Delete(
			ctx, stormService.Name, metav1.DeleteOptions{})
		waitForStormServiceDeletion(t, v1alpha1Client, stormService.Name)
	})

	// Create the StormService
	t.Log("creating StormService with canary configuration (pooled mode)")
	createdStorm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Create(
		ctx, stormService, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for initial deployment to complete
	t.Log("waiting for initial deployment to complete")
	err = waitForStormServiceReady(ctx, t, k8sClient, v1alpha1Client, createdStorm, 5*time.Minute)
	require.NoError(t, err)

	// Fetch the updated StormService after ready check
	storm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(ctx, createdStorm.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, int32(1), *storm.Spec.Replicas, "should have 1 replica (pooled mode)")

	// Trigger canary update
	t.Log("triggering canary update by changing container image")
	t.Logf("changing image from %s to %s",
		storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image, updatedBusyboxImage)
	storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = updatedBusyboxImage
	if len(storm.Spec.Template.Spec.Roles) > 1 {
		storm.Spec.Template.Spec.Roles[1].Template.Spec.Containers[0].Image = updatedBusyboxImage
	}
	updatedStorm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Update(
		ctx, storm, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Monitor canary progression - pooled mode has different weight progression
	t.Log("monitoring pooled mode canary deployment progression")

	// Step 1: 25% weight
	waitForCanaryWeight(t, v1alpha1Client, updatedStorm.Name, 25, "first pooled canary step")

	// Step 2: 50% weight after auto-pause
	time.Sleep(65 * time.Second) // Wait for 60s auto-pause + buffer
	waitForCanaryWeight(t, v1alpha1Client, updatedStorm.Name, 50, "second pooled canary step")

	// Step 3: 75% weight after auto-pause
	time.Sleep(125 * time.Second) // Wait for 120s auto-pause + buffer
	waitForCanaryWeight(t, v1alpha1Client, updatedStorm.Name, 75, "third pooled canary step")

	// Step 4: Manual pause at 75%
	waitForCanaryManualPause(t, v1alpha1Client, updatedStorm.Name)

	// Resume and complete by removing pause conditions
	t.Log("resuming pooled mode canary deployment by clearing pause conditions")
	storm, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(
		ctx, updatedStorm.Name, metav1.GetOptions{})
	require.NoError(t, err)

	// Clear pause conditions to resume
	if len(storm.Status.CanaryStatus.PauseConditions) > 0 {
		storm.Status.CanaryStatus.PauseConditions = []orchestrationv1alpha1.PauseCondition{}
		_, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").UpdateStatus(
			ctx, storm, metav1.UpdateOptions{})
		require.NoError(t, err)
	}

	waitForCanaryCompletion(t, v1alpha1Client, updatedStorm.Name)

	t.Log("pooled mode canary deployment completed successfully")
}

// TestStormServiceManualPauseResume tests manual pause and resume functionality
func TestStormServiceManualPauseResume(t *testing.T) {
	ctx := context.Background()
	k8sClient, v1alpha1Client := initializeClient(ctx, t)

	// Create simple canary StormService
	stormService := createSimpleCanaryStormService()

	t.Cleanup(func() {
		_ = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Delete(
			ctx, stormService.Name, metav1.DeleteOptions{})
		waitForStormServiceDeletion(t, v1alpha1Client, stormService.Name)
	})

	// Create and wait for ready
	t.Log("creating StormService for pause/resume test")
	createdStorm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Create(
		ctx, stormService, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForStormServiceReady(ctx, t, k8sClient, v1alpha1Client, createdStorm, 5*time.Minute)
	require.NoError(t, err)

	// Fetch the updated StormService after ready check
	storm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(ctx, createdStorm.Name, metav1.GetOptions{})
	require.NoError(t, err)

	// Trigger canary update
	t.Log("triggering canary update")
	t.Logf("changing image from %s to %s",
		storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image, updatedBusyboxImage)
	storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = updatedBusyboxImage
	if len(storm.Spec.Template.Spec.Roles) > 1 {
		storm.Spec.Template.Spec.Roles[1].Template.Spec.Containers[0].Image = updatedBusyboxImage
	}
	_, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Update(
		ctx, storm, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Wait for canary to start
	waitForCanaryInitialization(t, v1alpha1Client, storm.Name)

	// Pause the canary globally
	t.Log("pausing canary deployment globally")
	storm, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(
		ctx, storm.Name, metav1.GetOptions{})
	require.NoError(t, err)

	storm.Spec.Paused = true
	_, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Update(
		ctx, storm, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Verify canary is paused
	waitForCanaryPause(t, v1alpha1Client, storm.Name)

	// Resume the canary
	t.Log("resuming canary deployment")
	storm, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(
		ctx, storm.Name, metav1.GetOptions{})
	require.NoError(t, err)

	storm.Spec.Paused = false
	_, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Update(
		ctx, storm, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Wait for completion
	waitForCanaryCompletion(t, v1alpha1Client, storm.Name)

	t.Log("pause/resume test completed successfully")
}

// TestStormServiceCanaryStatusProgression tests canary status field updates
func TestStormServiceCanaryStatusProgression(t *testing.T) {
	ctx := context.Background()
	k8sClient, v1alpha1Client := initializeClient(ctx, t)

	// Create StormService
	stormService := createSimpleCanaryStormService()

	t.Cleanup(func() {
		_ = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Delete(
			ctx, stormService.Name, metav1.DeleteOptions{})
		waitForStormServiceDeletion(t, v1alpha1Client, stormService.Name)
	})

	// Create and trigger update
	createdStorm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Create(
		ctx, stormService, metav1.CreateOptions{})
	require.NoError(t, err)

	err = waitForStormServiceReady(ctx, t, k8sClient, v1alpha1Client, createdStorm, 5*time.Minute)
	require.NoError(t, err)

	// Fetch the updated StormService after ready check
	storm, err := v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(ctx, createdStorm.Name, metav1.GetOptions{})
	require.NoError(t, err)

	// Capture initial revision
	initialRevision := storm.Status.CurrentRevision

	// Trigger update
	t.Logf("changing image from %s to %s",
		storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image, updatedBusyboxImage)
	storm.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = updatedBusyboxImage
	if len(storm.Spec.Template.Spec.Roles) > 1 {
		storm.Spec.Template.Spec.Roles[1].Template.Spec.Containers[0].Image = updatedBusyboxImage
	}
	_, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Update(
		ctx, storm, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Monitor status progression
	t.Log("monitoring canary status progression")

	var canaryStatus *orchestrationv1alpha1.CanaryStatus
	assert.NoError(t, wait.PollUntilContextTimeout(ctx, canaryPollInterval, canaryTestTimeout, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(
				ctx, storm.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}

			if storm.Status.CanaryStatus != nil {
				canaryStatus = storm.Status.CanaryStatus
				t.Logf("canary status: step=%d, weight=%d, phase=%s, stable=%s, canary=%s",
					canaryStatus.CurrentStep,
					canaryStatus.CurrentWeight,
					canaryStatus.Phase,
					canaryStatus.StableRevision,
					canaryStatus.CanaryRevision)

				// Validate status fields
				assert.True(t, canaryStatus.CurrentStep >= 0, "current step should be non-negative")
				assert.True(t, canaryStatus.CurrentWeight >= 0 && canaryStatus.CurrentWeight <= 100,
					"weight should be between 0-100")
				assert.NotEmpty(t, canaryStatus.StableRevision, "stable revision should not be empty")
				assert.NotEmpty(t, canaryStatus.CanaryRevision, "canary revision should not be empty")
				assert.NotEqual(t, canaryStatus.StableRevision, canaryStatus.CanaryRevision,
					"stable and canary revisions should be different")
				assert.Equal(t, initialRevision, canaryStatus.StableRevision,
					"stable revision should match initial revision")

				// Check phase is valid
				validPhases := []orchestrationv1alpha1.CanaryPhase{
					orchestrationv1alpha1.CanaryPhaseInitializing,
					orchestrationv1alpha1.CanaryPhaseProgressing,
					orchestrationv1alpha1.CanaryPhasePaused,
					orchestrationv1alpha1.CanaryPhaseCompleted,
				}
				assert.Contains(t, validPhases, canaryStatus.Phase, "phase should be valid")

				return canaryStatus.Phase == orchestrationv1alpha1.CanaryPhaseCompleted, nil
			}

			return false, nil
		}))

	// Verify final state
	assert.NotNil(t, canaryStatus, "should have canary status during deployment")

	// After completion, canary status should be cleared
	assert.NoError(t, wait.PollUntilContextTimeout(ctx, canaryPollInterval, 30*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err = v1alpha1Client.OrchestrationV1alpha1().StormServices("default").Get(
				ctx, storm.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}

			// Status should be cleared after completion
			return storm.Status.CanaryStatus == nil, nil
		}))

	t.Log("canary status progression test completed successfully")
}

// Helper functions

func createCanaryReplicaModeStormService() *orchestrationv1alpha1.StormService {
	return &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name: canaryTestStormServiceName + "-replica",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To(int32(2)), // Replica mode
			Stateful: true,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "canary-test-replica",
				},
			},
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type:           orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
				MaxUnavailable: &intstr.IntOrString{IntVal: 1},
				MaxSurge:       &intstr.IntOrString{IntVal: 1},
				Canary: &orchestrationv1alpha1.CanaryUpdateStrategy{
					Steps: []orchestrationv1alpha1.CanaryStep{
						{SetWeight: ptr.To(int32(33))},
						{Pause: &orchestrationv1alpha1.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "30s"}}},
						{SetWeight: ptr.To(int32(66))},
						{Pause: &orchestrationv1alpha1.PauseStep{}}, // Manual pause
						{SetWeight: ptr.To(int32(100))},
					},
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "canary-test-replica",
					},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{
							Name:         "prefill",
							Replicas:     ptr.To(int32(2)),
							PodGroupSize: ptr.To(int32(2)),
							Stateful:     true,
							Template:     createPodTemplateSpec(),
						},
						{
							Name:         "decode",
							Replicas:     ptr.To(int32(2)),
							PodGroupSize: ptr.To(int32(2)),
							Stateful:     true,
							Template:     createPodTemplateSpec(),
						},
					},
				},
			},
		},
	}
}

func createCanaryPooledModeStormService() *orchestrationv1alpha1.StormService {
	return &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name: canaryTestStormServiceName + "-pooled",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To(int32(1)), // Pooled mode
			Stateful: true,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "canary-test-pooled",
				},
			},
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type: orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType,
				Canary: &orchestrationv1alpha1.CanaryUpdateStrategy{
					Steps: []orchestrationv1alpha1.CanaryStep{
						{SetWeight: ptr.To(int32(25))},
						{Pause: &orchestrationv1alpha1.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "60s"}}},
						{SetWeight: ptr.To(int32(50))},
						{Pause: &orchestrationv1alpha1.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "120s"}}},
						{SetWeight: ptr.To(int32(75))},
						{Pause: &orchestrationv1alpha1.PauseStep{}}, // Manual pause
						{SetWeight: ptr.To(int32(100))},
					},
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "canary-test-pooled",
					},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{
							Name:         "prefill",
							Replicas:     ptr.To(int32(2)),
							PodGroupSize: ptr.To(int32(2)),
							Stateful:     true,
							Template:     createPodTemplateSpec(),
						},
						{
							Name:         "decode",
							Replicas:     ptr.To(int32(2)),
							PodGroupSize: ptr.To(int32(2)),
							Stateful:     true,
							Template:     createPodTemplateSpec(),
						},
					},
				},
			},
		},
	}
}

func createSimpleCanaryStormService() *orchestrationv1alpha1.StormService {
	return &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name: canaryTestStormServiceName + "-simple",
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To(int32(1)),
			Stateful: true,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "canary-test-simple",
				},
			},
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type: orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
				Canary: &orchestrationv1alpha1.CanaryUpdateStrategy{
					Steps: []orchestrationv1alpha1.CanaryStep{
						{SetWeight: ptr.To(int32(50))},
						{Pause: &orchestrationv1alpha1.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "10s"}}},
						{SetWeight: ptr.To(int32(100))},
					},
				},
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "canary-test-simple",
					},
				},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{
							Name:         "prefill",
							Replicas:     ptr.To(int32(1)),
							PodGroupSize: ptr.To(int32(1)),
							Stateful:     true,
							Template:     createPodTemplateSpec(),
						},
						{
							Name:         "decode",
							Replicas:     ptr.To(int32(1)),
							PodGroupSize: ptr.To(int32(1)),
							Stateful:     true,
							Template:     createPodTemplateSpec(),
						},
					},
				},
			},
		},
	}
}

func createPodTemplateSpec() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "busybox",
					Image:           initialBusyboxImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"sh", "-c"},
					Args: []string{
						"echo \"${PODSET_NAME}-0.${STORM_SERVICE_NAME}.default.svc.cluster.local:5000\" && sleep 1000000",
					},
				},
			},
		},
	}
}

func waitForStormServiceDeletion(t *testing.T, client *v1alpha1.Clientset, name string) {
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), canaryPollInterval, canaryTestTimeout, true,
		func(ctx context.Context) (done bool, err error) {
			_, err = client.OrchestrationV1alpha1().StormServices("default").Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, nil
		}))
}

func waitForCanaryWeight(
	t *testing.T, client *v1alpha1.Clientset, name string, expectedWeight int32, stepDescription string) {
	t.Logf("waiting for canary to reach %d%% weight (%s)", expectedWeight, stepDescription)
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), canaryPollInterval, canaryTestTimeout, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err := client.OrchestrationV1alpha1().StormServices("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Logf("error getting storm service: %v", err)
				return false, err
			}

			t.Logf("storm status: ready=%d/%d, update=%s, current=%s",
				storm.Status.ReadyReplicas, storm.Status.Replicas,
				storm.Status.UpdateRevision, storm.Status.CurrentRevision)

			if storm.Status.CanaryStatus != nil {
				t.Logf("current canary: step=%d, weight=%d%%, phase=%s, stable=%s, canary=%s",
					storm.Status.CanaryStatus.CurrentStep,
					storm.Status.CanaryStatus.CurrentWeight,
					storm.Status.CanaryStatus.Phase,
					storm.Status.CanaryStatus.StableRevision,
					storm.Status.CanaryStatus.CanaryRevision)
				return storm.Status.CanaryStatus.CurrentWeight == expectedWeight, nil
			} else {
				t.Logf("no canary status found")
			}

			return false, nil
		}))
}

func waitForCanaryManualPause(t *testing.T, client *v1alpha1.Clientset, name string) {
	t.Log("waiting for canary to reach manual pause state")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), canaryPollInterval, canaryTestTimeout, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err := client.OrchestrationV1alpha1().StormServices("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}

			if storm.Status.CanaryStatus != nil {
				isPaused := storm.Status.CanaryStatus.Phase == orchestrationv1alpha1.CanaryPhasePaused
				hasTimestamp := storm.Status.CanaryStatus.PausedAt != nil

				// Check if any pause condition is CanaryPauseStep
				var hasCanaryPauseStep bool
				for _, condition := range storm.Status.CanaryStatus.PauseConditions {
					if condition.Reason == orchestrationv1alpha1.PauseReasonCanaryPauseStep {
						hasCanaryPauseStep = true
						break
					}
				}

				t.Logf("canary pause state: phase=%s, pausedAt=%v, pauseConditions=%d, hasCanaryPauseStep=%v",
					storm.Status.CanaryStatus.Phase, hasTimestamp, len(storm.Status.CanaryStatus.PauseConditions), hasCanaryPauseStep)

				// Manual pause is indicated by CanaryPauseStep condition
				return isPaused && (hasTimestamp || hasCanaryPauseStep), nil
			}

			return false, nil
		}))
}

func waitForCanaryInitialization(t *testing.T, client *v1alpha1.Clientset, name string) {
	t.Log("waiting for canary initialization")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), canaryPollInterval, 60*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err := client.OrchestrationV1alpha1().StormServices("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}

			return storm.Status.CanaryStatus != nil, nil
		}))
}

func waitForCanaryPause(t *testing.T, client *v1alpha1.Clientset, name string) {
	t.Log("waiting for canary to pause")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), canaryPollInterval, 60*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err := client.OrchestrationV1alpha1().StormServices("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}

			if storm.Status.CanaryStatus != nil {
				return storm.Status.CanaryStatus.Phase == orchestrationv1alpha1.CanaryPhasePaused, nil
			}

			return false, nil
		}))
}

func waitForRevisionChange(t *testing.T, client *v1alpha1.Clientset, name string, originalRevision string) {
	t.Log("waiting for revision change to trigger canary")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), canaryPollInterval, 60*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err := client.OrchestrationV1alpha1().StormServices("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Logf("error getting storm service: %v", err)
				return false, err
			}

			t.Logf("revision check: update=%s, current=%s (original=%s)",
				storm.Status.UpdateRevision, storm.Status.CurrentRevision, originalRevision)

			// We're looking for either different revisions or canary status to appear
			if storm.Status.UpdateRevision != storm.Status.CurrentRevision {
				t.Logf("revisions are different - canary should trigger")
				return true, nil
			}

			if storm.Status.CanaryStatus != nil {
				t.Logf("canary status appeared - update detected")
				return true, nil
			}

			return false, nil
		}))
}

func waitForCanaryResume(t *testing.T, client *v1alpha1.Clientset, name string) {
	t.Log("waiting for canary to resume from pause")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), canaryPollInterval, 60*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err := client.OrchestrationV1alpha1().StormServices("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Logf("error getting storm service: %v", err)
				return false, err
			}

			t.Logf("resume check: spec.paused=%v, canary phase=%s, step=%d, pausedAt=%v",
				storm.Spec.Paused, storm.Status.CanaryStatus.Phase,
				storm.Status.CanaryStatus.CurrentStep, storm.Status.CanaryStatus.PausedAt)

			// Check if canary resumed (not paused anymore and moved to next step)
			if storm.Status.CanaryStatus.Phase != orchestrationv1alpha1.CanaryPhasePaused {
				t.Logf("canary resumed - phase is now %s", storm.Status.CanaryStatus.Phase)
				return true, nil
			}

			// Or if pausedAt timestamp was cleared
			if storm.Status.CanaryStatus.PausedAt == nil {
				t.Logf("canary resumed - pausedAt cleared")
				return true, nil
			}

			return false, nil
		}))
}

func waitForCanaryCompletion(t *testing.T, client *v1alpha1.Clientset, name string) {
	t.Log("waiting for canary deployment to complete")
	assert.NoError(t, wait.PollUntilContextTimeout(context.Background(), canaryPollInterval, canaryTestTimeout, true,
		func(ctx context.Context) (done bool, err error) {
			storm, err := client.OrchestrationV1alpha1().StormServices("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Logf("error getting storm service: %v", err)
				return false, err
			}

			t.Logf("completion check: ready=%d/%d, update=%s, current=%s",
				storm.Status.ReadyReplicas, storm.Status.Replicas,
				storm.Status.UpdateRevision, storm.Status.CurrentRevision)

			if storm.Status.CanaryStatus != nil {
				t.Logf("canary still active: step=%d, weight=%d%%, phase=%s",
					storm.Status.CanaryStatus.CurrentStep,
					storm.Status.CanaryStatus.CurrentWeight,
					storm.Status.CanaryStatus.Phase)
				return false, nil
			} else {
				t.Logf("canary status cleared - deployment completed")
				return true, nil
			}
		}))
}
