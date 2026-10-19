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
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// TestModelClaimSleepLowerPriorityPreparedPool exercises soft preemption on a
// prepared card-backed runtime pool. The pool must have one card that fits two
// 2 GiB claims but not three, keep no wake reserve, and use a short ordinary
// make-room threshold so the sleeping victim can be woken at the end.
func TestModelClaimSleepLowerPriorityPreparedPool(t *testing.T) {
	pool := os.Getenv("AIBRIX_E2E_PREEMPTION_POOL")
	if pool == "" {
		t.Skip("set AIBRIX_E2E_PREEMPTION_POOL to a prepared card-backed pool " +
			"with noWakeReserveWhileAsleep=true and sleepToMakeRoomAfterSeconds=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	k8sClient, aibrixClient := initializeClient(ctx, t)
	claims := aibrixClient.ModelV1alpha1().ModelClaims(lifecycleNamespace)
	prefix := "preemption-" + rand.String(8)
	lowName, equalName, highName := prefix+"-low", prefix+"-equal", prefix+"-high"
	created := []string{lowName, equalName, highName}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cleanupCancel()
		for _, name := range created {
			err := claims.Delete(cleanupCtx, name, metav1.DeleteOptions{})
			if !apierrors.IsNotFound(err) {
				assert.NoError(t, err)
			}
		}
	})

	create := func(
		name string,
		priority int32,
		policy modelv1alpha1.ModelClaimPreemptionPolicy,
	) *modelv1alpha1.ModelClaim {
		claim, err := claims.Create(ctx, &modelv1alpha1.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lifecycleNamespace},
			Spec: modelv1alpha1.ModelClaimSpec{
				Priority: priority, PreemptionPolicy: policy,
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					constants.ModelPoolLabelName: pool,
				}},
				ArtifactURL: "huggingface://aibrix/" + name,
				Engine:      "vllm",
				PerGPU: &modelv1alpha1.ModelClaimPerGPU{
					MaximumFootprint: resource.MustParse("1Gi"),
					KVFloor:          resource.MustParse("1Gi"),
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		return claim
	}

	low := create(lowName, 1, modelv1alpha1.ModelClaimPreemptionNever)
	equal := create(equalName, 10, modelv1alpha1.ModelClaimPreemptionNever)
	waitForLifecycleClaimPhase(t, ctx, aibrixClient, low.Name, modelv1alpha1.ModelClaimActive)
	waitForLifecycleClaimPhase(t, ctx, aibrixClient, equal.Name, modelv1alpha1.ModelClaimActive)
	high := create(highName, 10, modelv1alpha1.ModelClaimPreemptionSleepLowerPriority)
	high = waitForLifecycleClaimPhase(t, ctx, aibrixClient, high.Name, modelv1alpha1.ModelClaimActive)
	low = waitForLifecycleClaimPhase(t, ctx, aibrixClient, low.Name, modelv1alpha1.ModelClaimSleeping)
	equal, err := claims.Get(ctx, equal.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, equal.Status.Phase, "equal priority is protected")
	require.Len(t, low.Status.Instances, 1)
	require.Len(t, high.Status.Instances, 1)
	assert.Equal(t, low.Status.Instances[0].Pod, high.Status.Instances[0].Pod)
	assertLifecycleRouteBinding(t, ctx, k8sClient, low.Status.Instances[0].Pod, low.Name,
		lifecycleRouteBinding{Model: low.Name, Port: 0, State: constants.ModelClaimRoutingStateSleeping})

	// Normally the gateway reports the victim as sleeping and writes the wake
	// request. A controller-only upgrade can ask the test to write the same Pod
	// annotation directly. Once preemption's reservation is gone, existing wake
	// admission makes room and returns the victim to Active.
	if os.Getenv("AIBRIX_E2E_PREEMPTION_SKIP_GATEWAY") == "1" {
		pod, err := k8sClient.CoreV1().Pods(lifecycleNamespace).Get(
			ctx, low.Status.Instances[0].Pod, metav1.GetOptions{},
		)
		require.NoError(t, err)
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+low.Name] = time.Now().UTC().Format(time.RFC3339)
		_, err = k8sClient.CoreV1().Pods(lifecycleNamespace).Update(ctx, pod, metav1.UpdateOptions{})
		require.NoError(t, err)
	} else {
		var lastStatus int
		var lastBody, retryAfter string
		require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true,
			func(context.Context) (bool, error) {
				response, body, err := sendLifecycleModelRequest(low.Name)
				if err != nil {
					lastBody = err.Error()
					return false, nil
				}
				lastStatus, lastBody = response.StatusCode, string(body)
				retryAfter = response.Header.Get("Retry-After")
				_ = response.Body.Close()
				return lastStatus == http.StatusServiceUnavailable && retryAfter == "3", nil
			}), "gateway did not observe sleeping route for %s; last status=%d retry-after=%s body=%s",
			low.Name, lastStatus, retryAfter, lastBody)
	}
	recovered := waitForLifecycleClaimPhase(t, ctx, aibrixClient, low.Name, modelv1alpha1.ModelClaimActive)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			pod, err := k8sClient.CoreV1().Pods(lifecycleNamespace).Get(
				ctx, recovered.Status.Instances[0].Pod, metav1.GetOptions{},
			)
			if err != nil {
				return false, err
			}
			_, waiting := pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+low.Name]
			return !waiting, nil
		}))
}
