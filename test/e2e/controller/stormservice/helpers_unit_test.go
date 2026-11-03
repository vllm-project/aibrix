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

//nolint:lll // Compact Kubernetes fixtures are clearer when their expected fields remain together.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixfake "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/fake"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
	stormservicecontroller "github.com/vllm-project/aibrix/pkg/controller/stormservice"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRequireConsistentForPollsForEntireWindow(t *testing.T) {
	checks := 0
	err := requireConsistentFor(context.Background(), time.Millisecond, 5*time.Millisecond, func(context.Context) error {
		checks++
		return nil
	})
	if err != nil {
		t.Fatalf("require consistent for window: %v", err)
	}
	if checks < 2 {
		t.Fatalf("consistency checks = %d, want at least 2", checks)
	}
}

func TestStormServiceClientsAcceptKubeconfigPathList(t *testing.T) {
	config := []byte(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test
`)
	first := filepath.Join(t.TempDir(), "first-kubeconfig")
	second := filepath.Join(t.TempDir(), "second-kubeconfig")
	if err := os.WriteFile(first, config, 0o600); err != nil {
		t.Fatalf("write first kubeconfig: %v", err)
	}
	if err := os.WriteFile(second, config, 0o600); err != nil {
		t.Fatalf("write second kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", strings.Join([]string{first, second}, string(os.PathListSeparator)))

	stormServiceClients(t, stormServiceE2EDefaultNamespace)
}

func TestRequireConsistentForStopsOnFailedCheck(t *testing.T) {
	want := fmt.Errorf("pod changed while paused")
	err := requireConsistentFor(context.Background(), time.Millisecond, time.Second, func(context.Context) error {
		return want
	})
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("require consistent for error = %v, want %v", err, want)
	}
}

func TestPausedUpdateRemainsUnchanged(t *testing.T) {
	roleSetUID := types.UID("roleset-uid")
	podUID := types.UID("pod-uid")
	roleSets := []unstructured.Unstructured{{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"uid": string(roleSetUID)},
	}}}
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{UID: podUID},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: stormServiceWorkerContainerName, Image: stormServiceInPlaceImageV1,
		}}},
	}}

	if !pausedUpdateRemainsUnchanged(roleSets, pods, roleSetUID, podUID, stormServiceInPlaceImageV1) {
		t.Fatal("expected paused update observations to preserve RoleSet, pod, and image")
	}

	pods[0].Spec.Containers[0].Image = stormServiceInPlaceImageV2
	if pausedUpdateRemainsUnchanged(roleSets, pods, roleSetUID, podUID, stormServiceInPlaceImageV1) {
		t.Fatal("expected changed pod image to fail paused consistency predicate")
	}
}

func TestPodCompletedInPlaceUpdate(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:    types.UID("pod-uid"),
			Labels: map[string]string{controllerconstants.RoleTemplateHashLabelKey: "new-hash"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: stormServiceWorkerContainerName, Image: stormServiceInPlaceImageV2,
		}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	if !podCompletedInPlaceUpdate(pod, pod.UID, "old-hash") {
		t.Fatal("expected completed in-place update")
	}
	pod.Annotations = map[string]string{controllerconstants.RoleInPlaceUpdateTargetHashAnnotationKey: "new-hash"}
	if podCompletedInPlaceUpdate(pod, pod.UID, "old-hash") {
		t.Fatal("target annotation must be cleared before completion")
	}
}

func TestPodCompletedFallbackUpdate(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("replacement")},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:    stormServiceWorkerContainerName,
			Image:   stormServiceInPlaceImageV2,
			Command: []string{"sh", "-c", "sleep 3600"},
		}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	if !podCompletedFallbackUpdate(pod, types.UID("original")) {
		t.Fatal("expected completed fallback update")
	}
	pod.UID = types.UID("original")
	if podCompletedFallbackUpdate(pod, types.UID("original")) {
		t.Fatal("fallback must replace the pod UID")
	}
}

func TestStormServiceHasReplicaStatus(t *testing.T) {
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
		Status: orchestrationv1alpha1.StormServiceStatus{
			ObservedGeneration:   4,
			Replicas:             2,
			ReadyReplicas:        2,
			UpdatedReplicas:      2,
			UpdatedReadyReplicas: 2,
			NotReadyReplicas:     0,
			CurrentRevision:      "storm-abc",
			UpdateRevision:       "storm-abc",
			Conditions:           orchestrationv1alpha1.Conditions{{Type: orchestrationv1alpha1.StormServiceReady, Status: corev1.ConditionTrue, Reason: "Ready"}},
			RoleStatuses:         []orchestrationv1alpha1.RoleStatus{{Name: stormServiceWorkerRoleName, ReadyReplicas: 2}},
		},
	}

	if !stormServiceHasReplicaStatus(stormService, 2, "storm-abc") {
		t.Fatal("expected complete ready replica status to match")
	}

	stormService.Status.UpdatedReadyReplicas = 1
	if stormServiceHasReplicaStatus(stormService, 2, "storm-abc") {
		t.Fatal("expected incomplete ready replica status not to match")
	}

	stormService.Status.UpdatedReadyReplicas = 2
	stormService.Status.RoleStatuses[0].ReadyReplicas = 1
	if stormServiceHasReplicaStatus(stormService, 2, "storm-abc") {
		t.Fatal("expected incomplete worker role status not to match")
	}
}

func TestRoleSetHasStormServiceOwnerAndMetadata(t *testing.T) {
	stormServiceUID := types.UID("storm-uid")
	roleSet := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				controllerconstants.StormServiceRevisionLabelKey: "storm-abc",
			},
			"annotations": map[string]interface{}{
				controllerconstants.RoleSetIndexAnnotationKey:    "0",
				controllerconstants.RoleSetRevisionAnnotationKey: "storm-abc",
			},
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": orchestrationv1alpha1.GroupVersion.String(),
				"kind":       orchestrationv1alpha1.StormServiceKind,
				"uid":        string(stormServiceUID),
				"controller": true,
			}},
		},
	}}

	if !roleSetHasStormServiceOwnerAndMetadata(roleSet, stormServiceUID) {
		t.Fatal("expected RoleSet with owner and required metadata to match")
	}

	roleSet.SetAnnotations(map[string]string{controllerconstants.RoleSetIndexAnnotationKey: "0"})
	if roleSetHasStormServiceOwnerAndMetadata(roleSet, stormServiceUID) {
		t.Fatal("expected RoleSet without revision metadata not to match")
	}

	roleSet.SetAnnotations(map[string]string{
		controllerconstants.RoleSetIndexAnnotationKey:    "0",
		controllerconstants.RoleSetRevisionAnnotationKey: "storm-abc",
	})
	roleSet.SetLabels(nil)
	if roleSetHasStormServiceOwnerAndMetadata(roleSet, stormServiceUID) {
		t.Fatal("expected RoleSet without StormService revision label not to match")
	}
}

func TestNewUpdateStormServiceUsesPooledInPlaceFixture(t *testing.T) {
	stormService := newUpdateStormService("default", "update")

	if stormService.Spec.Replicas == nil || *stormService.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want explicit 1", stormService.Spec.Replicas)
	}
	if stormService.Spec.Mode != orchestrationv1alpha1.StormServicePooledMode {
		t.Fatalf("mode = %q, want %q", stormService.Spec.Mode, orchestrationv1alpha1.StormServicePooledMode)
	}
	if stormService.Spec.UpdateStrategy.Type != orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType {
		t.Fatalf("StormService update strategy = %q, want %q", stormService.Spec.UpdateStrategy.Type, orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType)
	}
	if stormService.Spec.Template.Spec.UpdateStrategy != orchestrationv1alpha1.ParallelRoleSetUpdateStrategyType {
		t.Fatalf("RoleSet update strategy = %q, want %q", stormService.Spec.Template.Spec.UpdateStrategy, orchestrationv1alpha1.ParallelRoleSetUpdateStrategyType)
	}
	if role := stormService.Spec.Template.Spec.Roles[0]; role.UpdateStrategy.Type != orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType {
		t.Fatalf("role update strategy = %q, want %q", role.UpdateStrategy.Type, orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType)
	}
}

func TestNewDeadlineStormServiceUsesSingleReplicaRollingFixture(t *testing.T) {
	stormService := newDeadlineStormService("default", "deadline", 15)
	if stormService.Spec.Replicas == nil || *stormService.Spec.Replicas != 1 {
		t.Fatalf("deadline replicas = %v, want 1", stormService.Spec.Replicas)
	}
	if stormService.Spec.Mode != orchestrationv1alpha1.StormServiceReplicaMode {
		t.Fatalf("deadline mode = %q, want Replica", stormService.Spec.Mode)
	}
	if stormService.Spec.ProgressDeadlineSeconds == nil || *stormService.Spec.ProgressDeadlineSeconds != 15 {
		t.Fatalf("deadline seconds = %v, want 15", stormService.Spec.ProgressDeadlineSeconds)
	}
	if stormService.Spec.UpdateStrategy.Type != orchestrationv1alpha1.RollingUpdateStormServiceStrategyType {
		t.Fatalf("deadline update strategy = %q, want RollingUpdate", stormService.Spec.UpdateStrategy.Type)
	}
}

func TestStormServiceDeadlineAndRecoveryPredicates(t *testing.T) {
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status: orchestrationv1alpha1.StormServiceStatus{
			ObservedGeneration: 3,
			Conditions: orchestrationv1alpha1.Conditions{{
				Type: orchestrationv1alpha1.StormServiceProgressing, Status: corev1.ConditionFalse, Reason: stormservicecontroller.ProgressDeadlineExceededReason,
			}},
		},
	}
	if !stormServiceHasProgressDeadlineExceeded(stormService) {
		t.Fatal("expected progress deadline predicate to match")
	}

	stormService.Status = orchestrationv1alpha1.StormServiceStatus{
		ObservedGeneration:   3,
		Replicas:             1,
		ReadyReplicas:        1,
		UpdatedReplicas:      1,
		UpdatedReadyReplicas: 1,
		CurrentRevision:      "revision",
		UpdateRevision:       "revision",
		Conditions: orchestrationv1alpha1.Conditions{{
			Type: orchestrationv1alpha1.StormServiceReady, Status: corev1.ConditionTrue, Reason: "Ready",
		}},
	}
	if !stormServiceRecoveredFromDeadline(stormService) {
		t.Fatal("expected recovered deadline predicate to match")
	}
}

func TestEligibleNodeCount(t *testing.T) {
	nodes := []corev1.Node{
		{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		{Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Effect: corev1.TaintEffectNoSchedule}}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		{Spec: corev1.NodeSpec{Unschedulable: true}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
		{Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Effect: corev1.TaintEffectPreferNoSchedule}}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
	}
	if got := eligibleNodeCount(nodes); got != 2 {
		t.Fatalf("eligible node count = %d, want 2", got)
	}
}

func TestNewVolcanoStormServiceShapes(t *testing.T) {
	impossible := newVolcanoStormService("default", "impossible", true, 2)
	role := impossible.Spec.Template.Spec.Roles[0]
	strategy := impossible.Spec.Template.Spec.SchedulingStrategy.VolcanoSchedulingStrategy
	if role.Replicas == nil || *role.Replicas != 3 || strategy.MinMember != 3 || strategy.MinTaskMember[stormServiceWorkerRoleName] != 3 {
		t.Fatalf("impossible gang shape: role replicas=%v strategy=%+v", role.Replicas, strategy)
	}
	if role.Template.Spec.Affinity == nil || role.Template.Spec.Affinity.PodAntiAffinity == nil ||
		len(role.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("impossible gang missing required pod anti-affinity: %+v", role.Template.Spec.Affinity)
	}
	if role.Template.Spec.Containers[0].Resources.Requests.Cpu().IsZero() {
		t.Fatal("impossible gang must request non-zero CPU")
	}

	feasible := newVolcanoStormService("default", "feasible", false, 1)
	strategy = feasible.Spec.Template.Spec.SchedulingStrategy.VolcanoSchedulingStrategy
	if len(feasible.Spec.Template.Spec.Roles) != 2 || strategy.MinMember != 2 ||
		strategy.MinTaskMember["prefill"] != 1 || strategy.MinTaskMember["decode"] != 1 {
		t.Fatalf("feasible gang shape: roles=%+v strategy=%+v", feasible.Spec.Template.Spec.Roles, strategy)
	}
}

func TestVolcanoPodsReadyAndMarked(t *testing.T) {
	roleSetName := "roleset-generated"
	pods := []corev1.Pod{
		volcanoReadyPod("prefill", roleSetName),
		volcanoReadyPod("decode", roleSetName),
	}
	if !volcanoPodsReadyAndMarked(pods, roleSetName) {
		t.Fatal("expected ready Volcano pod markers")
	}
	pods[0].Spec.SchedulerName = "default-scheduler"
	if volcanoPodsReadyAndMarked(pods, roleSetName) {
		t.Fatal("wrong scheduler must fail marker predicate")
	}
}

func TestVolcanoPodsMarkedDoesNotRequireReady(t *testing.T) {
	roleSetName := "roleset-generated"
	pods := []corev1.Pod{
		volcanoReadyPod(stormServiceWorkerRoleName, roleSetName),
		volcanoReadyPod(stormServiceWorkerRoleName, roleSetName),
	}
	for i := range pods {
		pods[i].Status.Conditions = nil
	}
	if !volcanoPodsMarked(
		pods,
		roleSetName,
		map[string]int{stormServiceWorkerRoleName: 2},
	) {
		t.Fatal("expected scheduler and gang markers without Pod readiness")
	}
	if volcanoPodsReadyAndMarked(pods, roleSetName) {
		t.Fatal("ready predicate must still require Pod readiness")
	}
}

func TestVolcanoPodGroupConfigured(t *testing.T) {
	roleSetUID := types.UID("roleset-uid")
	podGroup := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{controllerconstants.RoleSetNameLabelKey: "roleset"},
			"ownerReferences": []interface{}{map[string]interface{}{
				"uid": string(roleSetUID), "controller": true,
			}},
		},
		"spec": map[string]interface{}{
			"minMember": int64(3),
			"minTaskMember": map[string]interface{}{
				stormServiceWorkerRoleName: int64(3),
			},
			"queue": stormServiceVolcanoDefaultQueue,
		},
	}}

	if !volcanoPodGroupConfigured(
		podGroup,
		"roleset",
		roleSetUID,
		3,
		map[string]int32{stormServiceWorkerRoleName: 3},
	) {
		t.Fatal("expected matching blocked PodGroup configuration")
	}
	podGroup.Object["spec"].(map[string]interface{})["minMember"] = int64(2)
	if volcanoPodGroupConfigured(
		podGroup,
		"roleset",
		roleSetUID,
		3,
		map[string]int32{stormServiceWorkerRoleName: 3},
	) {
		t.Fatal("incorrect minMember must not match")
	}
}

func volcanoReadyPod(roleName, roleSetName string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				controllerconstants.VolcanoPodGroupNameAnnotationKey: roleSetName,
				controllerconstants.VolcanoTaskSpecKey:               roleName,
			},
			Annotations: map[string]string{
				controllerconstants.VolcanoPodGroupNameAnnotationKey: roleSetName,
				controllerconstants.VolcanoTaskSpecKey:               roleName,
			},
		},
		Spec: corev1.PodSpec{SchedulerName: "volcano"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
}

func TestStormServiceHarnessRecordsRoleSetNamesAcrossObservations(t *testing.T) {
	harness := &stormServiceHarness{}
	harness.recordRoleSets("storm", []unstructured.Unstructured{
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "storm-roleset-a"}}},
	})
	harness.recordRoleSets("storm", []unstructured.Unstructured{
		{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "storm-roleset-b"}}},
	})

	names := harness.recordedRoleSetNames("storm")
	_, hasFirst := names["storm-roleset-a"]
	_, hasSecond := names["storm-roleset-b"]
	if len(names) != 2 || !hasFirst || !hasSecond {
		t.Fatalf("recorded RoleSet names = %v, want storm-roleset-a and storm-roleset-b", names)
	}
}

func TestStormServiceHarnessWaitForRoleSetsRecordsObservedNames(t *testing.T) {
	roleSet := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "orchestration.aibrix.ai/v1alpha1",
		"kind":       "RoleSet",
		"metadata": map[string]interface{}{
			"name":      "storm-roleset-a",
			"namespace": "default",
			"labels": map[string]interface{}{
				"storm-service-name": "storm",
			},
		},
	}}
	harness := &stormServiceHarness{
		namespace: "default",
		dynamicClient: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{roleSetGVR: "RoleSetList"},
			roleSet,
		),
	}

	if _, err := harness.waitForRoleSets(context.Background(), "storm", 1); err != nil {
		t.Fatalf("wait for RoleSets: %v", err)
	}
	if _, found := harness.recordedRoleSetNames("storm")["storm-roleset-a"]; !found {
		t.Fatalf("harness did not record RoleSet observed through waitForRoleSets")
	}
}

func TestWaitForOwnedServiceFailsImmediatelyForForbidden(t *testing.T) {
	kubeClient := k8sfake.NewSimpleClientset()
	getCalls := 0
	kubeClient.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "storm", fmt.Errorf("denied"))
	})

	_, err := waitForOwnedService(context.Background(), kubeClient, "default", "storm", types.UID("storm-uid"))
	if !apierrors.IsForbidden(err) {
		t.Fatalf("wait error = %v, want Forbidden", err)
	}
	if getCalls != 1 {
		t.Fatalf("get calls = %d, want 1 for a permanent error", getCalls)
	}
}

func TestWaitForOwnedServiceRetriesNotFound(t *testing.T) {
	ownerUID := types.UID("storm-uid")
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      "storm",
		Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{
			UID: ownerUID,
		}},
	}}
	kubeClient := k8sfake.NewSimpleClientset(service)
	getCalls := 0
	kubeClient.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, "storm")
		}
		return false, nil, nil
	})

	if _, err := waitForOwnedService(context.Background(), kubeClient, "default", "storm", ownerUID); err != nil {
		t.Fatalf("wait for owned Service: %v", err)
	}
	if getCalls != 2 {
		t.Fatalf("get calls = %d, want 2 after NotFound retry", getCalls)
	}
}

func TestWaitForOwnedServiceRetriesTransientError(t *testing.T) {
	ownerUID := types.UID("storm-uid")
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      "storm",
		Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{
			UID: ownerUID,
		}},
	}}
	kubeClient := k8sfake.NewSimpleClientset(service)
	getCalls := 0
	kubeClient.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, nil, apierrors.NewServiceUnavailable("temporarily unavailable")
		}
		return false, nil, nil
	})

	if _, err := waitForOwnedService(context.Background(), kubeClient, "default", "storm", ownerUID); err != nil {
		t.Fatalf("wait for owned Service: %v", err)
	}
	if getCalls != 2 {
		t.Fatalf("get calls = %d, want 2 after transient retry", getCalls)
	}
}

func TestWaitForServiceNotFoundRetriesUntilStrictAbsence(t *testing.T) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "storm", Namespace: "default"}}
	kubeClient := k8sfake.NewSimpleClientset()
	getCalls := 0
	kubeClient.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, service, nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, "storm")
	})

	if err := waitForServiceNotFound(context.Background(), kubeClient, "default", "storm"); err != nil {
		t.Fatalf("wait for strict Service absence: %v", err)
	}
	if getCalls != 2 {
		t.Fatalf("get calls = %d, want 2 after Service was deleted", getCalls)
	}
}

func TestCleanupStormServiceContinuesAfterIdentityListFailure(t *testing.T) {
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		roleSetGVR:         "RoleSetList",
		podSetGVR:          "PodSetList",
		volcanoPodGroupGVR: "PodGroupList",
	})
	roleSetListCalls := 0
	dynamicClient.PrependReactor("list", "rolesets", func(k8stesting.Action) (bool, runtime.Object, error) {
		roleSetListCalls++
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: roleSetGVR.Group, Resource: roleSetGVR.Resource}, "", fmt.Errorf("denied"))
	})

	kubeClient := k8sfake.NewSimpleClientset()
	podListCalls := 0
	kubeClient.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		podListCalls++
		return false, nil, nil
	})
	aibrixClient := aibrixfake.NewSimpleClientset()
	deleteCalls := 0
	aibrixClient.PrependReactor("delete", "stormservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		return false, nil, nil
	})

	harness := &stormServiceHarness{
		namespace:     "default",
		kubeClient:    kubeClient,
		stormServices: aibrixClient.OrchestrationV1alpha1().StormServices("default"),
		dynamicClient: dynamicClient,
	}
	err := harness.cleanupStormServiceResources(context.Background(), "storm", false)
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("cleanup error = %v, want joined Forbidden identity-list error", err)
	}
	if roleSetListCalls == 0 {
		t.Fatal("cleanup did not attempt the RoleSet identity list")
	}
	if deleteCalls != 1 {
		t.Fatalf("StormService delete calls = %d, want 1 despite identity-list failure", deleteCalls)
	}
	if podListCalls == 0 {
		t.Fatal("cleanup did not continue to the Pod check after identity-list failure")
	}
}

func TestStrictStormServiceCleanupRetriesAfterFailedAttempt(t *testing.T) {
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		roleSetGVR: "RoleSetList",
		podSetGVR:  "PodSetList",
	})
	roleSetListCalls := 0
	dynamicClient.PrependReactor("list", "rolesets", func(k8stesting.Action) (bool, runtime.Object, error) {
		roleSetListCalls++
		if roleSetListCalls == 1 {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: roleSetGVR.Group, Resource: roleSetGVR.Resource}, "", fmt.Errorf("denied"))
		}
		return false, nil, nil
	})

	kubeClient := k8sfake.NewSimpleClientset()
	aibrixClient := aibrixfake.NewSimpleClientset()
	deleteCalls := 0
	aibrixClient.PrependReactor("delete", "stormservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		return true, nil, nil
	})

	harness := &stormServiceHarness{
		namespace:     "default",
		kubeClient:    kubeClient,
		stormServices: aibrixClient.OrchestrationV1alpha1().StormServices("default"),
		dynamicClient: dynamicClient,
	}
	cleanup := newStrictStormServiceCleanup(harness, "storm", false)

	if err := cleanup.run(context.Background()); err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("first cleanup error = %v, want Forbidden", err)
	}
	if cleanup.completed {
		t.Fatal("cleanup was marked complete after a failed attempt")
	}
	if err := cleanup.run(context.Background()); err != nil {
		t.Fatalf("retry strict cleanup: %v", err)
	}
	if !cleanup.completed {
		t.Fatal("cleanup was not marked complete after a successful retry")
	}
	if deleteCalls != 2 {
		t.Fatalf("StormService delete calls = %d, want 2 across the failed attempt and retry", deleteCalls)
	}
}
