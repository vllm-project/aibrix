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
	"strconv"
	"testing"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	rayclientset "github.com/ray-project/kuberay/ray-operator/pkg/client/clientset/versioned"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixclientset "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	orchestrationclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/typed/orchestration/v1alpha1"
)

const (
	rayClusterE2EImage                 = "aibrix/inplace-e2e:v1"
	fleetFixtureLabel                  = "e2e.aibrix.ai/raycluster-fleet"
	rayOverwriteContainerCmdAnnotation = "ray.io/overwrite-container-cmd"
	rayClusterPollInterval             = 500 * time.Millisecond
	rayClusterPollTimeout              = 2 * time.Minute
	rayClusterCleanupTimeout           = 2 * time.Minute
	rayClusterControllerNamespace      = "aibrix-system"
	rayClusterControllerSelector       = "control-plane=controller-manager"
)

type rayClusterHarness struct {
	namespace        string
	kubeClient       kubernetes.Interface
	fleetClient      orchestrationclient.RayClusterFleetInterface
	replicaSetClient orchestrationclient.RayClusterReplicaSetInterface
	rayClient        rayclientset.Interface
}

func newRayClusterHarness(t *testing.T, ctx context.Context) *rayClusterHarness {
	t.Helper()
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatalf("build Kubernetes client configuration: %v", err)
	}
	config.QPS = 50
	config.Burst = 100
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	aibrixClient, err := aibrixclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("create AIBrix client: %v", err)
	}
	rayClient, err := rayclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("create KubeRay client: %v", err)
	}
	namespace := fixtureResourceName("raycluster-e2e")
	if _, err := kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create test namespace %s: %v", namespace, err)
	}
	harness := &rayClusterHarness{
		namespace:        namespace,
		kubeClient:       kubeClient,
		fleetClient:      aibrixClient.OrchestrationV1alpha1().RayClusterFleets(namespace),
		replicaSetClient: aibrixClient.OrchestrationV1alpha1().RayClusterReplicaSets(namespace),
		rayClient:        rayClient,
	}
	t.Cleanup(func() { harness.cleanup(t) })
	return harness
}

func (h *rayClusterHarness) createFleet(
	ctx context.Context,
	fleet *orchestrationv1alpha1.RayClusterFleet,
) (*orchestrationv1alpha1.RayClusterFleet, error) {
	return h.fleetClient.Create(ctx, fleet, metav1.CreateOptions{})
}

func (h *rayClusterHarness) updateFleet(
	ctx context.Context,
	name string,
	mutate func(*orchestrationv1alpha1.RayClusterFleet),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fleet, err := h.fleetClient.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		mutate(fleet)
		_, err = h.fleetClient.Update(ctx, fleet, metav1.UpdateOptions{})
		return err
	})
}

func (h *rayClusterHarness) listOwnedReplicaSets(
	ctx context.Context,
	fleetUID types.UID,
) ([]orchestrationv1alpha1.RayClusterReplicaSet, error) {
	list, err := h.replicaSetClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	owned := make([]orchestrationv1alpha1.RayClusterReplicaSet, 0)
	for i := range list.Items {
		uid, ok := controllerOwnerUID(&list.Items[i], "RayClusterFleet")
		if ok && uid == fleetUID {
			owned = append(owned, list.Items[i])
		}
	}
	return owned, nil
}

func (h *rayClusterHarness) listOwnedRayClusters(
	ctx context.Context,
	replicaSetUID types.UID,
) ([]rayv1.RayCluster, error) {
	list, err := h.rayClient.RayV1().RayClusters(h.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	owned := make([]rayv1.RayCluster, 0)
	for i := range list.Items {
		uid, ok := controllerOwnerUID(&list.Items[i], "RayClusterReplicaSet")
		if ok && uid == replicaSetUID {
			owned = append(owned, list.Items[i])
		}
	}
	return owned, nil
}

func (h *rayClusterHarness) waitForOwnedReplicaSets(
	ctx context.Context,
	fleetUID types.UID,
	expected int,
) ([]orchestrationv1alpha1.RayClusterReplicaSet, error) {
	var observed []orchestrationv1alpha1.RayClusterReplicaSet
	err := wait.PollUntilContextTimeout(ctx, rayClusterPollInterval, rayClusterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			items, err := h.listOwnedReplicaSets(ctx, fleetUID)
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			observed = items
			return len(items) == expected, nil
		})
	if err != nil {
		return nil, fmt.Errorf(
			"wait for %d ReplicaSets owned by Fleet %s (last=%d): %w",
			expected, fleetUID, len(observed), err,
		)
	}
	return observed, nil
}

func (h *rayClusterHarness) waitForOwnedRayClusters(
	ctx context.Context,
	replicaSetUID types.UID,
	expected int,
) ([]rayv1.RayCluster, error) {
	var observed []rayv1.RayCluster
	err := wait.PollUntilContextTimeout(ctx, rayClusterPollInterval, rayClusterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			items, err := h.listOwnedRayClusters(ctx, replicaSetUID)
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			observed = items
			return len(items) == expected, nil
		})
	if err != nil {
		return nil, fmt.Errorf(
			"wait for %d RayClusters owned by ReplicaSet %s (last=%d): %w",
			expected, replicaSetUID, len(observed), err,
		)
	}
	return observed, nil
}

func (h *rayClusterHarness) waitForRayClustersReady(ctx context.Context, clusters []rayv1.RayCluster) error {
	err := wait.PollUntilContextTimeout(ctx, rayClusterPollInterval, rayClusterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			for i := range clusters {
				current, err := h.rayClient.RayV1().RayClusters(h.namespace).Get(ctx, clusters[i].Name, metav1.GetOptions{})
				if err != nil {
					return false, retryableRayClusterAPIError(err)
				}
				if !rayClusterReady(current) || !h.headPodReady(ctx, current.UID) {
					return false, nil
				}
			}
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("wait for %d RayClusters to become Ready: %w", len(clusters), err)
	}
	return nil
}

func (h *rayClusterHarness) headPodReady(ctx context.Context, clusterUID types.UID) bool {
	pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false
	}
	for i := range pods.Items {
		uid, ok := controllerOwnerUID(&pods.Items[i], "RayCluster")
		if ok && uid == clusterUID && podReady(&pods.Items[i]) && pods.Items[i].Labels["ray.io/node-type"] == "head" {
			return true
		}
	}
	return false
}

func (h *rayClusterHarness) waitForReplicaSetStatus(
	ctx context.Context,
	name string,
	replicas, ready, available int32,
) error {
	err := wait.PollUntilContextTimeout(ctx, rayClusterPollInterval, rayClusterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			rs, err := h.replicaSetClient.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			return rs.Status.Replicas == replicas &&
				rs.Status.ReadyReplicas == ready && rs.Status.AvailableReplicas == available, nil
		})
	if err != nil {
		return fmt.Errorf(
			"wait for ReplicaSet %s status replicas=%d ready=%d available=%d: %w",
			name, replicas, ready, available, err,
		)
	}
	return nil
}

func (h *rayClusterHarness) waitForFleetStatus(
	ctx context.Context,
	name string,
	replicas, updated, ready, available int32,
) error {
	err := wait.PollUntilContextTimeout(ctx, rayClusterPollInterval, rayClusterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			fleet, err := h.fleetClient.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, retryableRayClusterAPIError(err)
			}
			status := fleet.Status
			return status.ObservedGeneration == fleet.Generation && status.Replicas == replicas &&
				status.UpdatedReplicas == updated && status.ReadyReplicas == ready && status.AvailableReplicas == available, nil
		})
	if err != nil {
		return fmt.Errorf(
			"wait for Fleet %s status replicas=%d updated=%d ready=%d available=%d: %w",
			name, replicas, updated, ready, available, err,
		)
	}
	return nil
}

func (h *rayClusterHarness) ensureNoChildren(ctx context.Context, fleetUID types.UID, duration time.Duration) error {
	deadline := time.Now().Add(duration)
	ticker := time.NewTicker(rayClusterPollInterval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		rss, err := h.listOwnedReplicaSets(ctx, fleetUID)
		if err != nil {
			if retryableRayClusterAPIError(err) != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
			continue
		}
		if len(rss) != 0 {
			return fmt.Errorf("Fleet %s created %d ReplicaSets while paused", fleetUID, len(rss))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func (h *rayClusterHarness) setDeletionCost(ctx context.Context, cluster *rayv1.RayCluster, cost string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := h.rayClient.RayV1().RayClusters(h.namespace).Get(ctx, cluster.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["controller.kubernetes.io/pod-deletion-cost"] = cost
		_, err = h.rayClient.RayV1().RayClusters(h.namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}

func retryableRayClusterAPIError(err error) error {
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

func podReady(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func newRayClusterFleet(namespace, name string, replicas int32, paused bool) *orchestrationv1alpha1.RayClusterFleet {
	labels := map[string]string{fleetFixtureLabel: name}
	return &orchestrationv1alpha1.RayClusterFleet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: orchestrationv1alpha1.GroupVersion.String(),
			Kind:       "RayClusterFleet",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: orchestrationv1alpha1.RayClusterFleetSpec{
			Replicas: ptr.To(replicas),
			Paused:   paused,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To(intstr.FromInt(0)),
					MaxSurge:       ptr.To(intstr.FromInt(1)),
				},
			},
			Template: orchestrationv1alpha1.RayClusterTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{rayOverwriteContainerCmdAnnotation: "true"},
				},
				Spec: rayv1.RayClusterSpec{
					RayVersion: "fake-ray-version",
					HeadGroupSpec: rayv1.HeadGroupSpec{
						RayStartParams: map[string]string{"dashboard-host": "0.0.0.0"},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: labels},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:            "ray-head",
									Image:           rayClusterE2EImage,
									ImagePullPolicy: corev1.PullIfNotPresent,
									Command:         []string{"sh", "-c", "sleep 3600"},
									Ports: []corev1.ContainerPort{
										{Name: "gcs-server", ContainerPort: 6379},
										{Name: "dashboard", ContainerPort: 8265},
										{Name: "client", ContainerPort: 10001},
									},
								}},
							},
						},
					},
				},
			},
			ProgressDeadlineSeconds: ptr.To(int32(600)),
			RevisionHistoryLimit:    ptr.To(int32(10)),
		},
	}
}

func rayClusterReady(cluster *rayv1.RayCluster) bool {
	if cluster == nil {
		return false
	}
	var provisioned, headReady bool
	for _, condition := range cluster.Status.Conditions {
		if condition.Status != metav1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case string(rayv1.RayClusterProvisioned):
			provisioned = true
		case string(rayv1.HeadPodReady):
			headReady = true
		}
	}
	return provisioned && headReady
}

func controllerOwnerUID(object metav1.Object, kind string) (types.UID, bool) {
	if object == nil {
		return "", false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.Controller != nil && *owner.Controller && owner.Kind == kind && owner.UID != "" {
			return owner.UID, true
		}
	}
	return "", false
}

func fixtureResourceName(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func describeOwner(object metav1.Object) string {
	if object == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s uid=%s", object.GetNamespace(), object.GetName(), object.GetUID())
}
