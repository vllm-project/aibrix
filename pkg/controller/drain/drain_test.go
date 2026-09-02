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

package drain

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
)

func TestDeletePodsStartsDrain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)
	pod := drainTestPod("pod-1")
	roleSet := drainTestRoleSet()
	fakeClient := drainFakeClient(t, roleSet, pod)
	recorder := record.NewFakeRecorder(1)

	result, err := DeletePods(ctx, fakeClient, recorder, roleSet, []*corev1.Pod{pod}, &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To[int32](10)}, aibrixconst.PodDrainReasonScaleIn, now)

	require.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, "true", updated.Annotations[aibrixconst.PodDrainingAnnotationKey])
	assert.Equal(t, now.Format(time.RFC3339), updated.Annotations[aibrixconst.PodDrainStartTimeAnnotationKey])
	assert.Equal(t, aibrixconst.PodDrainReasonScaleIn, updated.Annotations[aibrixconst.PodDrainReasonAnnotationKey])
	assert.Equal(t, aibrixconst.PodDrainTargetActionDelete, updated.Annotations[aibrixconst.PodDrainTargetActionAnnotationKey])
	assert.Contains(t, <-recorder.Events, EventStarted)
}

func TestDeletePodsWaitsWithoutEvent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)
	pod := drainTestPod("pod-1")
	pod.Annotations = drainAnnotations(now.Add(-4*time.Second), aibrixconst.PodDrainReasonScaleIn)
	roleSet := drainTestRoleSet()
	fakeClient := drainFakeClient(t, roleSet, pod)
	recorder := record.NewFakeRecorder(1)

	result, err := DeletePods(ctx, fakeClient, recorder, roleSet, []*corev1.Pod{pod}, &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To[int32](10)}, aibrixconst.PodDrainReasonScaleIn, now)

	require.NoError(t, err)
	assert.False(t, result.Changed)
	assert.Equal(t, 6*time.Second, result.RequeueAfter)
	assert.Empty(t, recorder.Events)
}

func TestDeletePodsTreatsFutureStartTimeAsWaitingWithoutEvent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)
	pod := drainTestPod("pod-1")
	pod.Annotations = drainAnnotations(now.Add(time.Hour), aibrixconst.PodDrainReasonScaleIn)
	roleSet := drainTestRoleSet()
	fakeClient := drainFakeClient(t, roleSet, pod)
	recorder := record.NewFakeRecorder(1)

	result, err := DeletePods(ctx, fakeClient, recorder, roleSet, []*corev1.Pod{pod}, &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To[int32](10)}, aibrixconst.PodDrainReasonScaleIn, now)

	require.NoError(t, err)
	assert.False(t, result.Changed)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, now.Add(time.Hour).Format(time.RFC3339), updated.Annotations[aibrixconst.PodDrainStartTimeAnnotationKey])
	assert.Empty(t, recorder.Events)
}

func TestDeletePodsDeletesExpiredDrain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)
	pod := drainTestPod("pod-1")
	pod.Annotations = drainAnnotations(now.Add(-11*time.Second), aibrixconst.PodDrainReasonScaleIn)
	roleSet := drainTestRoleSet()
	fakeClient := drainFakeClient(t, roleSet, pod)
	recorder := record.NewFakeRecorder(1)

	result, err := DeletePods(ctx, fakeClient, recorder, roleSet, []*corev1.Pod{pod}, &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To[int32](10)}, aibrixconst.PodDrainReasonScaleIn, now)

	require.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Zero(t, result.RequeueAfter)
	assert.Contains(t, <-recorder.Events, EventCompleted)
}

func TestDeletePodsRepairsInvalidState(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)
	pod := drainTestPod("pod-1")
	pod.Annotations = drainAnnotations(now.Add(-1*time.Minute), aibrixconst.PodDrainReasonScaleIn)
	pod.Annotations[aibrixconst.PodDrainTargetActionAnnotationKey] = "sleep"
	roleSet := drainTestRoleSet()
	fakeClient := drainFakeClient(t, roleSet, pod)
	recorder := record.NewFakeRecorder(1)

	result, err := DeletePods(ctx, fakeClient, recorder, roleSet, []*corev1.Pod{pod}, &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To[int32](10)}, aibrixconst.PodDrainReasonRollout, now)

	require.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, aibrixconst.PodDrainReasonRollout, updated.Annotations[aibrixconst.PodDrainReasonAnnotationKey])
	assert.Equal(t, aibrixconst.PodDrainTargetActionDelete, updated.Annotations[aibrixconst.PodDrainTargetActionAnnotationKey])
	assert.Contains(t, <-recorder.Events, EventStateInvalid)
}

func TestDeletePodsIgnoresPatchNotFound(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)
	pod := drainTestPod("pod-1")
	roleSet := drainTestRoleSet()
	scheme := drainTestScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(roleSet, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cli client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, obj.GetName())
			},
		}).
		Build()
	recorder := record.NewFakeRecorder(1)

	result, err := DeletePods(ctx, fakeClient, recorder, roleSet, []*corev1.Pod{pod}, &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To[int32](10)}, aibrixconst.PodDrainReasonScaleIn, now)

	require.NoError(t, err)
	assert.False(t, result.Changed)
	assert.Zero(t, result.RequeueAfter)
	assert.Empty(t, recorder.Events)
}

func drainTestPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func drainTestRoleSet() *orchestrationv1alpha1.RoleSet {
	return &orchestrationv1alpha1.RoleSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "default"}}
}

func drainAnnotations(start time.Time, reason string) map[string]string {
	return map[string]string{
		aibrixconst.PodDrainingAnnotationKey:          "true",
		aibrixconst.PodDrainStartTimeAnnotationKey:    start.Format(time.RFC3339),
		aibrixconst.PodDrainReasonAnnotationKey:       reason,
		aibrixconst.PodDrainTargetActionAnnotationKey: aibrixconst.PodDrainTargetActionDelete,
	}
}

func drainFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(drainTestScheme(t)).WithObjects(objects...).Build()
}

func drainTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	return scheme
}
