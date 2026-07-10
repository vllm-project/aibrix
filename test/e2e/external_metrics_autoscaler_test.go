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
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	aibrixclientset "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	aibrixfake "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/fake"
	patypes "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

const (
	externalMetricName = "aibrix_test_queue_depth"
	e2eTargetName      = "external-metrics-scale-target"
	e2ePANamespace     = "default"
	e2eEnabledValue    = "true"
)

func TestExternalMetricsAutoscaler(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(envOrDefault("AIBRIX_EXTERNAL_METRICS_E2E", "false"))) != e2eEnabledValue {
		t.Skip("set AIBRIX_EXTERNAL_METRICS_E2E=true to run the external metrics e2e test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	k8sClient, aibrixClient := externalMetricsClients(t)
	cleanupExternalMetricsE2E(ctx, t, k8sClient, aibrixClient)
	defer func() {
		if t.Failed() && strings.ToLower(envOrDefault("AIBRIX_EXTERNAL_METRICS_E2E_KEEP_ON_FAILURE", "false")) == e2eEnabledValue {
			t.Log("preserving external metrics e2e resources for failure diagnostics")
			return
		}
		cleanupExternalMetricsE2E(context.Background(), t, k8sClient, aibrixClient)
	}()

	assertExternalMetricAPI(ctx, t, k8sClient)
	createExternalMetricsScaleTarget(ctx, t, k8sClient)
	createExternalMetricPodAutoscaler(ctx, t, aibrixClient)
	waitForDesiredReplicas(ctx, t, k8sClient, 2)
}

func TestCreateExternalMetricsScaleTargetUpdatesExistingResource(t *testing.T) {
	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            e2eTargetName,
			Namespace:       e2ePANamespace,
			ResourceVersion: "1",
		},
	}
	k8sClient := k8sfake.NewSimpleClientset(existing)
	k8sClient.PrependReactor("update", "deployments", requireUpdateResourceVersion)

	createExternalMetricsScaleTarget(context.Background(), t, k8sClient)
}

func TestCreateExternalMetricPodAutoscalerUpdatesExistingResource(t *testing.T) {
	existing := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "external-metrics-e2e",
			Namespace:       e2ePANamespace,
			ResourceVersion: "1",
		},
	}
	aibrixClient := aibrixfake.NewSimpleClientset(existing)
	aibrixClient.PrependReactor("update", "podautoscalers", requireUpdateResourceVersion)

	createExternalMetricPodAutoscaler(context.Background(), t, aibrixClient)
}

func requireUpdateResourceVersion(action k8stesting.Action) (bool, runtime.Object, error) {
	update := action.(k8stesting.UpdateAction)
	accessor, err := meta.Accessor(update.GetObject())
	if err != nil {
		return true, nil, err
	}
	if accessor.GetResourceVersion() == "" {
		return true, nil, errors.New("resourceVersion required")
	}
	return false, nil, nil
}

func externalMetricsClients(t *testing.T) (*kubernetes.Clientset, *aibrixclientset.Clientset) {
	t.Helper()

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeConfig := os.Getenv("KUBECONFIG"); kubeConfig != "" {
		loadingRules.ExplicitPath = kubeConfig
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatalf("failed to build kube config: %v", err)
	}
	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("failed to create Kubernetes client: %v", err)
	}
	aibrixClient, err := aibrixclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("failed to create AIBrix client: %v", err)
	}
	return k8sClient, aibrixClient
}

func assertExternalMetricAPI(ctx context.Context, t *testing.T, k8sClient *kubernetes.Clientset) {
	t.Helper()

	body, err := k8sClient.Discovery().RESTClient().Get().
		AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/default/" + externalMetricName).
		DoRaw(ctx)
	if err != nil {
		t.Fatalf("external metrics API is not queryable: %v", err)
	}
	if !strings.Contains(string(body), externalMetricName) {
		t.Fatalf("external metrics API response does not contain %q: %s", externalMetricName, string(body))
	}
}

func createExternalMetricsScaleTarget(ctx context.Context, t *testing.T, k8sClient kubernetes.Interface) {
	t.Helper()

	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e2eTargetName,
			Namespace: e2ePANamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": e2eTargetName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": e2eTargetName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "target",
							Image:           "aibrix/external-metrics-adapter:e2e",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports: []corev1.ContainerPort{
								{Name: "https", ContainerPort: 8443},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/readyz",
										Port:   intstr.FromString("https"),
										Scheme: corev1.URISchemeHTTPS,
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

	_, err := k8sClient.AppsV1().Deployments(e2ePANamespace).Create(ctx, deployment, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, getErr := k8sClient.AppsV1().Deployments(e2ePANamespace).Get(ctx, e2eTargetName, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("failed to get existing scale target deployment: %v", getErr)
		}
		deployment.ResourceVersion = current.ResourceVersion
		_, err = k8sClient.AppsV1().Deployments(e2ePANamespace).Update(ctx, deployment, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("failed to create scale target deployment: %v", err)
	}
}

func createExternalMetricPodAutoscaler(ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface) {
	t.Helper()

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-metrics-e2e",
			Namespace: e2ePANamespace,
			Annotations: map[string]string{
				patypes.MaxScaleUpRateLabel: "4",
			},
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       e2eTargetName,
			},
			MinReplicas: ptr.To[int32](1),
			MaxReplicas: 4,
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.EXTERNAL,
					TargetMetric:     externalMetricName,
					TargetValue:      "40",
				},
			},
			ScalingStrategy: autoscalingv1alpha1.APA,
		},
	}

	_, err := aibrixClient.AutoscalingV1alpha1().PodAutoscalers(e2ePANamespace).Create(ctx, pa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		current, getErr := aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(e2ePANamespace).
			Get(ctx, pa.Name, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("failed to get existing PodAutoscaler: %v", getErr)
		}
		pa.ResourceVersion = current.ResourceVersion
		_, err = aibrixClient.AutoscalingV1alpha1().PodAutoscalers(e2ePANamespace).Update(ctx, pa, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("failed to create PodAutoscaler: %v", err)
	}
}

func waitForDesiredReplicas(ctx context.Context, t *testing.T, k8sClient *kubernetes.Clientset, minimum int32) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		deployment, err := k8sClient.AppsV1().Deployments(e2ePANamespace).Get(ctx, e2eTargetName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas >= minimum {
			return true, nil
		}
		current := int32(0)
		if deployment.Spec.Replicas != nil {
			current = *deployment.Spec.Replicas
		}
		t.Logf("waiting for %s replicas >= %d, current spec.replicas=%d", e2eTargetName, minimum, current)
		return false, nil
	})
	if err != nil {
		t.Fatalf("deployment did not scale to at least %d replicas: %v", minimum, err)
	}
}

func cleanupExternalMetricsE2E(
	ctx context.Context,
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	aibrixClient *aibrixclientset.Clientset,
) {
	t.Helper()

	err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(e2ePANamespace).
		Delete(ctx, "external-metrics-e2e", metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete PodAutoscaler: %v", err)
	}

	err = k8sClient.AppsV1().Deployments(e2ePANamespace).Delete(ctx, e2eTargetName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete scale target deployment: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(strings.Trim(os.Getenv(key), "\"")); value != "" {
		return value
	}
	return fallback
}
