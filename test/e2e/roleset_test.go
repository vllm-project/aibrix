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

	"github.com/stretchr/testify/require"
	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
	draincontroller "github.com/vllm-project/aibrix/pkg/controller/drain"
	rolesetcontroller "github.com/vllm-project/aibrix/pkg/controller/roleset"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

const (
	roleSetInPlaceE2EEnv        = "AIBRIX_ROLESET_INPLACE_E2E"
	roleSetInPlaceE2EKeepEnv    = "AIBRIX_ROLESET_INPLACE_E2E_KEEP_ON_FAILURE"
	roleSetInPlaceNamespace     = "default"
	roleSetInPlaceRoleName      = "worker"
	roleSetInPlaceContainerName = "worker"
	roleSetInPlaceImageV1       = "aibrix/inplace-e2e:v1"
	roleSetInPlaceImageV2       = "aibrix/inplace-e2e:v2"
	roleSetInPlaceMissingImage  = "aibrix/inplace-e2e:missing"

	roleSetDrainE2EEnv = "AIBRIX_ROLESET_DRAIN_E2E"

	roleSetHistoricalNodeE2EEnv                  = "AIBRIX_ROLESET_HISTORICAL_NODE_E2E"
	roleSetHistoricalNodeControllerNamespaceEnv  = "AIBRIX_ROLESET_CONTROLLER_NAMESPACE"
	roleSetHistoricalNodeControllerDeploymentEnv = "AIBRIX_ROLESET_CONTROLLER_DEPLOYMENT"
	roleSetHistoricalNodeControllerNamespace     = "aibrix-system"
	roleSetHistoricalNodeControllerDeployment    = "controller-manager"
)

var (
	roleSetGVR = schema.GroupVersionResource{Group: "orchestration.aibrix.ai", Version: "v1alpha1", Resource: "rolesets"}
	podSetGVR  = schema.GroupVersionResource{Group: "orchestration.aibrix.ai", Version: "v1alpha1", Resource: "podsets"}
)

func TestRoleSetInPlaceUpdate(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(envOrDefault(roleSetInPlaceE2EEnv, "false"))) != e2eEnabledValue {
		t.Skipf("set %s=true to run RoleSet in-place update e2e tests", roleSetInPlaceE2EEnv)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8sClient, dynamicClient := roleSetInPlaceClients(t)

	t.Run("image-only update keeps pod identity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()

		name := "roleset-inplace-success"
		cleanupRoleSetInPlaceE2E(ctx, t, k8sClient, dynamicClient, name)
		defer cleanupRoleSetInPlaceE2EAfterTest(ctx, t, k8sClient, dynamicClient, name)

		createRoleSet(ctx, t, dynamicClient, name, roleSetInPlaceImageV1, nil, 1)
		initialPod := waitForSingleRoleSetPodReady(ctx, t, k8sClient, name)
		initialUID := initialPod.UID
		initialImageID := containerImageID(t, initialPod, roleSetInPlaceContainerName)
		initialHash := initialPod.Labels[controllerconstants.RoleTemplateHashLabelKey]

		updateRoleSetContainerImage(ctx, t, dynamicClient, name, roleSetInPlaceImageV2)
		finalPod := waitForSingleRoleSetPod(ctx, t, k8sClient, name, func(pod *corev1.Pod) bool {
			return pod.UID == initialUID &&
				pod.Spec.Containers[0].Image == roleSetInPlaceImageV2 &&
				pod.Labels[controllerconstants.RoleTemplateHashLabelKey] != initialHash &&
				pod.Annotations[controllerconstants.RoleInPlaceUpdateTargetHashAnnotationKey] == "" &&
				pod.Annotations[controllerconstants.RoleInPlaceUpdateStateAnnotationKey] == "" &&
				pod.Annotations[controllerconstants.RoleInPlaceUpdatePendingReasonAnnotationKey] == ""
		})
		require.NotEqual(t, initialImageID, containerImageID(t, finalPod, roleSetInPlaceContainerName))
		waitForRoleSetEvent(ctx, t, k8sClient, name, rolesetcontroller.InPlaceUpdateStartedEventType)
		waitForRoleSetEvent(ctx, t, k8sClient, name, rolesetcontroller.InPlaceUpdateCompletedEventType)
	})

	t.Run("non-image change falls back to recreate", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()

		name := "roleset-inplace-fallback"
		cleanupRoleSetInPlaceE2E(ctx, t, k8sClient, dynamicClient, name)
		defer cleanupRoleSetInPlaceE2EAfterTest(ctx, t, k8sClient, dynamicClient, name)

		createRoleSet(ctx, t, dynamicClient, name, roleSetInPlaceImageV1, nil, 1)
		initialPod := waitForSingleRoleSetPodReady(ctx, t, k8sClient, name)

		updateRoleSetCommand(ctx, t, dynamicClient, name, []string{"sh", "-c", "sleep 3600"})
		replacementPod := waitForSingleRoleSetPod(ctx, t, k8sClient, name, func(pod *corev1.Pod) bool {
			return pod.UID != initialPod.UID &&
				pod.Annotations[controllerconstants.RoleInPlaceUpdateTargetHashAnnotationKey] == "" &&
				len(pod.Spec.Containers[0].Command) == 3 &&
				pod.Spec.Containers[0].Command[2] == "sleep 3600"
		})
		require.NotEqual(t, initialPod.Name, replacementPod.Name)
		waitForRoleSetEvent(ctx, t, k8sClient, name, rolesetcontroller.InPlaceFallbackEventType)
	})

	t.Run("stuck update exposes pending reason", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()

		name := "roleset-inplace-stuck"
		cleanupRoleSetInPlaceE2E(ctx, t, k8sClient, dynamicClient, name)
		defer cleanupRoleSetInPlaceE2EAfterTest(ctx, t, k8sClient, dynamicClient, name)

		createRoleSet(ctx, t, dynamicClient, name, roleSetInPlaceImageV1, nil, 1)
		initialPod := waitForSingleRoleSetPodReady(ctx, t, k8sClient, name)
		initialHash := initialPod.Labels[controllerconstants.RoleTemplateHashLabelKey]

		updateRoleSetContainerImage(ctx, t, dynamicClient, name, roleSetInPlaceMissingImage)
		stuckPod := waitForSingleRoleSetPod(ctx, t, k8sClient, name, func(pod *corev1.Pod) bool {
			reason := pod.Annotations[controllerconstants.RoleInPlaceUpdatePendingReasonAnnotationKey]
			return pod.UID == initialPod.UID &&
				pod.Labels[controllerconstants.RoleTemplateHashLabelKey] == initialHash &&
				pod.Annotations[controllerconstants.RoleInPlaceUpdateTargetHashAnnotationKey] != "" &&
				isExpectedInPlacePendingReason(reason)
		})
		t.Logf(
			"observed stuck in-place update reason: %s",
			stuckPod.Annotations[controllerconstants.RoleInPlaceUpdatePendingReasonAnnotationKey],
		)
	})

	t.Run("podset role falls back to recreate", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()

		name := "roleset-inplace-podset-fallback"
		cleanupRoleSetInPlaceE2E(ctx, t, k8sClient, dynamicClient, name)
		defer cleanupRoleSetInPlaceE2EAfterTest(ctx, t, k8sClient, dynamicClient, name)

		podGroupSize := int32(2)
		createRoleSet(ctx, t, dynamicClient, name, roleSetInPlaceImageV1, &podGroupSize, 1)
		initialPodSet := waitForSingleRoleSetPodSet(
			ctx,
			t,
			dynamicClient,
			name,
			func(podSet *unstructured.Unstructured) bool {
				return podSet.GetLabels()[controllerconstants.RoleTemplateHashLabelKey] != ""
			},
		)
		initialUID := initialPodSet.GetUID()
		initialHash := initialPodSet.GetLabels()[controllerconstants.RoleTemplateHashLabelKey]
		waitForRoleSetPodsReady(ctx, t, k8sClient, name, int(podGroupSize))

		updateRoleSetContainerImage(ctx, t, dynamicClient, name, roleSetInPlaceImageV2)
		replacementPodSet := waitForSingleRoleSetPodSet(
			ctx,
			t,
			dynamicClient,
			name,
			func(podSet *unstructured.Unstructured) bool {
				image := podSetContainerImage(podSet)
				return podSet.GetUID() != initialUID &&
					podSet.GetLabels()[controllerconstants.RoleTemplateHashLabelKey] != initialHash &&
					image == roleSetInPlaceImageV2 &&
					podSet.GetAnnotations()[controllerconstants.RoleInPlaceUpdateTargetHashAnnotationKey] == ""
			},
		)
		require.NotEqual(t, initialUID, replacementPodSet.GetUID())
		waitForRoleSetEvent(ctx, t, k8sClient, name, rolesetcontroller.InPlaceFallbackEventType)
	})
}

func TestRoleSetDrainScaleDown(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(envOrDefault(roleSetDrainE2EEnv, "false"))) != e2eEnabledValue {
		t.Skipf("set %s=true to run RoleSet drain e2e tests", roleSetDrainE2EEnv)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8sClient, dynamicClient := roleSetInPlaceClients(t)

	t.Run("scale down marks pod draining before deletion", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()

		name := "roleset-drain-scale-down"
		drainTimeoutSeconds := int32(5)
		cleanupRoleSetInPlaceE2E(ctx, t, k8sClient, dynamicClient, name)
		defer cleanupRoleSetInPlaceE2EAfterTest(ctx, t, k8sClient, dynamicClient, name)

		createDrainRoleSet(ctx, t, dynamicClient, name, roleSetInPlaceImageV1, 2, drainTimeoutSeconds)
		waitForRoleSetPodsReady(ctx, t, k8sClient, name, 2)

		updateRoleSetReplicas(ctx, t, dynamicClient, name, 1)

		drainingPod := waitForRoleSetPodMatching(ctx, t, k8sClient, name, func(pod *corev1.Pod) bool {
			annotations := pod.GetAnnotations()
			return annotations[aibrixconst.PodDrainingAnnotationKey] == "true" &&
				annotations[aibrixconst.PodDrainStartTimeAnnotationKey] != "" &&
				annotations[aibrixconst.PodDrainReasonAnnotationKey] == aibrixconst.PodDrainReasonScaleIn &&
				annotations[aibrixconst.PodDrainTargetActionAnnotationKey] == aibrixconst.PodDrainTargetActionDelete
		})
		require.Nil(t, drainingPod.DeletionTimestamp)

		drainHoldUntil := time.Now().Add(2 * time.Second)
		require.NoError(t, wait.PollUntilContextTimeout(
			ctx,
			500*time.Millisecond,
			3*time.Second,
			true,
			func(ctx context.Context) (bool, error) {
				if time.Now().After(drainHoldUntil) {
					return true, nil
				}
				current, err := k8sClient.CoreV1().Pods(roleSetInPlaceNamespace).Get(ctx, drainingPod.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if current.DeletionTimestamp != nil {
					return false, fmt.Errorf("draining pod %s was deleted before drain timeout elapsed", current.Name)
				}
				return false, nil
			},
		))

		waitForRoleSetPodsReady(ctx, t, k8sClient, name, 1)
		waitForRoleSetEvent(ctx, t, k8sClient, name, draincontroller.EventStarted)
		waitForRoleSetEvent(ctx, t, k8sClient, name, draincontroller.EventCompleted)
	})
}

func TestRoleSetHistoricalNodeReplacementScheduling(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(envOrDefault(roleSetHistoricalNodeE2EEnv, "false"))) != e2eEnabledValue {
		t.Skipf("set %s=true to run RoleSet historical-node replacement scheduling e2e tests", roleSetHistoricalNodeE2EEnv)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8sClient, dynamicClient := roleSetInPlaceClients(t)

	t.Run("stateful restart keeps slot history", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		name := "roleset-history-stateful"
		cleanupRoleSetInPlaceE2E(ctx, t, k8sClient, dynamicClient, name)
		defer cleanupRoleSetInPlaceE2EAfterTest(ctx, t, k8sClient, dynamicClient, name)

		createHistoricalNodeRoleSet(ctx, t, dynamicClient, name, true, 0, 1)
		initialPod := waitForSingleRoleSetPodReady(ctx, t, k8sClient, name)
		initialNode := initialPod.Spec.NodeName
		require.NotEmpty(t, initialNode)
		waitForRoleSetHistoricalNodeAnnotation(ctx, t, dynamicClient, name, initialNode)

		restartRoleSetController(ctx, t, k8sClient)
		updateRoleSetContainerImage(ctx, t, dynamicClient, name, roleSetInPlaceImageV2)
		require.NoError(t, k8sClient.CoreV1().Pods(roleSetInPlaceNamespace).Delete(ctx, initialPod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](0),
		}))

		replacementPod := waitForSingleRoleSetPod(ctx, t, k8sClient, name, func(pod *corev1.Pod) bool {
			return pod.UID != initialPod.UID &&
				pod.Spec.Containers[0].Image == roleSetInPlaceImageV2 &&
				hasHistoricalNodeAffinity(pod, initialNode)
		})
		require.NotEqual(t, initialPod.Name, replacementPod.Name)
	})

	t.Run("stateless restart keeps recent role history", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		name := "roleset-history-stateless"
		cleanupRoleSetInPlaceE2E(ctx, t, k8sClient, dynamicClient, name)
		defer cleanupRoleSetInPlaceE2EAfterTest(ctx, t, k8sClient, dynamicClient, name)

		createHistoricalNodeRoleSet(ctx, t, dynamicClient, name, false, 1, 0)
		initialPod := waitForSingleRoleSetPodReady(ctx, t, k8sClient, name)
		initialNode := initialPod.Spec.NodeName
		require.NotEmpty(t, initialNode)
		waitForRoleSetHistoricalNodeAnnotation(ctx, t, dynamicClient, name, initialNode)

		restartRoleSetController(ctx, t, k8sClient)
		updateRoleSetContainerImage(ctx, t, dynamicClient, name, roleSetInPlaceImageV2)

		waitForRoleSetPodMatching(ctx, t, k8sClient, name, func(pod *corev1.Pod) bool {
			return pod.UID != initialPod.UID &&
				pod.Spec.Containers[0].Image == roleSetInPlaceImageV2 &&
				hasHistoricalNodeAffinity(pod, initialNode)
		})
	})
}

func roleSetInPlaceClients(t *testing.T) (*kubernetes.Clientset, dynamic.Interface) {
	t.Helper()

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeConfig := os.Getenv("KUBECONFIG"); kubeConfig != "" {
		loadingRules.ExplicitPath = kubeConfig
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	require.NoError(t, err)

	k8sClient, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	dynamicClient, err := dynamic.NewForConfig(config)
	require.NoError(t, err)
	return k8sClient, dynamicClient
}

func createRoleSet(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	name, image string,
	podGroupSize *int32,
	replicas int32,
) {
	t.Helper()

	maxSurge := intstr.FromInt32(0)
	maxUnavailable := intstr.FromInt32(1)
	role := orchestrationapi.RoleSpec{
		Name:         roleSetInPlaceRoleName,
		Replicas:     ptr.To(replicas),
		PodGroupSize: podGroupSize,
		UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
			Type:           orchestrationapi.InPlaceIfPossibleRoleUpdateStrategyType,
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": name},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:            roleSetInPlaceContainerName,
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
				}},
			},
		},
	}
	roleSet := &orchestrationapi.RoleSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "orchestration.aibrix.ai/v1alpha1",
			Kind:       "RoleSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: roleSetInPlaceNamespace,
		},
		Spec: orchestrationapi.RoleSetSpec{
			UpdateStrategy: orchestrationapi.ParallelRoleSetUpdateStrategyType,
			Roles:          []orchestrationapi.RoleSpec{role},
		},
	}

	objMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(roleSet)
	require.NoError(t, err)
	obj := &unstructured.Unstructured{Object: objMap}
	_, err = dynamicClient.Resource(roleSetGVR).Namespace(roleSetInPlaceNamespace).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err)
}

func createHistoricalNodeRoleSet(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	name string,
	stateful bool,
	maxSurgeValue, maxUnavailableValue int32,
) {
	t.Helper()

	maxSurge := intstr.FromInt32(maxSurgeValue)
	maxUnavailable := intstr.FromInt32(maxUnavailableValue)
	role := orchestrationapi.RoleSpec{
		Name:     roleSetInPlaceRoleName,
		Replicas: ptr.To[int32](1),
		Stateful: stateful,
		UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
			Type:           orchestrationapi.RecreateRoleUpdateStrategyType,
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
			ReplacementScheduling: &orchestrationapi.RoleReplacementScheduling{
				HistoricalNode: &orchestrationapi.HistoricalNodeSchedulingPolicy{
					Mode: orchestrationapi.HistoricalNodeSchedulingPreferred,
				},
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": name},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:            roleSetInPlaceContainerName,
					Image:           roleSetInPlaceImageV1,
					ImagePullPolicy: corev1.PullIfNotPresent,
				}},
			},
		},
	}
	roleSet := &orchestrationapi.RoleSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "orchestration.aibrix.ai/v1alpha1",
			Kind:       "RoleSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: roleSetInPlaceNamespace,
		},
		Spec: orchestrationapi.RoleSetSpec{
			UpdateStrategy: orchestrationapi.ParallelRoleSetUpdateStrategyType,
			Roles:          []orchestrationapi.RoleSpec{role},
		},
	}

	objMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(roleSet)
	require.NoError(t, err)
	obj := &unstructured.Unstructured{Object: objMap}
	_, err = dynamicClient.Resource(roleSetGVR).Namespace(roleSetInPlaceNamespace).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err)
}

func createDrainRoleSet(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	name, image string,
	replicas, drainTimeoutSeconds int32,
) {
	t.Helper()

	role := orchestrationapi.RoleSpec{
		Name:     roleSetInPlaceRoleName,
		Replicas: ptr.To(replicas),
		Drain: &orchestrationapi.RoleDrainSpec{
			TimeoutSeconds: ptr.To(drainTimeoutSeconds),
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": name},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:            roleSetInPlaceContainerName,
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
				}},
			},
		},
	}
	roleSet := &orchestrationapi.RoleSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "orchestration.aibrix.ai/v1alpha1",
			Kind:       "RoleSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: roleSetInPlaceNamespace,
		},
		Spec: orchestrationapi.RoleSetSpec{
			UpdateStrategy: orchestrationapi.ParallelRoleSetUpdateStrategyType,
			Roles:          []orchestrationapi.RoleSpec{role},
		},
	}

	objMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(roleSet)
	require.NoError(t, err)
	obj := &unstructured.Unstructured{Object: objMap}
	_, err = dynamicClient.Resource(roleSetGVR).Namespace(roleSetInPlaceNamespace).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err)
}

func updateRoleSetContainerImage(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	name, image string,
) {
	t.Helper()
	updateRoleSet(ctx, t, dynamicClient, name, func(obj *unstructured.Unstructured) {
		setFirstRoleContainerField(t, obj, "image", image)
	})
}

func updateRoleSetCommand(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	name string,
	command []string,
) {
	t.Helper()
	updateRoleSet(ctx, t, dynamicClient, name, func(obj *unstructured.Unstructured) {
		commandValues := make([]interface{}, len(command))
		for i := range command {
			commandValues[i] = command[i]
		}
		setFirstRoleContainerField(t, obj, "command", commandValues)
	})
}

func updateRoleSetReplicas(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	name string,
	replicas int32,
) {
	t.Helper()
	updateRoleSet(ctx, t, dynamicClient, name, func(obj *unstructured.Unstructured) {
		roles, found, err := unstructured.NestedSlice(obj.Object, "spec", "roles")
		require.NoError(t, err)
		require.True(t, found)
		require.NotEmpty(t, roles)
		role := roles[0].(map[string]interface{})
		role["replicas"] = int64(replicas)
		require.NoError(t, unstructured.SetNestedSlice(obj.Object, roles, "spec", "roles"))
	})
}

func updateRoleSet(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	name string,
	mutate func(*unstructured.Unstructured),
) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		30*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			current, err := dynamicClient.Resource(roleSetGVR).
				Namespace(roleSetInPlaceNamespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			mutate(current)
			_, err = dynamicClient.Resource(roleSetGVR).
				Namespace(roleSetInPlaceNamespace).
				Update(ctx, current, metav1.UpdateOptions{})
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return err == nil, err
		},
	))
}

func setFirstRoleContainerField(t *testing.T, obj *unstructured.Unstructured, field string, value interface{}) {
	t.Helper()
	roles, found, err := unstructured.NestedSlice(obj.Object, "spec", "roles")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, roles)
	role := roles[0].(map[string]interface{})
	template := role["template"].(map[string]interface{})
	spec := template["spec"].(map[string]interface{})
	containers := spec["containers"].([]interface{})
	container := containers[0].(map[string]interface{})
	container[field] = value
	require.NoError(t, unstructured.SetNestedSlice(obj.Object, roles, "spec", "roles"))
}

func waitForSingleRoleSetPodReady(
	ctx context.Context,
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	roleSetName string,
) *corev1.Pod {
	t.Helper()
	return waitForSingleRoleSetPod(ctx, t, k8sClient, roleSetName, func(pod *corev1.Pod) bool {
		return isPodReady(pod)
	})
}

func waitForSingleRoleSetPod(
	ctx context.Context,
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	roleSetName string,
	predicate func(*corev1.Pod) bool,
) *corev1.Pod {
	t.Helper()
	var matched *corev1.Pod
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		120*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			pods, err := listRoleSetPods(ctx, k8sClient, roleSetName)
			if err != nil {
				return false, err
			}
			if len(pods) != 1 {
				t.Logf("waiting for one active pod for RoleSet %s, got %d", roleSetName, len(pods))
				return false, nil
			}
			if !predicate(&pods[0]) {
				t.Logf("waiting for pod %s to match predicate", pods[0].Name)
				return false, nil
			}
			matched = pods[0].DeepCopy()
			return true, nil
		},
	))
	return matched
}

func waitForRoleSetPodMatching(
	ctx context.Context,
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	roleSetName string,
	predicate func(*corev1.Pod) bool,
) *corev1.Pod {
	t.Helper()
	var matched *corev1.Pod
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		120*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			pods, err := listRoleSetPods(ctx, k8sClient, roleSetName)
			if err != nil {
				return false, err
			}
			for i := range pods {
				if predicate(&pods[i]) {
					matched = pods[i].DeepCopy()
					return true, nil
				}
			}
			t.Logf("waiting for matching active pod for RoleSet %s, got %d active pods", roleSetName, len(pods))
			return false, nil
		},
	))
	return matched
}

func restartRoleSetController(ctx context.Context, t *testing.T, k8sClient *kubernetes.Clientset) {
	t.Helper()
	namespace := envOrDefault(roleSetHistoricalNodeControllerNamespaceEnv, roleSetHistoricalNodeControllerNamespace)
	deploymentName := envOrDefault(roleSetHistoricalNodeControllerDeploymentEnv, roleSetHistoricalNodeControllerDeployment)
	deploymentClient := k8sClient.AppsV1().Deployments(namespace)
	podClient := k8sClient.CoreV1().Pods(namespace)

	deployment, err := deploymentClient.Get(ctx, deploymentName, metav1.GetOptions{})
	require.NoError(t, err)
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	require.NoError(t, err)
	require.NotEmpty(t, selector.String())

	pods, err := podClient.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	require.NoError(t, err)
	require.NotEmpty(
		t,
		pods.Items,
		"expected controller deployment %s/%s to have at least one pod",
		namespace,
		deploymentName,
	)
	for i := range pods.Items {
		require.NoError(t, podClient.Delete(ctx, pods.Items[i].Name, metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](0),
		}))
	}

	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		120*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			current, err := deploymentClient.Get(ctx, deploymentName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return controllerDeploymentAvailable(current), nil
		},
	))
}

func controllerDeploymentAvailable(deployment *appsv1.Deployment) bool {
	if deployment.Generation > deployment.Status.ObservedGeneration {
		return false
	}
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	return deployment.Status.ReadyReplicas >= desired && deployment.Status.AvailableReplicas >= desired
}

func waitForRoleSetHistoricalNodeAnnotation(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	roleSetName, nodeName string,
) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		60*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			obj, err := dynamicClient.Resource(roleSetGVR).
				Namespace(roleSetInPlaceNamespace).
				Get(ctx, roleSetName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			value := obj.GetAnnotations()[controllerconstants.RoleSetHistoricalNodeBindingsAnnotationKey]
			return strings.Contains(value, nodeName), nil
		},
	), "expected RoleSet %s historical-node annotation to contain node %s", roleSetName, nodeName)
}

func hasHistoricalNodeAffinity(pod *corev1.Pod, nodeName string) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return false
	}
	for _, term := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		if term.Weight != 100 {
			continue
		}
		for _, expression := range term.Preference.MatchExpressions {
			if expression.Key != corev1.LabelHostname || expression.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			for _, value := range expression.Values {
				if value == nodeName {
					return true
				}
			}
		}
	}
	return false
}

func waitForRoleSetPodsReady(
	ctx context.Context,
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	roleSetName string,
	expected int,
) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		120*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			pods, err := listRoleSetPods(ctx, k8sClient, roleSetName)
			if err != nil {
				return false, err
			}
			if len(pods) != expected {
				return false, nil
			}
			for i := range pods {
				if !isPodReady(&pods[i]) {
					return false, nil
				}
			}
			return true, nil
		},
	))
}

func listRoleSetPods(ctx context.Context, k8sClient *kubernetes.Clientset, roleSetName string) ([]corev1.Pod, error) {
	podList, err := k8sClient.CoreV1().Pods(roleSetInPlaceNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: roleSetPodSelector(roleSetName),
	})
	if err != nil {
		return nil, err
	}
	var active []corev1.Pod
	for _, pod := range podList.Items {
		if pod.DeletionTimestamp == nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			active = append(active, pod)
		}
	}
	return active, nil
}

func waitForSingleRoleSetPodSet(
	ctx context.Context,
	t *testing.T,
	dynamicClient dynamic.Interface,
	roleSetName string,
	predicate func(*unstructured.Unstructured) bool,
) *unstructured.Unstructured {
	t.Helper()
	var matched *unstructured.Unstructured
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		120*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			list, err := dynamicClient.Resource(podSetGVR).
				Namespace(roleSetInPlaceNamespace).
				List(ctx, metav1.ListOptions{
					LabelSelector: roleSetPodSelector(roleSetName),
				})
			if err != nil {
				return false, err
			}
			var active []unstructured.Unstructured
			for _, item := range list.Items {
				if item.GetDeletionTimestamp() == nil {
					active = append(active, item)
				}
			}
			if len(active) != 1 {
				t.Logf("waiting for one active PodSet for RoleSet %s, got %d", roleSetName, len(active))
				return false, nil
			}
			if !predicate(&active[0]) {
				return false, nil
			}
			matched = active[0].DeepCopy()
			return true, nil
		},
	))
	return matched
}

func roleSetPodSelector(roleSetName string) string {
	return fmt.Sprintf(
		"%s=%s,%s=%s",
		controllerconstants.RoleSetNameLabelKey,
		roleSetName,
		controllerconstants.RoleNameLabelKey,
		roleSetInPlaceRoleName,
	)
}

func podSetContainerImage(podSet *unstructured.Unstructured) string {
	containers, found, err := unstructured.NestedSlice(podSet.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return ""
	}
	container, ok := containers[0].(map[string]interface{})
	if !ok {
		return ""
	}
	image, _ := container["image"].(string)
	return image
}

func waitForRoleSetEvent(
	ctx context.Context,
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	roleSetName, reason string,
) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		60*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			events, err := k8sClient.CoreV1().Events(roleSetInPlaceNamespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			for _, event := range events.Items {
				if event.InvolvedObject.Name == roleSetName && event.Reason == reason {
					return true, nil
				}
			}
			return false, nil
		},
	), "expected RoleSet %s event reason %s", roleSetName, reason)
}

func cleanupRoleSetInPlaceE2EAfterTest(
	ctx context.Context,
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	dynamicClient dynamic.Interface,
	roleSetName string,
) {
	t.Helper()
	if t.Failed() && strings.ToLower(envOrDefault(roleSetInPlaceE2EKeepEnv, "false")) == e2eEnabledValue {
		t.Logf("preserving RoleSet in-place e2e resources for %s", roleSetName)
		return
	}
	cleanupRoleSetInPlaceE2E(ctx, t, k8sClient, dynamicClient, roleSetName)
}

func cleanupRoleSetInPlaceE2E(
	ctx context.Context,
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	dynamicClient dynamic.Interface,
	roleSetName string,
) {
	t.Helper()
	err := dynamicClient.Resource(roleSetGVR).
		Namespace(roleSetInPlaceNamespace).
		Delete(ctx, roleSetName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("failed to delete RoleSet %s: %v", roleSetName, err)
	}
	_ = k8sClient.CoreV1().Pods(roleSetInPlaceNamespace).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", controllerconstants.RoleSetNameLabelKey, roleSetName),
	})
	require.NoError(t, wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		60*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			_, err := dynamicClient.Resource(roleSetGVR).
				Namespace(roleSetInPlaceNamespace).
				Get(ctx, roleSetName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		},
	))
}

func containerImageID(t *testing.T, pod *corev1.Pod, containerName string) string {
	t.Helper()
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			return status.ImageID
		}
	}
	t.Fatalf("container %s status not found in pod %s", containerName, pod.Name)
	return ""
}

func isExpectedInPlacePendingReason(reason string) bool {
	switch reason {
	case controllerconstants.RoleInPlaceUpdatePendingReasonPodNotReady,
		controllerconstants.RoleInPlaceUpdatePendingReasonContainerStatusMissing,
		controllerconstants.RoleInPlaceUpdatePendingReasonContainerNotReady,
		controllerconstants.RoleInPlaceUpdatePendingReasonImageIDUnchanged:
		return true
	default:
		return false
	}
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
