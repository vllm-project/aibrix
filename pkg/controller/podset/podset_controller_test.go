/*
Copyright 2025 The Aibrix Team.

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

package podset

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicFake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientFake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

func TestCreatePodFromTemplate_EnvOrder(t *testing.T) {
	// Create a podSet with podGroupSize: 2
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-podset",
			Namespace: "test-namespace",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 2,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Name: "init-container",
							Env: []corev1.EnvVar{
								{Name: "INIT_USER_VAR", Value: "init-value"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Env: []corev1.EnvVar{
								{Name: "USER_VAR_Z", Value: "value-z"},
								{Name: "USER_VAR_A", Value: "value-a"},
							},
						},
					},
				},
			},
		},
	}
	scheme := runtime.NewScheme()
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().Build(),
		Scheme:        scheme,
		EventRecorder: &record.FakeRecorder{},
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	// Call createPodFromTemplate with podIndex 0
	pod, err := reconciler.createPodFromTemplate(podSet, 0)
	assert.NoError(t, err, "createPodFromTemplate should not return error")
	assert.NotNil(t, pod, "pod should not be nil")

	// Verify container count
	assert.Len(t, pod.Spec.Containers, 1, "pod should have one container")
	container := &pod.Spec.Containers[0]

	// Define built-in environment variables
	builtInEnvNames := []string{
		constants.PodSetNameEnvKey,
		constants.PodSetIndexEnvKey,
		constants.PodSetSizeEnvKey,
	}

	// Verify environment variables count
	expectedEnvCount := len(builtInEnvNames) + 2 // 3 built-in + 2 user-defined
	assert.Equal(t, expectedEnvCount, len(container.Env), "container should have correct number of env vars")

	// Verify built-in env vars are at the beginning
	for i := 0; i < len(builtInEnvNames); i++ {
		assert.Equal(t, builtInEnvNames[i], container.Env[i].Name, "Built-in env var should be at the beginning")
	}

	// Verify user-defined env vars maintain their order
	userEnvStartIndex := len(builtInEnvNames)
	expectedUserEnvOrder := []string{"USER_VAR_Z", "USER_VAR_A"}
	for i, expectedName := range expectedUserEnvOrder {
		actualIndex := userEnvStartIndex + i
		assert.Less(t, actualIndex, len(container.Env), "should have enough user-defined env vars")
		assert.Equal(t, expectedName, container.Env[actualIndex].Name, "User-defined env var should maintain original order")
	}

	// Verify InitContainers have built-in env vars at the beginning
	assert.Len(t, pod.Spec.InitContainers, 1, "pod should have one init container")
	initContainer := &pod.Spec.InitContainers[0]
	expectedInitEnvCount := len(builtInEnvNames) + 1 // 3 built-in + 1 user-defined
	assert.Equal(t, expectedInitEnvCount, len(initContainer.Env), "init container should have correct number of env vars")
	// Verify built-in env vars are at the beginning of init container
	for i := 0; i < len(builtInEnvNames); i++ {
		assert.Equal(t, builtInEnvNames[i], initContainer.Env[i].Name, "Built-in env var should be at the beginning of init container")
	}
	// Verify user-defined env var in init container
	assert.Equal(t, "INIT_USER_VAR", initContainer.Env[len(builtInEnvNames)].Name, "User-defined env var should be present in init container")
	assert.Equal(t, "init-value", initContainer.Env[len(builtInEnvNames)].Value, "User-defined env var value should be preserved in init container")
}

func TestCreatePodFromTemplate_EnvConflict(t *testing.T) {
	// Create a podSet with podGroupSize: 2 and conflicting env vars
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-podset",
			Namespace: "test-namespace",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey: "test-service",
			},
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 2,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Env: []corev1.EnvVar{
								{Name: constants.PodSetNameEnvKey, Value: "user-override"}, // Conflict with built-in
								{Name: "USER_VAR", Value: "value"},                         // Non-conflicting
								{Name: constants.PodSetSizeEnvKey, Value: "999"},           // Conflict with built-in
							},
						},
					},
				},
			},
		},
	}
	scheme := runtime.NewScheme()
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().Build(),
		Scheme:        scheme,
		EventRecorder: &record.FakeRecorder{},
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	// Call createPodFromTemplate with podIndex 0
	pod, err := reconciler.createPodFromTemplate(podSet, 0)
	assert.NoError(t, err, "createPodFromTemplate should not return error")
	assert.NotNil(t, pod, "pod should not be nil")

	// Verify container count
	assert.Len(t, pod.Spec.Containers, 1, "pod should have one container")
	container := &pod.Spec.Containers[0]

	// Define built-in environment variables
	builtInEnvNames := []string{
		constants.PodSetNameEnvKey,
		constants.PodSetIndexEnvKey,
		constants.PodSetSizeEnvKey,
	}

	// Verify environment variables count
	expectedEnvCount := len(builtInEnvNames) + 1 // 3 built-in + 1 non-conflicting user-defined
	assert.Equal(t, expectedEnvCount, len(container.Env), "container should have correct number of env vars")

	// Verify built-in env vars are at the beginning and not overridden
	for i := 0; i < len(builtInEnvNames); i++ {
		assert.Equal(t, builtInEnvNames[i], container.Env[i].Name, "Built-in env var should be at the beginning")
	}

	// Only non-conflicting user-defined env var should be present
	env := container.Env[len(builtInEnvNames)]
	assert.NotNil(t, env, "should have non-conflicting user-defined env var")
	assert.Equal(t, "USER_VAR", env.Name, "Only non-conflicting user-defined env var should be present")
	assert.Equal(t, "value", env.Value, "User-defined env var value should be preserved")
}

func TestCreatePodFromTemplate_VolcanoSchedulingMetadata(t *testing.T) {
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-podset",
			Namespace: "test-namespace",
			Labels: map[string]string{
				constants.StormServiceNameLabelKey:         "test-service",
				constants.VolcanoPodGroupNameAnnotationKey: "test-podset",
				constants.VolcanoTaskSpecKey:               "prefill",
			},
			Annotations: map[string]string{
				constants.VolcanoPodGroupNameAnnotationKey: "test-podset",
				constants.VolcanoTaskSpecKey:               "prefill",
			},
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 2,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test-container"}},
				},
			},
		},
	}
	scheme := runtime.NewScheme()
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().Build(),
		Scheme:        scheme,
		EventRecorder: &record.FakeRecorder{},
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	pod, err := reconciler.createPodFromTemplate(podSet, 0)

	assert.NoError(t, err)
	assert.Equal(t, "volcano", pod.Spec.SchedulerName)
	assert.Equal(t, "test-podset", pod.Labels[constants.VolcanoPodGroupNameAnnotationKey])
	assert.Equal(t, "test-podset", pod.Annotations[constants.VolcanoPodGroupNameAnnotationKey])
	assert.Equal(t, "prefill", pod.Labels[constants.VolcanoTaskSpecKey])
	assert.Equal(t, "prefill", pod.Annotations[constants.VolcanoTaskSpecKey])
}

func TestHandleScaleDownStartsDrain(t *testing.T) {
	ctx := context.Background()
	timeout := int32(15)
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-podset", Namespace: "test-namespace"},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 1,
			Drain:        &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout},
		},
	}
	activePods := []corev1.Pod{
		podSetScaleDownPod("test-podset-0", 0),
		podSetScaleDownPod("test-podset-1", 1),
	}
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().WithScheme(scheme).WithObjects(&activePods[0], &activePods[1]).Build(),
		Scheme:        scheme,
		EventRecorder: record.NewFakeRecorder(2),
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	result, err := reconciler.handleScaleDown(ctx, podSet, activePods, 2, 1)

	assert.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Equal(t, 15*time.Second, result.RequeueAfter)
	updated := &corev1.Pod{}
	assert.NoError(t, reconciler.Get(ctx, client.ObjectKey{Namespace: "test-namespace", Name: "test-podset-1"}, updated))
	assert.Equal(t, "true", updated.Annotations[aibrixconst.PodDrainingAnnotationKey])
	assert.Equal(t, aibrixconst.PodDrainReasonScaleIn, updated.Annotations[aibrixconst.PodDrainReasonAnnotationKey])
	assert.Equal(t, aibrixconst.PodDrainTargetActionDelete, updated.Annotations[aibrixconst.PodDrainTargetActionAnnotationKey])
}

func TestReconcilePodsScaleDownCancelClearsDrain(t *testing.T) {
	ctx := context.Background()
	timeout := int32(15)
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-podset", Namespace: "test-namespace"},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 1,
			Drain:        &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout},
		},
	}
	pod0 := podSetScaleDownPod("test-podset-0", 0)
	pod1 := podSetScaleDownPod("test-podset-1", 1)
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().WithScheme(scheme).WithObjects(&pod0, &pod1).Build(),
		Scheme:        scheme,
		EventRecorder: record.NewFakeRecorder(10),
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	result, err := reconciler.reconcilePods(ctx, podSet)
	assert.NoError(t, err)
	require.True(t, result.Changed)
	require.Equal(t, 15*time.Second, result.RequeueAfter)

	podSet.Spec.PodGroupSize = 2
	result, err = reconciler.reconcilePods(ctx, podSet)

	assert.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Zero(t, result.RequeueAfter)
	pods := &corev1.PodList{}
	assert.NoError(t, reconciler.List(ctx, pods))
	assert.Len(t, pods.Items, 2)
	for i := range pods.Items {
		assert.NotEqual(t, "true", pods.Items[i].Annotations[aibrixconst.PodDrainingAnnotationKey])
		assert.Empty(t, pods.Items[i].Annotations[aibrixconst.PodDrainStartTimeAnnotationKey])
		assert.Empty(t, pods.Items[i].Annotations[aibrixconst.PodDrainReasonAnnotationKey])
		assert.Empty(t, pods.Items[i].Annotations[aibrixconst.PodDrainTargetActionAnnotationKey])
	}
}

func TestHandleScaleDownCancelsStaleDrainOutsideDeleteSet(t *testing.T) {
	ctx := context.Background()
	timeout := int32(15)
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-podset", Namespace: "test-namespace"},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 1,
			Drain:        &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout},
		},
	}
	pod0 := podSetScaleDownPod("test-podset-0", 0)
	pod0.Annotations = scaleInDrainAnnotations(time.Now().Add(-5 * time.Second))
	pod1 := podSetScaleDownPod("test-podset-1", 1)
	pod2 := podSetScaleDownPod("test-podset-2", 2)
	activePods := []corev1.Pod{pod0, pod1, pod2}
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().WithScheme(scheme).WithObjects(&pod0, &pod1, &pod2).Build(),
		Scheme:        scheme,
		EventRecorder: record.NewFakeRecorder(10),
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	result, err := reconciler.handleScaleDown(ctx, podSet, activePods, 3, 1)

	assert.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Equal(t, 15*time.Second, result.RequeueAfter)
	updated0 := &corev1.Pod{}
	assert.NoError(t, reconciler.Get(ctx, client.ObjectKey{Namespace: "test-namespace", Name: "test-podset-0"}, updated0))
	assert.Empty(t, updated0.Annotations[aibrixconst.PodDrainingAnnotationKey])
	updated1 := &corev1.Pod{}
	assert.NoError(t, reconciler.Get(ctx, client.ObjectKey{Namespace: "test-namespace", Name: "test-podset-1"}, updated1))
	assert.Equal(t, "true", updated1.Annotations[aibrixconst.PodDrainingAnnotationKey])
	updated2 := &corev1.Pod{}
	assert.NoError(t, reconciler.Get(ctx, client.ObjectKey{Namespace: "test-namespace", Name: "test-podset-2"}, updated2))
	assert.Equal(t, "true", updated2.Annotations[aibrixconst.PodDrainingAnnotationKey])
}

func TestHandleScaleDownKeepsDeleteSetDrainAndDeletesWhenExpired(t *testing.T) {
	ctx := context.Background()
	timeout := int32(15)
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-podset", Namespace: "test-namespace"},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 1,
			Drain:        &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout},
		},
	}
	pod0 := podSetScaleDownPod("test-podset-0", 0)
	pod1 := podSetScaleDownPod("test-podset-1", 1)
	pod1.Annotations = scaleInDrainAnnotations(time.Now().Add(-30 * time.Second))
	activePods := []corev1.Pod{pod0, pod1}
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().WithScheme(scheme).WithObjects(&pod0, &pod1).Build(),
		Scheme:        scheme,
		EventRecorder: record.NewFakeRecorder(10),
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	result, err := reconciler.handleScaleDown(ctx, podSet, activePods, 2, 1)

	assert.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Zero(t, result.RequeueAfter)
	deleted := &corev1.Pod{}
	err = reconciler.Get(ctx, client.ObjectKey{Namespace: "test-namespace", Name: "test-podset-1"}, deleted)
	assert.True(t, apierrors.IsNotFound(err))
	kept := &corev1.Pod{}
	assert.NoError(t, reconciler.Get(ctx, client.ObjectKey{Namespace: "test-namespace", Name: "test-podset-0"}, kept))
}

func TestReconcilePodsDoesNotCancelRolloutDrain(t *testing.T) {
	ctx := context.Background()
	timeout := int32(15)
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-podset", Namespace: "test-namespace"},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 1,
			Drain:        &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout},
		},
	}
	pod0 := podSetScaleDownPod("test-podset-0", 0)
	pod0.Annotations = scaleInDrainAnnotations(time.Now().Add(-5 * time.Second))
	pod0.Annotations[aibrixconst.PodDrainReasonAnnotationKey] = aibrixconst.PodDrainReasonRollout
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().WithScheme(scheme).WithObjects(&pod0).Build(),
		Scheme:        scheme,
		EventRecorder: record.NewFakeRecorder(10),
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	result, err := reconciler.reconcilePods(ctx, podSet)

	assert.NoError(t, err)
	assert.False(t, result.Changed)
	assert.Zero(t, result.RequeueAfter)
	updated := &corev1.Pod{}
	assert.NoError(t, reconciler.Get(ctx, client.ObjectKey{Namespace: "test-namespace", Name: "test-podset-0"}, updated))
	assert.Equal(t, "true", updated.Annotations[aibrixconst.PodDrainingAnnotationKey])
	assert.Equal(t, aibrixconst.PodDrainReasonRollout, updated.Annotations[aibrixconst.PodDrainReasonAnnotationKey])
}

func TestHandleReplaceUnhealthyDeletesImmediatelyWithoutDrain(t *testing.T) {
	ctx := context.Background()
	timeout := int32(15)
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-podset", Namespace: "test-namespace"},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize:   2,
			RecoveryPolicy: orchestrationv1alpha1.ReplaceUnhealthy,
			Drain:          &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: &timeout},
		},
	}
	unhealthyPod := podSetScaleDownPod("test-podset-0", 0)
	unhealthyPod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runtime", RestartCount: 1}}
	healthyPod := podSetScaleDownPod("test-podset-1", 1)
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	reconciler := &PodSetReconciler{
		Client:        clientFake.NewClientBuilder().WithScheme(scheme).WithObjects(&unhealthyPod, &healthyPod).Build(),
		Scheme:        scheme,
		EventRecorder: record.NewFakeRecorder(2),
		DynamicClient: dynamicFake.NewSimpleDynamicClient(scheme),
	}

	result, err := reconciler.handleReplaceUnhealthy(ctx, podSet, []corev1.Pod{unhealthyPod, healthyPod}, 2, 2)

	assert.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Zero(t, result.RequeueAfter)
	updated := &corev1.Pod{}
	err = reconciler.Get(ctx, client.ObjectKey{Namespace: "test-namespace", Name: "test-podset-0"}, updated)
	assert.True(t, apierrors.IsNotFound(err))
}

func podSetScaleDownPod(name string, index int) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-namespace",
			Labels: map[string]string{
				constants.PodSetNameLabelKey:    "test-podset",
				constants.PodGroupIndexLabelKey: strconv.Itoa(index),
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func scaleInDrainAnnotations(start time.Time) map[string]string {
	return map[string]string{
		aibrixconst.PodDrainingAnnotationKey:          "true",
		aibrixconst.PodDrainStartTimeAnnotationKey:    start.UTC().Format(time.RFC3339),
		aibrixconst.PodDrainReasonAnnotationKey:       aibrixconst.PodDrainReasonScaleIn,
		aibrixconst.PodDrainTargetActionAnnotationKey: aibrixconst.PodDrainTargetActionDelete,
	}
}
