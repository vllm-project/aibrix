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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
)

func TestStormServiceVolcanoGangScheduling(t *testing.T) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(stormServiceVolcanoE2EEnv)), "true") {
		t.Skipf("set %s=true to run the StormService Volcano gang scheduling e2e", stormServiceVolcanoE2EEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	namespace := stormServiceNamespace()
	h := newStormServiceHarness(t, namespace)

	nodes, err := h.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	eligibleNodes := eligibleNodeCount(nodes.Items)
	if eligibleNodes == 0 {
		t.Fatal("Volcano gang test requires at least one Ready, schedulable node")
	}

	t.Run("impossible gang remains wholly unbound", func(t *testing.T) {
		name := fmt.Sprintf("stormservice-volcano-blocked-%d", time.Now().UnixNano())
		cleanupState := registerVolcanoCleanup(t, h, name)
		stormService := newVolcanoStormService(namespace, name, true, eligibleNodes)
		members := int(*stormService.Spec.Template.Spec.Roles[0].Replicas)
		if _, err := h.stormServices.Create(ctx, stormService, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create impossible Volcano StormService: %v", err)
		}
		roleSets, err := h.waitForRoleSets(ctx, name, 1)
		if err != nil {
			t.Fatalf("wait for impossible gang RoleSet: %v", err)
		}
		roleSetName := roleSets[0].GetName()
		_, err = waitForVolcanoPodGroup(
			ctx,
			h.dynamicClient,
			namespace,
			roleSetName,
			func(podGroup *unstructured.Unstructured) bool {
				return volcanoPodGroupConfigured(
					podGroup,
					roleSetName,
					roleSets[0].GetUID(),
					int32(members),
					map[string]int32{stormServiceWorkerRoleName: int32(members)},
				)
			},
		)
		if err != nil {
			t.Fatalf("wait for impossible gang PodGroup: %v", err)
		}
		blockedPods, err := waitForPods(ctx, h.kubeClient, namespace, name, members, false)
		if err != nil {
			t.Fatalf("wait for impossible gang Pods: %v", err)
		}
		if !volcanoPodsMarked(
			blockedPods,
			roleSetName,
			map[string]int{stormServiceWorkerRoleName: members},
		) {
			t.Fatalf("impossible gang Pods are missing Volcano scheduler/group/task markers: %s", describePods(blockedPods))
		}
		if err := requireConsistentFor(ctx, time.Second, 45*time.Second, func(ctx context.Context) error {
			pods, err := h.kubeClient.CoreV1().Pods(namespace).List(
				ctx,
				metav1.ListOptions{LabelSelector: stormServiceSelector(name)},
			)
			if err != nil {
				return err
			}
			if len(pods.Items) != members {
				return fmt.Errorf("impossible gang pod count=%d, want %d", len(pods.Items), members)
			}
			for i := range pods.Items {
				if pods.Items[i].Spec.NodeName != "" {
					return fmt.Errorf("gang Pod %s bound early to node %s", pods.Items[i].Name, pods.Items[i].Spec.NodeName)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("verify impossible gang remains unbound: %v", err)
		}
		if err := cleanupState.run(ctx); err != nil {
			t.Fatalf("clean up impossible gang: %v", err)
		}
	})

	t.Run("feasible gang schedules and reports healthy", func(t *testing.T) {
		name := fmt.Sprintf("stormservice-volcano-ready-%d", time.Now().UnixNano())
		cleanupState := registerVolcanoCleanup(t, h, name)
		stormService := newVolcanoStormService(namespace, name, false, eligibleNodes)
		if _, err := h.stormServices.Create(ctx, stormService, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create feasible Volcano StormService: %v", err)
		}
		roleSets, err := h.waitForRoleSets(ctx, name, 1)
		if err != nil {
			t.Fatalf("wait for feasible gang RoleSet: %v", err)
		}
		roleSet := &roleSets[0]
		roleSetName := roleSet.GetName()
		if _, err := waitForVolcanoPods(ctx, h, name, roleSetName); err != nil {
			t.Fatalf("wait for feasible Volcano Pods: %v", err)
		}
		_, err = waitForVolcanoPodGroup(
			ctx,
			h.dynamicClient,
			namespace,
			roleSetName,
			func(podGroup *unstructured.Unstructured) bool {
				return volcanoPodGroupRunningAndConfigured(podGroup, roleSetName, roleSet.GetUID())
			},
		)
		if err != nil {
			t.Fatalf("wait for configured Running PodGroup: %v", err)
		}
		if err := waitForRoleSetGangHealthy(ctx, h.dynamicClient, namespace, roleSetName); err != nil {
			t.Fatalf("wait for RoleSet gang conditions: %v", err)
		}
		if _, err := waitForStormServiceState(ctx, h.stormServices, name, stormServiceGangHealthy); err != nil {
			t.Fatalf("wait for StormService gang conditions: %v", err)
		}
		if err := cleanupState.run(ctx); err != nil {
			t.Fatalf("clean up feasible gang: %v", err)
		}
	})
}

func registerVolcanoCleanup(t *testing.T, h *stormServiceHarness, name string) *strictStormServiceCleanup {
	t.Helper()
	cleanupState := newStrictStormServiceCleanup(h, name, true)
	t.Cleanup(func() {
		if cleanupState.completed {
			return
		}
		if t.Failed() && strings.EqualFold(strings.TrimSpace(os.Getenv(stormServiceE2EKeepOnFailureEnv)), "true") {
			h.logRetainedStormServiceResources(t, name, true)
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), stormServiceCleanupTimeout)
		defer cancel()
		if err := cleanupState.run(cleanupCtx); err != nil {
			t.Errorf("cleanup Volcano StormService %s/%s: %v", h.namespace, name, err)
		}
	})
	return cleanupState
}

func waitForVolcanoPodGroup(
	ctx context.Context,
	dynamicClient dynamic.Interface,
	namespace, name string,
	predicate func(*unstructured.Unstructured) bool,
) (*unstructured.Unstructured, error) {
	var observed *unstructured.Unstructured
	latest := "not observed"
	err := wait.PollUntilContextTimeout(
		ctx,
		stormServicePollInterval,
		stormServicePollTimeout,
		true,
		func(ctx context.Context) (bool, error) {
			podGroup, err := dynamicClient.Resource(volcanoPodGroupGVR).
				Namespace(namespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, retryPollError(err, true, &latest)
			}
			observed = podGroup
			latest = fmt.Sprintf(
				"phase=%v spec=%v owners=%v",
				nestedString(podGroup.Object, "status", "phase"),
				podGroup.Object["spec"],
				podGroup.GetOwnerReferences(),
			)
			return predicate(podGroup), nil
		},
	)
	if err != nil {
		return observed, fmt.Errorf(
			"wait for Volcano PodGroup %s/%s: %w; latest observation: %s",
			namespace, name, err, latest,
		)
	}
	return observed, nil
}

func waitForVolcanoPods(
	ctx context.Context,
	h *stormServiceHarness,
	stormServiceName, roleSetName string,
) ([]corev1.Pod, error) {
	var observed []corev1.Pod
	latest := "not observed"
	err := wait.PollUntilContextTimeout(
		ctx,
		stormServicePollInterval,
		stormServicePollTimeout,
		true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(
				ctx,
				metav1.ListOptions{LabelSelector: stormServiceSelector(stormServiceName)},
			)
			if err != nil {
				return false, retryPollError(err, false, &latest)
			}
			observed = pods.Items
			latest = describePods(observed)
			return volcanoPodsReadyAndMarked(observed, roleSetName), nil
		},
	)
	if err != nil {
		return observed, fmt.Errorf(
			"wait for marked Volcano Pods for StormService %q: %w; latest observation: %s",
			stormServiceName, err, latest,
		)
	}
	return observed, nil
}

func volcanoPodsReadyAndMarked(pods []corev1.Pod, roleSetName string) bool {
	if !volcanoPodsMarked(pods, roleSetName, map[string]int{"prefill": 1, "decode": 1}) {
		return false
	}
	for i := range pods {
		if !podReady(&pods[i]) {
			return false
		}
	}
	return true
}

func volcanoPodsMarked(pods []corev1.Pod, roleSetName string, expectedRoles map[string]int) bool {
	expectedTotal := 0
	for _, count := range expectedRoles {
		expectedTotal += count
	}
	if len(pods) != expectedTotal {
		return false
	}
	observedRoles := make(map[string]int, len(expectedRoles))
	for i := range pods {
		pod := &pods[i]
		if pod.Spec.SchedulerName != "volcano" ||
			pod.Labels[controllerconstants.VolcanoPodGroupNameAnnotationKey] != roleSetName ||
			pod.Annotations[controllerconstants.VolcanoPodGroupNameAnnotationKey] != roleSetName {
			return false
		}
		role := pod.Labels[controllerconstants.VolcanoTaskSpecKey]
		if pod.Annotations[controllerconstants.VolcanoTaskSpecKey] != role {
			return false
		}
		if _, found := expectedRoles[role]; !found {
			return false
		}
		observedRoles[role]++
	}
	for role, expected := range expectedRoles {
		if observedRoles[role] != expected {
			return false
		}
	}
	return true
}

func volcanoPodGroupRunningAndConfigured(
	podGroup *unstructured.Unstructured,
	roleSetName string,
	roleSetUID types.UID,
) bool {
	if !volcanoPodGroupConfigured(
		podGroup,
		roleSetName,
		roleSetUID,
		2,
		map[string]int32{"prefill": 1, "decode": 1},
	) {
		return false
	}
	phase, found, err := unstructured.NestedString(podGroup.Object, "status", "phase")
	return err == nil && found && phase == "Running"
}

func volcanoPodGroupConfigured(
	podGroup *unstructured.Unstructured,
	roleSetName string,
	roleSetUID types.UID,
	minMember int32,
	minTaskMembers map[string]int32,
) bool {
	if podGroup == nil || podGroup.GetLabels()[controllerconstants.RoleSetNameLabelKey] != roleSetName ||
		!hasControllerOwnerUID(podGroup.GetOwnerReferences(), roleSetUID) {
		return false
	}
	observedMinMember, found, err := unstructured.NestedInt64(podGroup.Object, "spec", "minMember")
	if err != nil || !found || observedMinMember != int64(minMember) {
		return false
	}
	queue, found, err := unstructured.NestedString(podGroup.Object, "spec", "queue")
	if err != nil || !found || queue != stormServiceVolcanoDefaultQueue {
		return false
	}
	observedTasks, found, err := unstructured.NestedMap(podGroup.Object, "spec", "minTaskMember")
	if err != nil || !found || len(observedTasks) != len(minTaskMembers) {
		return false
	}
	for role, expected := range minTaskMembers {
		if integerValue(observedTasks[role]) != int64(expected) {
			return false
		}
	}
	return true
}

func hasControllerOwnerUID(ownerReferences []metav1.OwnerReference, ownerUID types.UID) bool {
	for i := range ownerReferences {
		owner := &ownerReferences[i]
		if owner.UID == ownerUID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func integerValue(value interface{}) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int32:
		return int64(typed)
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func nestedString(object map[string]interface{}, fields ...string) string {
	value, _, _ := unstructured.NestedString(object, fields...)
	return value
}

func waitForRoleSetGangHealthy(ctx context.Context, dynamicClient dynamic.Interface, namespace, name string) error {
	latest := "not observed"
	err := wait.PollUntilContextTimeout(
		ctx,
		stormServicePollInterval,
		3*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			object, err := dynamicClient.Resource(roleSetGVR).
				Namespace(namespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, retryPollError(err, true, &latest)
			}
			roleSet := &orchestrationv1alpha1.RoleSet{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, roleSet); err != nil {
				return false, err
			}
			latest = fmt.Sprintf("conditions=%v", roleSet.Status.Conditions)
			synced := condition(roleSet.Status.Conditions, orchestrationv1alpha1.RoleSetPodGroupSynced)
			healthy := condition(roleSet.Status.Conditions, orchestrationv1alpha1.RoleSetGangSchedulingError)
			return synced != nil && synced.Status == corev1.ConditionTrue && synced.Reason == "PodGroupSynced" &&
				healthy != nil && healthy.Status == corev1.ConditionFalse &&
				healthy.Reason == "GangSchedulingHealthy", nil
		},
	)
	if err != nil {
		return fmt.Errorf(
			"wait for RoleSet %s/%s healthy gang conditions: %w; latest observation: %s",
			namespace, name, err, latest,
		)
	}
	return nil
}

func stormServiceGangHealthy(stormService *orchestrationv1alpha1.StormService) bool {
	if !stormServiceReadyAtCurrentGeneration(stormService) {
		return false
	}
	synced := condition(stormService.Status.Conditions, orchestrationv1alpha1.StormServicePodGroupSynced)
	healthy := condition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceGangSchedulingError)
	return synced != nil && synced.Status == corev1.ConditionTrue && synced.Reason == "PodGroupSynced" &&
		healthy != nil && healthy.Status == corev1.ConditionFalse && healthy.Reason == "GangSchedulingHealthy"
}
