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
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	aibrixclientset "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
)

const (
	scheduledBoundsE2EEnabledEnv = "AIBRIX_SCHEDULED_BOUNDS_E2E"
	scheduledBoundsTargetName    = "scheduled-bounds-scale-target"
	scheduledBoundsPAName        = "scheduled-bounds-e2e"
	scheduledBoundsNamespace     = "default"
)

func TestScheduledBoundsAutoscalerAppliesEffectiveBoundsToHPA(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(envOrDefault(scheduledBoundsE2EEnabledEnv, "false"))) != e2eEnabledValue {
		t.Skip("set AIBRIX_SCHEDULED_BOUNDS_E2E=true to run the scheduled bounds e2e test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	k8sClient, aibrixClient := scheduledBoundsClients(t)
	cleanupScheduledBoundsE2E(ctx, t, k8sClient, aibrixClient)
	defer cleanupScheduledBoundsE2E(context.Background(), t, k8sClient, aibrixClient)

	createScheduledBoundsScaleTarget(ctx, t, k8sClient)
	createScheduledBoundsPodAutoscaler(ctx, t, aibrixClient, time.Now().UTC())
	waitForScheduledBoundsHPA(ctx, t, k8sClient, 4, 7)
}

func scheduledBoundsClients(t *testing.T) (*kubernetes.Clientset, *aibrixclientset.Clientset) {
	t.Helper()

	config, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		t.Fatalf("failed to load kube config: %v", err)
	}
	restConfig, err := clientcmd.NewDefaultClientConfig(*config, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("failed to build kube config: %v", err)
	}
	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("failed to create Kubernetes client: %v", err)
	}
	aibrixClient, err := aibrixclientset.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("failed to create AIBrix client: %v", err)
	}
	return k8sClient, aibrixClient
}

func createScheduledBoundsScaleTarget(ctx context.Context, t *testing.T, k8sClient kubernetes.Interface) {
	t.Helper()

	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scheduledBoundsTargetName,
			Namespace: scheduledBoundsNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": scheduledBoundsTargetName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": scheduledBoundsTargetName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "target",
						Image:           "busybox:1.36",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sh", "-c", "sleep 3600"},
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: 8080,
						}},
					}},
				},
			},
		},
	}

	_, err := k8sClient.AppsV1().
		Deployments(scheduledBoundsNamespace).
		Create(ctx, deployment, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, getErr := k8sClient.AppsV1().
			Deployments(scheduledBoundsNamespace).
			Get(ctx, scheduledBoundsTargetName, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("failed to get existing scale target deployment: %v", getErr)
		}
		deployment.ResourceVersion = current.ResourceVersion
		_, err = k8sClient.AppsV1().
			Deployments(scheduledBoundsNamespace).
			Update(ctx, deployment, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("failed to create scale target deployment: %v", err)
	}
}

func createScheduledBoundsPodAutoscaler(
	ctx context.Context,
	t *testing.T,
	aibrixClient aibrixclientset.Interface,
	now time.Time,
) {
	t.Helper()

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scheduledBoundsPAName,
			Namespace: scheduledBoundsNamespace,
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       scheduledBoundsTargetName,
			},
			MinReplicas: ptr.To[int32](1),
			MaxReplicas: 3,
			ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name:        "active-e2e-window",
				Timezone:    "UTC",
				Cron:        fmt.Sprintf("%d %d * * *", now.Minute(), now.Hour()),
				Duration:    metav1.Duration{Duration: 2 * time.Hour},
				MinReplicas: ptr.To[int32](4),
				MaxReplicas: ptr.To[int32](7),
			}},
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     autoscalingv1alpha1.CPU,
				TargetValue:      "50",
			}},
			ScalingStrategy: autoscalingv1alpha1.HPA,
		},
	}

	_, err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(scheduledBoundsNamespace).
		Create(ctx, pa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, getErr := aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(scheduledBoundsNamespace).
			Get(ctx, pa.Name, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("failed to get existing PodAutoscaler: %v", getErr)
		}
		pa.ResourceVersion = current.ResourceVersion
		_, err = aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(scheduledBoundsNamespace).
			Update(ctx, pa, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("failed to create PodAutoscaler: %v", err)
	}
}

func waitForScheduledBoundsHPA(
	ctx context.Context,
	t *testing.T,
	k8sClient kubernetes.Interface,
	wantMin, wantMax int32,
) {
	t.Helper()

	hpaName := fmt.Sprintf("%s-hpa", scheduledBoundsPAName)
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		hpa, err := k8sClient.AutoscalingV2().
			HorizontalPodAutoscalers(scheduledBoundsNamespace).
			Get(ctx, hpaName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			t.Logf("waiting for generated HPA %s/%s", scheduledBoundsNamespace, hpaName)
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if hpa.Spec.MinReplicas != nil && *hpa.Spec.MinReplicas == wantMin && hpa.Spec.MaxReplicas == wantMax {
			return true, nil
		}
		t.Logf("waiting for scheduled HPA bounds, got min=%s max=%d", hpaMinReplicas(hpa), hpa.Spec.MaxReplicas)
		return false, nil
	})
	if err != nil {
		t.Fatalf("generated HPA did not use scheduled bounds min=%d max=%d: %v", wantMin, wantMax, err)
	}
}

func hpaMinReplicas(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	if hpa.Spec.MinReplicas == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *hpa.Spec.MinReplicas)
}

func cleanupScheduledBoundsE2E(
	ctx context.Context,
	t *testing.T,
	k8sClient kubernetes.Interface,
	aibrixClient aibrixclientset.Interface,
) {
	t.Helper()

	err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(scheduledBoundsNamespace).
		Delete(ctx, scheduledBoundsPAName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete PodAutoscaler: %v", err)
	}

	err = k8sClient.AutoscalingV2().
		HorizontalPodAutoscalers(scheduledBoundsNamespace).
		Delete(ctx, fmt.Sprintf("%s-hpa", scheduledBoundsPAName), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete HPA: %v", err)
	}

	err = k8sClient.AppsV1().
		Deployments(scheduledBoundsNamespace).
		Delete(ctx, scheduledBoundsTargetName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete scale target deployment: %v", err)
	}
}
