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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	modelclaimcontroller "github.com/vllm-project/aibrix/pkg/controller/modelclaim"
)

func TestModelClaimPriorityControlsPodEventPlacementOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	k8sClient, aibrixClient := initializeClient(ctx, t)
	name := "modelclaim-priority-" + rand.String(8)
	lowName, highName := name+"-a-low", name+"-z-high"
	claims := aibrixClient.ModelV1alpha1().ModelClaims(lifecycleNamespace)
	deployments := k8sClient.AppsV1().Deployments(lifecycleNamespace)
	deployment := runtimeOnlyPoolDeployment()
	deployment.Name, deployment.Annotations = name, nil
	deployment.Spec.Replicas = ptr.To[int32](0)
	deployment.Spec.Selector.MatchLabels["app"] = name
	deployment.Spec.Template.Labels["app"] = name
	deployment.Spec.Template.Labels[constants.ModelPoolLabelName] = name
	_, err := deployments.Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cleanupCancel()
		for _, claimName := range []string{lowName, highName} {
			err := claims.Delete(cleanupCtx, claimName, metav1.DeleteOptions{})
			if !apierrors.IsNotFound(err) {
				assert.NoError(t, err)
			}
		}
		assert.NoError(t, wait.PollUntilContextTimeout(cleanupCtx, 200*time.Millisecond, 45*time.Second, true,
			func(ctx context.Context) (bool, error) {
				for _, claimName := range []string{lowName, highName} {
					_, err := claims.Get(ctx, claimName, metav1.GetOptions{})
					if !apierrors.IsNotFound(err) {
						return false, err
					}
				}
				return true, nil
			}), "priority claims were not cleaned up")
		err := deployments.Delete(cleanupCtx, name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			assert.NoError(t, err)
		}
		assert.NoError(t, wait.PollUntilContextTimeout(cleanupCtx, 200*time.Millisecond, 40*time.Second, true,
			func(ctx context.Context) (bool, error) {
				_, err := deployments.Get(ctx, name, metav1.GetOptions{})
				if !apierrors.IsNotFound(err) {
					return false, err
				}
				pods, err := k8sClient.CoreV1().Pods(lifecycleNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + name})
				return err == nil && len(pods.Items) == 0, err
			}), "priority deployment and pods were not cleaned up")
	})

	for _, spec := range []struct {
		name     string
		priority int32
	}{{lowName, 1}, {highName, 10}} {
		_, err := claims.Create(ctx, &modelv1alpha1.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Name: spec.name, Namespace: lifecycleNamespace},
			Spec: modelv1alpha1.ModelClaimSpec{
				Priority:    spec.priority,
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{constants.ModelPoolLabelName: name}},
				ArtifactURL: "huggingface://aibrix/" + spec.name,
				Engine:      "vllm",
				PerGPU: &modelv1alpha1.ModelClaimPerGPU{
					MaximumFootprint: resource.MustParse("1Gi"),
					KVFloor:          resource.MustParse("1Gi"),
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	var low *modelv1alpha1.ModelClaim
	var firstRetry time.Time
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			for _, claimName := range []string{lowName, highName} {
				claim, err := claims.Get(ctx, claimName, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				condition := meta.FindStatusCondition(
					claim.Status.Conditions, string(modelv1alpha1.ModelClaimConditionTypeScheduled),
				)
				if condition == nil || condition.Reason != "NoMatchingPods" || len(claim.Status.Instances) != 0 {
					return false, nil
				}
				due := condition.LastTransitionTime.Add(modelclaimcontroller.DefaultRequeueDuration)
				if firstRetry.IsZero() || due.Before(firstRetry) {
					firstRetry = due
				}
				if claimName == lowName {
					low = claim
				}
			}
			return true, nil
		}), "both claims must be waiting before a pod joins")
	watcher, err := claims.Watch(ctx, metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("metadata.name", lowName).String(),
		ResourceVersion: low.ResourceVersion,
	})
	require.NoError(t, err)
	defer watcher.Stop()

	// Scaling before the ordinary retry exercises the older-first Pod watch
	// queue. The watch retains even a short-lived placement-turn condition.
	require.True(t, time.Now().Before(firstRetry), "setup missed the initial placement-backoff window")
	scaleCtx, scaleCancel := context.WithDeadline(ctx, firstRetry)
	defer scaleCancel()
	scale, err := deployments.GetScale(scaleCtx, name, metav1.GetOptions{})
	require.NoError(t, err)
	scale.Spec.Replicas = 1
	_, err = deployments.UpdateScale(scaleCtx, name, scale, metav1.UpdateOptions{})
	require.NoError(t, err, "pool must scale before placement backoff is due")

	_, err = watchtools.UntilWithoutRetry(ctx, watcher, func(event watch.Event) (bool, error) {
		if event.Type == watch.Error {
			return false, apierrors.FromObject(event.Object)
		}
		claim, ok := event.Object.(*modelv1alpha1.ModelClaim)
		if !ok {
			return false, fmt.Errorf("unexpected priority watch object %T", event.Object)
		}
		condition := meta.FindStatusCondition(claim.Status.Conditions, string(modelv1alpha1.ModelClaimConditionTypeScheduled))
		if condition != nil && condition.Reason == "WaitingForPlacementTurn" {
			assert.Equal(t, metav1.ConditionFalse, condition.Status)
			assert.Equal(t,
				fmt.Sprintf("waiting behind ModelClaim %s with priority 10 that can use the same pool", highName),
				condition.Message,
			)
			return true, nil
		}
		if claim.Status.Phase == modelv1alpha1.ModelClaimActive {
			return false, fmt.Errorf("low-priority claim became Active without observing WaitingForPlacementTurn")
		}
		return false, nil
	})
	require.NoError(t, err, "older low-priority claim must yield its placement turn")
	for _, claimName := range []string{highName, lowName} {
		waitForLifecycleClaimPhase(t, ctx, aibrixClient, claimName, modelv1alpha1.ModelClaimActive)
	}
}
