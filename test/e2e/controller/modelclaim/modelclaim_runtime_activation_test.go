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
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	modelclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	runtimeOnlyDeployment = "modelclaim-runtime-only-pool"
	runtimeOnlyPool       = "modelclaim-runtime-only"
	runtimeOnlyClaim      = "runtime-only-claim"
	runtimeOnlyModel      = "runtime-only-model"
)

func TestModelClaimDrivesRuntimeMockActivationAndDeactivation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	k8sClient, aibrixClient := initializeClient(ctx, t)
	cleanupRuntimeOnlyResources(t, k8sClient, aibrixClient, false)
	t.Cleanup(func() {
		cleanupRuntimeOnlyResources(t, k8sClient, aibrixClient, true)
	})

	_, err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Create(
		ctx, runtimeOnlyPoolDeployment(), metav1.CreateOptions{},
	)
	require.NoError(t, err)
	pod := waitForLifecyclePoolPodWithApp(t, ctx, k8sClient, runtimeOnlyDeployment)
	claim := createLifecycleClaimInPool(
		t, ctx, aibrixClient, runtimeOnlyClaim, runtimeOnlyModel, runtimeOnlyPool,
	)
	waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, runtimeOnlyClaim, modelv1alpha1.ModelClaimActive,
	)

	var last lifecycleRuntimeSnapshot
	err = wait.PollUntilContextTimeout(ctx, time.Second, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			raw, err := k8sClient.CoreV1().Pods(lifecycleNamespace).ProxyGet(
				"http", pod.Name, fmt.Sprint(lifecycleRuntimePort), "v1/runtime/snapshot", nil,
			).DoRaw(ctx)
			if err != nil {
				return false, err
			}
			if err := json.Unmarshal(raw, &last); err != nil {
				return false, err
			}
			for _, observed := range last.Models {
				if observed.ModelName != runtimeOnlyModel || observed.ClaimRef == nil {
					continue
				}
				return observed.ClaimRef.Namespace == lifecycleNamespace &&
					observed.ClaimRef.Name == runtimeOnlyClaim &&
					observed.ClaimRef.UID == string(claim.UID), nil
			}
			return false, nil
		})
	require.NoError(t, err, "runtime never recorded claim activation: %+v", last.Models)

	require.NoError(t, aibrixClient.ModelV1alpha1().ModelClaims(lifecycleNamespace).Delete(
		ctx, runtimeOnlyClaim, metav1.DeleteOptions{},
	))
	waitForLifecycleRuntimeModelAbsent(t, ctx, k8sClient, pod.Name, runtimeOnlyModel)
	waitForLifecycleClaimDeleted(t, ctx, aibrixClient, runtimeOnlyClaim)
}

func runtimeOnlyPoolDeployment() *appsv1.Deployment {
	deployment := lifecyclePoolDeployment()
	deployment.Name = runtimeOnlyDeployment
	deployment.Spec.Selector.MatchLabels["app"] = runtimeOnlyDeployment
	deployment.Spec.Template.Labels["app"] = runtimeOnlyDeployment
	deployment.Spec.Template.Labels[constants.ModelPoolLabelName] = runtimeOnlyPool
	runtimeContainer := lifecycleRuntimeContainer()
	for i := range runtimeContainer.Env {
		if runtimeContainer.Env[i].Name == "AIBRIX_MODEL_RUNTIME_MOCK_EXTERNAL_ENGINES" {
			runtimeContainer.Env[i].Value = "0"
		}
	}
	deployment.Spec.Template.Spec.Containers = []corev1.Container{runtimeContainer}
	return deployment
}

func TestRuntimeOnlyPoolDeploymentUsesTheInProcessMockLauncher(t *testing.T) {
	deployment := runtimeOnlyPoolDeployment()

	assert.Equal(t, runtimeOnlyDeployment, deployment.Name)
	assert.Equal(t, runtimeOnlyPool, deployment.Spec.Template.Labels[constants.ModelPoolLabelName])
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	runtimeContainer := deployment.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "aibrix-runtime", runtimeContainer.Name)
	for _, env := range runtimeContainer.Env {
		if env.Name == "AIBRIX_MODEL_RUNTIME_MOCK_EXTERNAL_ENGINES" {
			assert.Equal(t, "0", env.Value)
			return
		}
	}
	t.Fatal("runtime-only pool does not configure AIBRIX_MODEL_RUNTIME_MOCK_EXTERNAL_ENGINES")
}

func cleanupRuntimeOnlyResources(
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	aibrixClient *modelclient.Clientset,
	requireComplete bool,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	claims := aibrixClient.ModelV1alpha1().ModelClaims(lifecycleNamespace)
	err := claims.Delete(ctx, runtimeOnlyClaim, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		if requireComplete {
			require.NoError(t, err)
		}
		t.Logf("delete runtime-only ModelClaim: %v", err)
	}
	claimErr := wait.PollUntilContextTimeout(ctx, time.Second, 20*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := claims.Get(ctx, runtimeOnlyClaim, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		})
	if requireComplete {
		require.NoError(t, claimErr, "runtime-only ModelClaim was not deleted")
	} else if claimErr != nil {
		t.Logf("wait for stale runtime-only ModelClaim: %v", claimErr)
	}

	err = k8sClient.AppsV1().Deployments(lifecycleNamespace).Delete(
		ctx, runtimeOnlyDeployment, metav1.DeleteOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		if requireComplete {
			require.NoError(t, err)
		}
		t.Logf("delete runtime-only Deployment: %v", err)
	}
	deploymentErr := wait.PollUntilContextTimeout(ctx, time.Second, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Get(
				ctx, runtimeOnlyDeployment, metav1.GetOptions{},
			)
			return apierrors.IsNotFound(err), nil
		})
	if requireComplete {
		require.NoError(t, deploymentErr, "runtime-only Deployment was not deleted")
	} else if deploymentErr != nil {
		t.Logf("wait for stale runtime-only Deployment: %v", deploymentErr)
	}

	var lastPods []corev1.Pod
	podsErr := wait.PollUntilContextTimeout(ctx, time.Second, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			pods, err := k8sClient.CoreV1().Pods(lifecycleNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + runtimeOnlyDeployment,
			})
			if err != nil {
				return false, err
			}
			lastPods = pods.Items
			return len(pods.Items) == 0, nil
		})
	if requireComplete {
		require.NoError(t, podsErr, "runtime-only Pods remain after cleanup: %+v", lastPods)
		assert.Empty(t, lastPods)
	} else if podsErr != nil {
		t.Logf("wait for stale runtime-only Pods: %v", podsErr)
	}
}
