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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixclientset "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	"github.com/vllm-project/aibrix/pkg/constants"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
)

const (
	roleSetKPAE2EEnabledEnv = "AIBRIX_ROLESET_KPA_E2E"
	roleSetKPAStormService  = "roleset-kpa-e2e"
	roleSetKPAPAName        = "roleset-kpa-e2e"
	roleSetKPANamespace     = "default"
	// Each RoleSet runs 1 prefill pod and a decode pod group of 2 pods.
	roleSetKPAPodsPerRoleSet = 3
)

// TestKPAScalesReplicaModeStormServiceWithPodGroups runs KPA against a replica-mode
// StormService whose decode role runs pod groups of 2. Every pod reports a
// gpu_cache_usage_perc of 60 against a target of 50, and the autoscaler selects
// the leader of each decode group. KPA scales 2 RoleSets to the maxReplicas of 3
// and keeps 3 once every RoleSet is ready.
func TestKPAScalesReplicaModeStormServiceWithPodGroups(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(envOrDefault(roleSetKPAE2EEnabledEnv, "false"))) != e2eEnabledValue {
		t.Skip("set AIBRIX_ROLESET_KPA_E2E=true to run the RoleSet KPA e2e test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	k8sClient, aibrixClient := scheduledBoundsClients(t)
	cleanupRoleSetKPAE2E(ctx, t, aibrixClient)
	defer cleanupRoleSetKPAE2E(context.Background(), t, aibrixClient)

	createRoleSetKPAStormService(ctx, t, aibrixClient, 2)
	waitForReadyRoleSets(ctx, t, k8sClient, 2)
	createRoleSetKPAPodAutoscaler(ctx, t, aibrixClient)

	waitForStormServiceReplicas(ctx, t, aibrixClient, 3)
	waitForReadyRoleSets(ctx, t, k8sClient, 3)

	// 3 ready RoleSets at 60 want 3.6, which maxReplicas caps at 3. Hold for 6
	// controller resyncs.
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		got := stormServiceReplicas(ctx, t, aibrixClient)
		pa, err := aibrixClient.AutoscalingV1alpha1().
			PodAutoscalers(roleSetKPANamespace).
			Get(ctx, roleSetKPAPAName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get PodAutoscaler: %v", err)
		}
		t.Logf("StormService replicas %d, PodAutoscaler desired %d actual %d",
			got, pa.Status.DesiredScale, pa.Status.ActualScale)
		if got != 3 {
			t.Fatalf("StormService replicas changed to %d under a fixed load, want 3", got)
		}
		time.Sleep(10 * time.Second)
	}
}

func createRoleSetKPAStormService(
	ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, replicas int32,
) {
	t.Helper()

	labels := map[string]string{"app": roleSetKPAStormService}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"app":                      roleSetKPAStormService,
			constants.ModelLabelEngine: "vllm",
		}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:            "llm-engine",
				Image:           "aibrix/vllm-mock:nightly",
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports:           []corev1.ContainerPort{{ContainerPort: 8000}},
				Env: []corev1.EnvVar{
					{Name: "STANDALONE_MODE", Value: "true"},
					{Name: "METRICS_OVERRIDES", Value: `{"gpu_cache_usage_perc": 60}`},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8000)}},
					PeriodSeconds: 2,
				},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				}},
			}},
		},
	}
	ss := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: roleSetKPAStormService, Namespace: roleSetKPANamespace},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Mode:     orchestrationv1alpha1.StormServiceReplicaMode,
			Replicas: ptr.To(replicas),
			Selector: metav1.SetAsLabelSelector(labels),
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type: orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: &orchestrationv1alpha1.RoleSetSpec{Roles: []orchestrationv1alpha1.RoleSpec{
					{Name: "prefill", Replicas: ptr.To(int32(1)), Template: template},
					{Name: "decode", Replicas: ptr.To(int32(1)), PodGroupSize: ptr.To(int32(2)), Template: template},
				}},
			},
		},
	}
	_, err := aibrixClient.OrchestrationV1alpha1().
		StormServices(roleSetKPANamespace).
		Create(ctx, ss, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create StormService: %v", err)
	}
}

func createRoleSetKPAPodAutoscaler(ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface) {
	t.Helper()

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: roleSetKPAPAName, Namespace: roleSetKPANamespace},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: orchestrationv1alpha1.GroupVersion.String(),
				Kind:       "StormService",
				Name:       roleSetKPAStormService,
			},
			MinReplicas:     ptr.To(int32(1)),
			MaxReplicas:     3,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.POD,
				ProtocolType:     autoscalingv1alpha1.HTTP,
				Port:             "8000",
				Path:             "/metrics",
				TargetMetric:     "gpu_cache_usage_perc",
				TargetValue:      "50",
			}},
		},
	}
	_, err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(roleSetKPANamespace).
		Create(ctx, pa, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create PodAutoscaler: %v", err)
	}
}

func stormServiceReplicas(ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface) int32 {
	t.Helper()

	ss, err := aibrixClient.OrchestrationV1alpha1().
		StormServices(roleSetKPANamespace).
		Get(ctx, roleSetKPAStormService, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get StormService: %v", err)
	}
	return ptr.Deref(ss.Spec.Replicas, 0)
}

func waitForStormServiceReplicas(
	ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, want int32,
) {
	t.Helper()

	got := int32(-1)
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
		if replicas := stormServiceReplicas(ctx, t, aibrixClient); replicas != got {
			got = replicas
			t.Logf("StormService replicas %d", got)
		}
		return got == want, nil
	})
	if err != nil {
		t.Fatalf("StormService replicas stayed at %d, want %d: %v", got, want, err)
	}
}

// waitForReadyRoleSets waits until the StormService runs want RoleSets of ready pods.
func waitForReadyRoleSets(ctx context.Context, t *testing.T, k8sClient kubernetes.Interface, want int) {
	t.Helper()

	ready := -1
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods, err := k8sClient.CoreV1().Pods(roleSetKPANamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", controllerconstants.StormServiceNameLabelKey, roleSetKPAStormService),
		})
		if err != nil {
			return false, err
		}
		n := 0
		for _, pod := range pods.Items {
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					n++
				}
			}
		}
		if n != ready {
			ready = n
			t.Logf("%d of %d pods ready", ready, len(pods.Items))
		}
		return ready == want*roleSetKPAPodsPerRoleSet, nil
	})
	if err != nil {
		t.Fatalf("%d pods ready, want %d: %v", ready, want*roleSetKPAPodsPerRoleSet, err)
	}
}

func cleanupRoleSetKPAE2E(ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface) {
	t.Helper()

	err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(roleSetKPANamespace).
		Delete(ctx, roleSetKPAPAName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete PodAutoscaler: %v", err)
	}
	stormServices := aibrixClient.OrchestrationV1alpha1().StormServices(roleSetKPANamespace)
	err = stormServices.Delete(ctx, roleSetKPAStormService, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete StormService: %v", err)
	}
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := stormServices.Get(ctx, roleSetKPAStormService, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Logf("StormService still exists after cleanup: %v", err)
	}
}
