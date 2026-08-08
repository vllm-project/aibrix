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
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	aibrixclientset "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	patypes "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

const (
	circuitBreakerE2ENamespace  = "default"
	circuitBreakerInvalidMetric = "aibrix_test_missing_metric"
)

func TestAutoscalerCircuitBreakerE2E(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(envOrDefault("AIBRIX_AUTOSCALER_CIRCUIT_BREAKER_E2E", "false"))) != "true" {
		t.Skip("set AIBRIX_AUTOSCALER_CIRCUIT_BREAKER_E2E=true to run the autoscaler circuit breaker e2e test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	k8sClient, aibrixClient := externalMetricsClients(t)
	cleanupCircuitBreakerE2E(ctx, t, k8sClient, aibrixClient)
	defer cleanupCircuitBreakerE2E(context.Background(), t, k8sClient, aibrixClient)

	assertExternalMetricAPI(ctx, t, k8sClient)

	createCircuitBreakerTarget(ctx, t, k8sClient, "circuit-breaker-freeze-target", 2)
	createCircuitBreakerTarget(ctx, t, k8sClient, "circuit-breaker-max-target", 1)

	createCircuitBreakerPA(ctx, t, aibrixClient, circuitBreakerPAConfig{
		name:    "circuit-breaker-freeze-e2e",
		target:  "circuit-breaker-freeze-target",
		action:  autoscalingv1alpha1.CircuitBreakerActionFreeze,
		metrics: []autoscalingv1alpha1.MetricSource{validExternalMetric("1"), invalidExternalMetric()},
	})
	waitForDesiredAtLeast(ctx, t, k8sClient, "circuit-breaker-freeze-target", 2)

	updateCircuitBreakerPAMetrics(ctx, t, aibrixClient, "circuit-breaker-freeze-e2e", []autoscalingv1alpha1.MetricSource{
		validExternalMetric("1000000"),
		invalidExternalMetric(),
	})
	waitForReplicas(ctx, t, k8sClient, "circuit-breaker-freeze-target", 4)

	updateCircuitBreakerPAMetrics(ctx, t, aibrixClient, "circuit-breaker-freeze-e2e", []autoscalingv1alpha1.MetricSource{
		invalidExternalMetric(),
	})
	waitForCircuitBreakerState(
		ctx,
		t,
		aibrixClient,
		"circuit-breaker-freeze-e2e",
		autoscalingv1alpha1.CircuitBreakerStateOpen,
	)
	waitForReplicas(ctx, t, k8sClient, "circuit-breaker-freeze-target", 4)

	updateCircuitBreakerPAMetrics(ctx, t, aibrixClient, "circuit-breaker-freeze-e2e", []autoscalingv1alpha1.MetricSource{
		validExternalMetric("1"),
	})
	waitForCircuitBreakerState(
		ctx,
		t,
		aibrixClient,
		"circuit-breaker-freeze-e2e",
		autoscalingv1alpha1.CircuitBreakerStateClosed,
	)

	disableCircuitBreaker(ctx, t, aibrixClient, "circuit-breaker-freeze-e2e")
	waitForCircuitBreakerCleared(ctx, t, aibrixClient, "circuit-breaker-freeze-e2e")

	createCircuitBreakerPA(ctx, t, aibrixClient, circuitBreakerPAConfig{
		name:    "circuit-breaker-max-e2e",
		target:  "circuit-breaker-max-target",
		action:  autoscalingv1alpha1.CircuitBreakerActionMax,
		metrics: []autoscalingv1alpha1.MetricSource{invalidExternalMetric()},
	})
	waitForCircuitBreakerState(
		ctx,
		t,
		aibrixClient,
		"circuit-breaker-max-e2e",
		autoscalingv1alpha1.CircuitBreakerStateOpen,
	)
	waitForReplicas(ctx, t, k8sClient, "circuit-breaker-max-target", 4)
}

type circuitBreakerPAConfig struct {
	name    string
	target  string
	action  autoscalingv1alpha1.CircuitBreakerAction
	metrics []autoscalingv1alpha1.MetricSource
}

func createCircuitBreakerTarget(
	ctx context.Context,
	t *testing.T,
	k8sClient kubernetes.Interface,
	name string,
	replicas int32,
) {
	t.Helper()

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: circuitBreakerE2ENamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "target",
							Image:   "nginx:stable-alpine",
							Command: []string{"/bin/sh", "-c", "nginx -g 'daemon off;'"},
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 80},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/",
										Port: intstr.FromString("http"),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       5,
							},
						},
					},
				},
			},
		},
	}

	_, err := k8sClient.AppsV1().Deployments(circuitBreakerE2ENamespace).Create(ctx, deployment, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, getErr := k8sClient.AppsV1().Deployments(circuitBreakerE2ENamespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("failed to get existing deployment %s: %v", name, getErr)
		}
		deployment.ResourceVersion = current.ResourceVersion
		_, err = k8sClient.AppsV1().Deployments(circuitBreakerE2ENamespace).Update(ctx, deployment, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("failed to create deployment %s: %v", name, err)
	}
}

func createCircuitBreakerPA(
	ctx context.Context,
	t *testing.T,
	aibrixClient aibrixclientset.Interface,
	cfg circuitBreakerPAConfig,
) {
	t.Helper()

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.name,
			Namespace: circuitBreakerE2ENamespace,
			Annotations: map[string]string{
				patypes.MaxScaleUpRateLabel: "4",
			},
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       cfg.target,
			},
			MinReplicas:     ptr.To[int32](1),
			MaxReplicas:     4,
			MetricsSources:  cfg.metrics,
			ScalingStrategy: autoscalingv1alpha1.APA,
			CircuitBreaker: &autoscalingv1alpha1.CircuitBreakerConfig{
				Enabled:           true,
				Action:            cfg.action,
				FailureThreshold:  1,
				RecoveryThreshold: 1,
			},
		},
	}

	_, err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(circuitBreakerE2ENamespace).
		Create(ctx, pa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, getErr := aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(circuitBreakerE2ENamespace).
			Get(ctx, pa.Name, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("failed to get existing PodAutoscaler %s: %v", pa.Name, getErr)
		}
		pa.ResourceVersion = current.ResourceVersion
		_, err = aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(circuitBreakerE2ENamespace).
			Update(ctx, pa, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("failed to create PodAutoscaler %s: %v", pa.Name, err)
	}
}

func updateCircuitBreakerPAMetrics(
	ctx context.Context,
	t *testing.T,
	aibrixClient aibrixclientset.Interface,
	name string,
	metrics []autoscalingv1alpha1.MetricSource,
) {
	t.Helper()

	patch, err := json.Marshal(struct {
		Spec struct {
			MetricsSources []autoscalingv1alpha1.MetricSource `json:"metricsSources"`
		} `json:"spec"`
	}{Spec: struct {
		MetricsSources []autoscalingv1alpha1.MetricSource `json:"metricsSources"`
	}{MetricsSources: metrics}})
	if err != nil {
		t.Fatalf("failed to marshal PodAutoscaler %s metrics patch: %v", name, err)
	}

	if _, err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(circuitBreakerE2ENamespace).
		Patch(ctx, name, k8stypes.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("failed to update PodAutoscaler %s metrics: %v", name, err)
	}
}

func disableCircuitBreaker(ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, name string) {
	t.Helper()

	patch, err := json.Marshal(struct {
		Spec struct {
			MetricsSources []autoscalingv1alpha1.MetricSource `json:"metricsSources"`
			CircuitBreaker struct {
				Enabled bool `json:"enabled"`
			} `json:"circuitBreaker"`
		} `json:"spec"`
	}{Spec: struct {
		MetricsSources []autoscalingv1alpha1.MetricSource `json:"metricsSources"`
		CircuitBreaker struct {
			Enabled bool `json:"enabled"`
		} `json:"circuitBreaker"`
	}{
		MetricsSources: []autoscalingv1alpha1.MetricSource{validExternalMetric("1")},
		CircuitBreaker: struct {
			Enabled bool `json:"enabled"`
		}{Enabled: false},
	}})
	if err != nil {
		t.Fatalf("failed to marshal PodAutoscaler %s disable patch: %v", name, err)
	}

	if _, err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(circuitBreakerE2ENamespace).
		Patch(ctx, name, k8stypes.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("failed to disable circuit breaker for PodAutoscaler %s: %v", name, err)
	}
}

func validExternalMetric(targetValue string) autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.EXTERNAL,
		TargetMetric:     externalMetricName,
		TargetValue:      targetValue,
	}
}

func invalidExternalMetric() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.EXTERNAL,
		TargetMetric:     circuitBreakerInvalidMetric,
		TargetValue:      "1",
	}
}

func waitForDesiredAtLeast(
	ctx context.Context,
	t *testing.T,
	k8sClient kubernetes.Interface,
	name string,
	minimum int32,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		deployment, err := k8sClient.AppsV1().Deployments(circuitBreakerE2ENamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas >= minimum {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("deployment %s did not scale to at least %d replicas: %v", name, minimum, err)
	}
}

func waitForReplicas(ctx context.Context, t *testing.T, k8sClient kubernetes.Interface, name string, replicas int32) {
	t.Helper()

	var currentReplicas string
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		deployment, err := k8sClient.AppsV1().Deployments(circuitBreakerE2ENamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			currentReplicas = fmt.Sprintf("get failed: %v", err)
			return false, err
		}
		currentReplicas = "<nil>"
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == replicas {
			return true, nil
		}
		if deployment.Spec.Replicas != nil {
			currentReplicas = fmt.Sprintf("%d", *deployment.Spec.Replicas)
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("deployment %s did not reach %d replicas, current=%s: %v", name, replicas, currentReplicas, err)
	}
}

func waitForCircuitBreakerState(
	ctx context.Context,
	t *testing.T,
	aibrixClient aibrixclientset.Interface,
	name string,
	state autoscalingv1alpha1.CircuitBreakerState,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		pa, err := aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(circuitBreakerE2ENamespace).
			Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pa.Status.CircuitBreaker != nil && pa.Status.CircuitBreaker.State == state, nil
	})
	if err != nil {
		t.Fatalf("PodAutoscaler %s did not reach circuit breaker state %s: %v", name, state, err)
	}
}

func waitForCircuitBreakerCleared(
	ctx context.Context,
	t *testing.T,
	aibrixClient aibrixclientset.Interface,
	name string,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		pa, err := aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(circuitBreakerE2ENamespace).
			Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pa.Status.CircuitBreaker == nil, nil
	})
	if err != nil {
		t.Fatalf("PodAutoscaler %s circuit breaker status was not cleared: %v", name, err)
	}
}

func cleanupCircuitBreakerE2E(
	ctx context.Context,
	t *testing.T,
	k8sClient kubernetes.Interface,
	aibrixClient aibrixclientset.Interface,
) {
	t.Helper()

	for _, name := range []string{"circuit-breaker-freeze-e2e", "circuit-breaker-max-e2e"} {
		err := aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(circuitBreakerE2ENamespace).
			Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Logf("failed to delete PodAutoscaler %s: %v", name, err)
		}
	}
	for _, name := range []string{"circuit-breaker-freeze-target", "circuit-breaker-max-target"} {
		err := k8sClient.AppsV1().
			Deployments(circuitBreakerE2ENamespace).
			Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Logf("failed to delete deployment %s: %v", name, err)
		}
	}
}

func formatCircuitBreakerStatus(status *autoscalingv1alpha1.CircuitBreakerStatus) string {
	if status == nil {
		return "<nil>"
	}
	return fmt.Sprintf(
		"state=%s failureCount=%d recoveryCount=%d action=%s",
		status.State,
		status.FailureCount,
		status.RecoveryCount,
		status.Action,
	)
}
