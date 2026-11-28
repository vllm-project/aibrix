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
	roleSetAutoscalingE2EEnabledEnv = "AIBRIX_ROLESET_AUTOSCALING_E2E"
	roleSetAutoscalingNamespace     = "default"
	// Every pod carries this readiness gate, so the test decides when a RoleSet
	// becomes ready.
	roleSetReadyGate corev1.PodConditionType = "e2e.aibrix.ai/ready"
)

// roleSetScenario is a replica-mode StormService of 2 RoleSets whose mock vLLM
// pods all report a gpu_cache_usage_perc of 60, scaled by a PodAutoscaler with a
// target of 50 and a maxReplicas of 10.
type roleSetScenario struct {
	name     string
	strategy autoscalingv1alpha1.ScalingStrategyType
	// With decodeGroupSize > 1 the decode role runs pod groups and the
	// autoscaler selects one pod per RoleSet, the group leader. Otherwise it
	// selects both the prefill and the decode pod of each RoleSet.
	decodeGroupSize int32
	// start is when the case started; logs show the seconds since then.
	start time.Time
}

const (
	roleSetScenarioMetric   = 60
	roleSetScenarioReplicas = 2
)

func (s roleSetScenario) podsPerRoleSet() int {
	if s.decodeGroupSize > 1 {
		return 1 + int(s.decodeGroupSize)
	}
	return 2
}

func (s roleSetScenario) logf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("+%3.0fs "+format, append([]any{time.Since(s.start).Seconds()}, args...)...)
}

// TestReplicaModeStormServiceAutoscaling runs KPA and APA against replica-mode
// StormServices, with the readiness of each RoleSet set by the test. Whether a
// RoleSet has one selected pod or two, every RoleSet carries the same load, so
// both layouts must scale to the same RoleSet counts.
func TestReplicaModeStormServiceAutoscaling(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(envOrDefault(roleSetAutoscalingE2EEnabledEnv, "false"))) != e2eEnabledValue {
		t.Skip("set AIBRIX_ROLESET_AUTOSCALING_E2E=true to run the RoleSet autoscaling e2e test")
	}
	k8sClient, aibrixClient := scheduledBoundsClients(t)

	layouts := []struct {
		name            string
		decodeGroupSize int32
	}{
		{name: "pod-groups", decodeGroupSize: 2},
		{name: "two-pods"},
	}
	strategies := []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.KPA, autoscalingv1alpha1.APA}
	for _, layout := range layouts {
		for _, strategy := range strategies {
			// The StormService webhook rejects names that, together with the
			// longest pod-group role name, could push PodSet names past 63 characters.
			s := roleSetScenario{
				name:            fmt.Sprintf("e2e-%s-%s", strings.ToLower(string(strategy)), layout.name),
				strategy:        strategy,
				decodeGroupSize: layout.decodeGroupSize,
			}
			t.Run(s.name, func(t *testing.T) {
				s.start = time.Now()
				ctx := startRoleSetScenario(t, k8sClient, aibrixClient, s)

				// 2 ready RoleSets at 60 against 50 per selected pod is 2.4 RoleSets.
				waitForRoleSetReplicas(ctx, t, aibrixClient, s, 3)
				// The 3rd RoleSet is not ready and counts as carrying no load.
				holdRoleSetReplicas(ctx, t, aibrixClient, s, 3, 30*time.Second)

				// 3 ready RoleSets is 3.6 RoleSets. The metric window averages the
				// summed load, so this can take up to its 1 minute panic window.
				markRoleSetsReady(ctx, t, k8sClient, s, 3)
				waitForRoleSetReplicas(ctx, t, aibrixClient, s, 4)
				// The 4th RoleSet is not ready, and the load of the 3 ready RoleSets
				// holds 4.
				holdRoleSetReplicas(ctx, t, aibrixClient, s, 4, 30*time.Second)
			})
		}
	}
}

// startRoleSetScenario creates the StormService, marks its RoleSets ready and
// then creates the PodAutoscaler.
func startRoleSetScenario(
	t *testing.T, k8sClient kubernetes.Interface, aibrixClient aibrixclientset.Interface, s roleSetScenario,
) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	cleanupRoleSetScenario(ctx, t, aibrixClient, s)
	t.Cleanup(func() { cleanupRoleSetScenario(context.Background(), t, aibrixClient, s) })

	createRoleSetStormService(ctx, t, aibrixClient, s)
	markRoleSetsReady(ctx, t, k8sClient, s, roleSetScenarioReplicas)
	createRoleSetPodAutoscaler(ctx, t, aibrixClient, s)
	return ctx
}

func createRoleSetStormService(
	ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, s roleSetScenario,
) {
	t.Helper()

	labels := map[string]string{"app": s.name}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"app":                      s.name,
			constants.ModelLabelEngine: "vllm",
		}},
		Spec: corev1.PodSpec{
			ReadinessGates: []corev1.PodReadinessGate{{ConditionType: roleSetReadyGate}},
			Containers: []corev1.Container{{
				Name:            "llm-engine",
				Image:           "aibrix/vllm-mock:nightly",
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports:           []corev1.ContainerPort{{ContainerPort: 8000}},
				Env: []corev1.EnvVar{
					{Name: "STANDALONE_MODE", Value: "true"},
					{Name: "METRICS_OVERRIDES", Value: fmt.Sprintf(`{"gpu_cache_usage_perc": %d}`, roleSetScenarioMetric)},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8000)},
					},
					PeriodSeconds: 2,
				},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				}},
			}},
		},
	}
	decode := orchestrationv1alpha1.RoleSpec{Name: "decode", Replicas: ptr.To(int32(1)), Template: template}
	if s.decodeGroupSize > 1 {
		decode.PodGroupSize = ptr.To(s.decodeGroupSize)
	}
	ss := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: roleSetAutoscalingNamespace},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Mode:     orchestrationv1alpha1.StormServiceReplicaMode,
			Replicas: ptr.To(int32(roleSetScenarioReplicas)),
			Selector: metav1.SetAsLabelSelector(labels),
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type: orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
			},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: &orchestrationv1alpha1.RoleSetSpec{Roles: []orchestrationv1alpha1.RoleSpec{
					{Name: "prefill", Replicas: ptr.To(int32(1)), Template: template},
					decode,
				}},
			},
		},
	}
	_, err := aibrixClient.OrchestrationV1alpha1().
		StormServices(roleSetAutoscalingNamespace).
		Create(ctx, ss, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create StormService: %v", err)
	}
}

func createRoleSetPodAutoscaler(
	ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, s roleSetScenario,
) {
	t.Helper()

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: roleSetAutoscalingNamespace},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: orchestrationv1alpha1.GroupVersion.String(),
				Kind:       "StormService",
				Name:       s.name,
			},
			MinReplicas:     ptr.To(int32(1)),
			MaxReplicas:     10,
			ScalingStrategy: s.strategy,
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
		PodAutoscalers(roleSetAutoscalingNamespace).
		Create(ctx, pa, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create PodAutoscaler: %v", err)
	}
}

// roleSetScale is the StormService replicas and the desired scale in the
// PodAutoscaler status.
type roleSetScale struct {
	replicas, desired int32
}

func getRoleSetScale(
	ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, s roleSetScenario,
) roleSetScale {
	t.Helper()

	ss, err := aibrixClient.OrchestrationV1alpha1().
		StormServices(roleSetAutoscalingNamespace).
		Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get StormService: %v", err)
	}
	pa, err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(roleSetAutoscalingNamespace).
		Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get PodAutoscaler: %v", err)
	}
	return roleSetScale{replicas: ptr.Deref(ss.Spec.Replicas, 0), desired: pa.Status.DesiredScale}
}

// waitForRoleSetReplicas waits until both the StormService replicas and the
// PodAutoscaler's desired scale are want, and fails as soon as either goes past
// want.
func waitForRoleSetReplicas(
	ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, s roleSetScenario, want int32,
) {
	t.Helper()

	last := roleSetScale{-1, -1}
	for deadline := time.Now().Add(3 * time.Minute); last != (roleSetScale{want, want}); time.Sleep(2 * time.Second) {
		if got := getRoleSetScale(ctx, t, aibrixClient, s); got != last {
			last = got
			s.logf(t, "StormService replicas %d, PodAutoscaler desired %d", got.replicas, got.desired)
		}
		if last.replicas > want || last.desired > want || time.Now().After(deadline) {
			t.Fatalf("StormService replicas %d and PodAutoscaler desired %d, want %d",
				last.replicas, last.desired, want)
		}
	}
}

// holdRoleSetReplicas checks every 10 seconds, across several 10 second
// controller resyncs, that the StormService keeps want replicas.
func holdRoleSetReplicas(
	ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, s roleSetScenario,
	want int32, d time.Duration,
) {
	t.Helper()

	for deadline := time.Now().Add(d); ; time.Sleep(10 * time.Second) {
		got := getRoleSetScale(ctx, t, aibrixClient, s)
		s.logf(t, "StormService replicas %d, PodAutoscaler desired %d", got.replicas, got.desired)
		if got.replicas != want {
			t.Fatalf("StormService replicas changed to %d, want %d", got.replicas, want)
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

// markRoleSetsReady waits until the StormService has the pods of exactly want
// RoleSets, sets the readiness gate on those pods, and waits until they are
// ready. Pods created after that, for the next RoleSet, stay not ready.
func markRoleSetsReady(ctx context.Context, t *testing.T, k8sClient kubernetes.Interface, s roleSetScenario, want int) {
	t.Helper()

	pods := k8sClient.CoreV1().Pods(roleSetAutoscalingNamespace)
	selector := fmt.Sprintf("%s=%s", controllerconstants.StormServiceNameLabelKey, s.name)
	wantPods := want * s.podsPerRoleSet()
	var marked map[string]bool
	ready := -1
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}
		if marked == nil {
			if len(list.Items) != wantPods {
				return false, nil
			}
			marked = map[string]bool{}
			for _, pod := range list.Items {
				marked[pod.Name] = true
			}
		}
		n := 0
		for i := range list.Items {
			pod := &list.Items[i]
			if !marked[pod.Name] {
				continue
			}
			if !hasPodCondition(pod, roleSetReadyGate) {
				pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
					Type:               roleSetReadyGate,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				})
				if _, err := pods.UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil && !apierrors.IsConflict(err) {
					return false, err
				}
			}
			if hasPodCondition(pod, corev1.PodReady) {
				n++
			}
		}
		if n != ready {
			ready = n
			s.logf(t, "%d of %d pods ready", ready, wantPods)
		}
		return ready == wantPods, nil
	})
	if err != nil {
		t.Fatalf("%d pods ready, want %d: %v", ready, wantPods, err)
	}
}

func hasPodCondition(pod *corev1.Pod, conditionType corev1.PodConditionType) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == conditionType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// cleanupRoleSetScenario deletes the PodAutoscaler and the StormService and
// waits until the StormService is gone.
func cleanupRoleSetScenario(
	ctx context.Context, t *testing.T, aibrixClient aibrixclientset.Interface, s roleSetScenario,
) {
	t.Helper()

	err := aibrixClient.AutoscalingV1alpha1().
		PodAutoscalers(roleSetAutoscalingNamespace).
		Delete(ctx, s.name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete PodAutoscaler: %v", err)
	}
	stormServices := aibrixClient.OrchestrationV1alpha1().StormServices(roleSetAutoscalingNamespace)
	err = stormServices.Delete(ctx, s.name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("failed to delete StormService: %v", err)
	}
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := stormServices.Get(ctx, s.name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Logf("StormService still exists after cleanup: %v", err)
	}
}
