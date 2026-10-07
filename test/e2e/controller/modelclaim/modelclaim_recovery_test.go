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

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func TestModelClaimPendingRecoversWhenPoolAppears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	k8sClient, aibrixClient := initializeClient(ctx, t)
	cleanupLifecycleResources(t, k8sClient, aibrixClient, false)
	t.Cleanup(func() {
		cleanupLifecycleResources(t, k8sClient, aibrixClient, true)
	})

	createLifecycleClaim(t, ctx, aibrixClient, lifecycleBusyClaim, lifecycleBusyModel)
	waitForLifecycleModelPending(t, lifecycleBusyModel, 30*time.Second)

	_, err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Create(
		ctx, lifecyclePoolDeployment(), metav1.CreateOptions{},
	)
	require.NoError(t, err)
	pod := waitForLifecyclePoolPod(t, ctx, k8sClient)
	claim := waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleBusyClaim, modelv1alpha1.ModelClaimActive,
	)
	require.Len(t, claim.Status.Instances, 1)
	assert.Equal(t, pod.Name, claim.Status.Instances[0].Pod)
	waitForLifecycleModelStatus(t, lifecycleBusyModel, http.StatusOK, 30*time.Second)
}

func TestModelClaimDeletionCleansRuntimeAndGateway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	k8sClient, aibrixClient := initializeClient(ctx, t)
	cleanupLifecycleResources(t, k8sClient, aibrixClient, false)
	t.Cleanup(func() {
		cleanupLifecycleResources(t, k8sClient, aibrixClient, true)
	})

	_, err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Create(
		ctx, lifecyclePoolDeployment(), metav1.CreateOptions{},
	)
	require.NoError(t, err)
	pod := waitForLifecyclePoolPod(t, ctx, k8sClient)
	createLifecycleClaim(t, ctx, aibrixClient, lifecycleIdleClaim, lifecycleIdleModel)
	waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleIdleClaim, modelv1alpha1.ModelClaimActive,
	)
	waitForLifecycleModelStatus(t, lifecycleIdleModel, http.StatusOK, 30*time.Second)

	require.NoError(t, aibrixClient.ModelV1alpha1().ModelClaims(lifecycleNamespace).Delete(
		ctx, lifecycleIdleClaim, metav1.DeleteOptions{},
	))
	waitForLifecycleRouteAbsent(t, ctx, k8sClient, pod.Name, lifecycleIdleClaim)
	waitForLifecycleRuntimeModelAbsent(t, ctx, k8sClient, pod.Name, lifecycleIdleModel)
	waitForLifecycleClaimDeleted(t, ctx, aibrixClient, lifecycleIdleClaim)
	waitForLifecycleModelStatus(t, lifecycleIdleModel, http.StatusBadRequest, 60*time.Second)
}

func TestModelClaimRecoversAfterPoolPodReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	k8sClient, aibrixClient := initializeClient(ctx, t)
	cleanupLifecycleResources(t, k8sClient, aibrixClient, false)
	t.Cleanup(func() {
		cleanupLifecycleResources(t, k8sClient, aibrixClient, true)
	})

	_, err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Create(
		ctx, lifecyclePoolDeployment(), metav1.CreateOptions{},
	)
	require.NoError(t, err)
	oldPod := waitForLifecyclePoolPod(t, ctx, k8sClient)
	createLifecycleClaim(t, ctx, aibrixClient, lifecycleBusyClaim, lifecycleBusyModel)
	waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleBusyClaim, modelv1alpha1.ModelClaimActive,
	)
	waitForLifecycleModelStatus(t, lifecycleBusyModel, http.StatusOK, 30*time.Second)

	require.NoError(t, k8sClient.CoreV1().Pods(lifecycleNamespace).Delete(
		ctx, oldPod.Name, metav1.DeleteOptions{},
	))
	replacement := waitForLifecycleDeploymentPodReplacement(t, ctx, k8sClient, oldPod.UID)
	recovered := waitForLifecycleClaimOnPod(
		t, ctx, aibrixClient, lifecycleBusyClaim, replacement.Name,
	)
	require.Len(t, recovered.Status.Instances, 1)
	assert.Equal(t, replacement.Name, recovered.Status.Instances[0].Pod)
	waitForLifecycleRouteBinding(t, ctx, k8sClient, replacement.Name, lifecycleBusyClaim, lifecycleRouteBinding{
		Model: lifecycleBusyModel,
		Port:  lifecycleBusyPort,
		State: constants.ModelClaimRoutingStateActive,
	})
	waitForLifecycleModelStatus(t, lifecycleBusyModel, http.StatusOK, 60*time.Second)

	pods, err := k8sClient.CoreV1().Pods(lifecycleNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + lifecycleDeploymentName,
	})
	require.NoError(t, err)
	routed := 0
	for i := range pods.Items {
		if _, found := pods.Items[i].Annotations[constants.ModelClaimPodAnnotationPrefix+lifecycleBusyClaim]; found {
			routed++
		}
	}
	assert.Equal(t, 1, routed)
}

func TestModelClaimDeletionKeepsColocatedModelServing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	k8sClient, aibrixClient := initializeClient(ctx, t)
	cleanupLifecycleResources(t, k8sClient, aibrixClient, false)
	t.Cleanup(func() {
		cleanupLifecycleResources(t, k8sClient, aibrixClient, true)
	})

	_, err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Create(
		ctx, lifecyclePoolDeployment(), metav1.CreateOptions{},
	)
	require.NoError(t, err)
	pod := waitForLifecyclePoolPod(t, ctx, k8sClient)
	createLifecycleClaim(t, ctx, aibrixClient, lifecycleBusyClaim, lifecycleBusyModel)
	createLifecycleClaim(t, ctx, aibrixClient, lifecycleIdleClaim, lifecycleIdleModel)
	waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleBusyClaim, modelv1alpha1.ModelClaimActive,
	)
	waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleIdleClaim, modelv1alpha1.ModelClaimActive,
	)
	waitForLifecycleModelStatus(t, lifecycleBusyModel, http.StatusOK, 30*time.Second)

	before, err := k8sClient.CoreV1().Pods(lifecycleNamespace).Get(
		ctx, pod.Name, metav1.GetOptions{},
	)
	require.NoError(t, err)
	busyRoute := before.Annotations[constants.ModelClaimPodAnnotationPrefix+lifecycleBusyClaim]
	require.NotEmpty(t, busyRoute)

	require.NoError(t, aibrixClient.ModelV1alpha1().ModelClaims(lifecycleNamespace).Delete(
		ctx, lifecycleIdleClaim, metav1.DeleteOptions{},
	))
	waitForLifecycleRouteAbsent(t, ctx, k8sClient, pod.Name, lifecycleIdleClaim)
	waitForLifecycleClaimDeleted(t, ctx, aibrixClient, lifecycleIdleClaim)
	waitForLifecycleModelStatus(t, lifecycleIdleModel, http.StatusBadRequest, 60*time.Second)

	busy := waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleBusyClaim, modelv1alpha1.ModelClaimActive,
	)
	assert.Equal(t, int32(1), busy.Status.ReadyReplicas)
	after, err := k8sClient.CoreV1().Pods(lifecycleNamespace).Get(
		ctx, pod.Name, metav1.GetOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, busyRoute, after.Annotations[constants.ModelClaimPodAnnotationPrefix+lifecycleBusyClaim])

	for range 3 {
		response, _, err := sendLifecycleModelRequest(lifecycleBusyModel)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, response.StatusCode)
		_ = response.Body.Close()
	}
}
