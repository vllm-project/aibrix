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
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

const rayClusterKeepOnFailureEnv = "AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE"

const rayClusterOperatorContainer = "kuberay-operator"

func (h *rayClusterHarness) cleanup(t *testing.T) {
	t.Helper()
	if t.Failed() {
		h.logDiagnostics(t)
		if strings.EqualFold(strings.TrimSpace(os.Getenv(rayClusterKeepOnFailureEnv)), "true") {
			t.Logf("preserving RayCluster E2E namespace %s after failure", h.namespace)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), rayClusterCleanupTimeout)
	defer cancel()
	propagation := metav1.DeletePropagationForeground
	err := h.kubeClient.CoreV1().Namespaces().Delete(
		ctx,
		h.namespace,
		metav1.DeleteOptions{PropagationPolicy: &propagation},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete RayCluster E2E namespace %s: %v", h.namespace, err)
		return
	}
	if err := wait.PollUntilContextTimeout(ctx, rayClusterPollInterval, rayClusterCleanupTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := h.kubeClient.CoreV1().Namespaces().Get(ctx, h.namespace, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}); err != nil {
		t.Errorf("wait for RayCluster E2E namespace %s deletion: %v", h.namespace, err)
	}
}

func (h *rayClusterHarness) logDiagnostics(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if fleets, err := h.fleetClient.List(ctx, metav1.ListOptions{}); err != nil {
		t.Logf("list Fleets in %s: %v", h.namespace, err)
	} else {
		t.Logf("Fleets in %s: %+v", h.namespace, fleets.Items)
	}
	if replicaSets, err := h.replicaSetClient.List(ctx, metav1.ListOptions{}); err != nil {
		t.Logf("list ReplicaSets in %s: %v", h.namespace, err)
	} else {
		t.Logf("ReplicaSets in %s: %+v", h.namespace, replicaSets.Items)
	}
	if clusters, err := h.rayClient.RayV1().RayClusters(h.namespace).List(ctx, metav1.ListOptions{}); err != nil {
		t.Logf("list RayClusters in %s: %v", h.namespace, err)
	} else {
		t.Logf("RayClusters in %s: %+v", h.namespace, clusters.Items)
	}
	if pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{}); err != nil {
		t.Logf("list Pods in %s: %v", h.namespace, err)
	} else {
		t.Logf("Pods in %s: %+v", h.namespace, pods.Items)
	}
	if events, err := h.kubeClient.CoreV1().Events(h.namespace).List(ctx, metav1.ListOptions{}); err != nil {
		t.Logf("list Events in %s: %v", h.namespace, err)
	} else {
		t.Logf("Events in %s: %+v", h.namespace, events.Items)
	}
	h.logDeploymentLogs(t, ctx, rayClusterControllerSelector, "AIBrix controller")
	h.logDeploymentLogs(t, ctx, "app.kubernetes.io/component=kuberay-operator", "KubeRay operator")
}

func (h *rayClusterHarness) logDeploymentLogs(t *testing.T, ctx context.Context, selector, component string) {
	t.Helper()
	pods, err := h.kubeClient.CoreV1().Pods(rayClusterControllerNamespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: selector},
	)
	if err != nil {
		t.Logf("list %s Pods: %v", component, err)
		return
	}
	for i := range pods.Items {
		container := "manager"
		if component == "KubeRay operator" {
			container = rayClusterOperatorContainer
		}
		logs, err := h.kubeClient.CoreV1().Pods(rayClusterControllerNamespace).
			GetLogs(pods.Items[i].Name, &corev1.PodLogOptions{Container: container, TailLines: ptr.To(int64(200))}).
			DoRaw(ctx)
		if err != nil {
			t.Logf("get %s logs from %s: %v", component, pods.Items[i].Name, err)
			continue
		}
		t.Logf("%s logs from %s:\n%s", component, pods.Items[i].Name, logs)
	}
}

func (h *rayClusterHarness) restartController(ctx context.Context, t *testing.T) error {
	t.Helper()
	deployments := h.kubeClient.AppsV1().Deployments(rayClusterControllerNamespace)
	deploymentList, err := deployments.List(ctx, metav1.ListOptions{LabelSelector: rayClusterControllerSelector})
	if err != nil {
		return fmt.Errorf("list AIBrix controller Deployments: %w", err)
	}
	if len(deploymentList.Items) != 1 {
		return fmt.Errorf("expected one AIBrix controller Deployment, got %d", len(deploymentList.Items))
	}
	deployment := deploymentList.Items[0]
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return fmt.Errorf("build AIBrix controller Pod selector: %w", err)
	}
	podsClient := h.kubeClient.CoreV1().Pods(rayClusterControllerNamespace)
	oldPods, err := podsClient.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return fmt.Errorf("list AIBrix controller Pods: %w", err)
	}
	if len(oldPods.Items) == 0 {
		return fmt.Errorf("AIBrix controller Deployment has no Pods")
	}
	oldUIDs := make(map[types.UID]struct{}, len(oldPods.Items))
	for i := range oldPods.Items {
		oldUIDs[oldPods.Items[i].UID] = struct{}{}
		if err := podsClient.Delete(ctx, oldPods.Items[i].Name, metav1.DeleteOptions{}); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("delete AIBrix controller Pod %s: %w", oldPods.Items[i].Name, err)
		}
	}
	err = wait.PollUntilContextTimeout(ctx, rayClusterPollInterval, rayClusterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			currentDeployment, err := deployments.Get(ctx, deployment.Name, metav1.GetOptions{})
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			if currentDeployment.Status.AvailableReplicas < 1 {
				return false, nil
			}
			currentPods, err := podsClient.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			for i := range currentPods.Items {
				if _, old := oldUIDs[currentPods.Items[i].UID]; old {
					return false, nil
				}
				if podReady(&currentPods.Items[i]) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("wait for AIBrix controller restart: %w", err)
	}
	return nil
}

func (h *rayClusterHarness) deleteFleetForeground(ctx context.Context, name string) (types.UID, error) {
	fleet, err := h.fleetClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	uid := fleet.UID
	propagation := metav1.DeletePropagationForeground
	if err := h.fleetClient.Delete(
		ctx,
		name,
		metav1.DeleteOptions{PropagationPolicy: &propagation},
	); err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	return uid, nil
}

func (h *rayClusterHarness) waitForFleetResourcesGone(
	ctx context.Context,
	fleetName string,
	fleetUID types.UID,
	replicaSetUIDs ...types.UID,
) error {
	ownedReplicaSetUIDs := make(map[types.UID]struct{}, len(replicaSetUIDs))
	for _, uid := range replicaSetUIDs {
		ownedReplicaSetUIDs[uid] = struct{}{}
	}
	err := wait.PollUntilContextTimeout(ctx, rayClusterPollInterval, rayClusterCleanupTimeout, true,
		func(ctx context.Context) (bool, error) {
			if _, err := h.fleetClient.Get(ctx, fleetName, metav1.GetOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return false, retryableRayClusterAPIError(err)
			} else if err == nil {
				return false, nil
			}
			rss, err := h.listOwnedReplicaSets(ctx, fleetUID)
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			if len(rss) != 0 {
				return false, nil
			}
			allReplicaSets, err := h.replicaSetClient.List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			for i := range allReplicaSets.Items {
				if allReplicaSets.Items[i].Labels[fleetFixtureLabel] == fleetName {
					return false, nil
				}
			}
			clusters, err := h.rayClient.RayV1().RayClusters(h.namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			for i := range clusters.Items {
				if clusters.Items[i].Labels[fleetFixtureLabel] == fleetName {
					return false, nil
				}
				if uid, ok := controllerOwnerUID(&clusters.Items[i], "RayClusterReplicaSet"); ok {
					if _, tracked := ownedReplicaSetUIDs[uid]; !tracked {
						continue
					}
					return false, nil
				}
			}
			pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fleetFixtureLabel + "=" + fleetName,
			})
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			return len(pods.Items) == 0, nil
		})
	if err != nil {
		return fmt.Errorf("wait for Fleet %s descendants to disappear: %w", fleetUID, err)
	}
	return nil
}
