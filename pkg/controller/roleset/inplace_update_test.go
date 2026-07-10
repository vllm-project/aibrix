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

package roleset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

func TestRoleUpdateStrategyTypeOrDefault(t *testing.T) {
	tests := []struct {
		name string
		role *orchestrationv1alpha1.RoleSpec
		want orchestrationv1alpha1.RoleUpdateStrategyType
	}{
		{
			name: "empty defaults to recreate",
			role: &orchestrationv1alpha1.RoleSpec{},
			want: orchestrationv1alpha1.RecreateRoleUpdateStrategyType,
		},
		{
			name: "explicit in-place if possible is preserved",
			role: &orchestrationv1alpha1.RoleSpec{
				UpdateStrategy: orchestrationv1alpha1.RoleUpdateStrategy{
					Type: orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType,
				},
			},
			want: orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, roleUpdateStrategyTypeOrDefault(tt.role))
		})
	}
}

func TestInPlaceUpdateEligibility(t *testing.T) {
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	oldRole := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v1")
	newRole := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")

	pod, err := buildRenderedPod(roleSet, oldRole, nil)
	require.NoError(t, err)

	eligible, reason, err := canInPlaceUpdatePod(roleSet, newRole, pod, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.True(t, eligible)
	assert.Empty(t, reason)

	livePod := pod.DeepCopy()
	livePod.Spec.NodeName = "node-1"
	livePod.Spec.SchedulerName = corev1.DefaultSchedulerName
	livePod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	livePod.Spec.DNSPolicy = corev1.DNSClusterFirst
	livePod.Spec.TerminationGracePeriodSeconds = ptr.To[int64](30)
	livePod.Spec.EnableServiceLinks = ptr.To(true)
	livePod.Spec.ServiceAccountName = "default"
	livePod.Spec.DeprecatedServiceAccount = "default"
	livePod.Spec.SecurityContext = &corev1.PodSecurityContext{}
	livePod.Spec.Priority = ptr.To[int32](0)
	livePod.Spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
	livePod.Spec.Tolerations = []corev1.Toleration{
		{
			Key:               "node.kubernetes.io/not-ready",
			Operator:          corev1.TolerationOpExists,
			Effect:            corev1.TaintEffectNoExecute,
			TolerationSeconds: ptr.To[int64](300),
		},
		{
			Key:               "node.kubernetes.io/unreachable",
			Operator:          corev1.TolerationOpExists,
			Effect:            corev1.TaintEffectNoExecute,
			TolerationSeconds: ptr.To[int64](300),
		},
	}
	livePod.Spec.Volumes = []corev1.Volume{{
		Name: "kube-api-access-test",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"},
				}},
			},
		},
	}}
	livePod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name:      "kube-api-access-test",
		MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
		ReadOnly:  true,
	}}
	livePod.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	livePod.Spec.Containers[0].TerminationMessagePath = corev1.TerminationMessagePathDefault
	livePod.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	eligible, reason, err = canInPlaceUpdatePod(roleSet, newRole, livePod, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.True(t, eligible)
	assert.Empty(t, reason)

	podWithTargetAnnotation := pod.DeepCopy()
	podWithTargetAnnotation.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] = newHash
	eligible, reason, err = canInPlaceUpdatePod(roleSet, newRole, podWithTargetAnnotation, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.True(t, eligible)
	assert.Empty(t, reason)

	changedCommand := oldRole.DeepCopy()
	changedCommand.Template.Spec.Containers[0].Command = []string{"python", "-m", "vllm.entrypoints.openai.api_server"}
	eligible, reason, err = canInPlaceUpdatePod(roleSet, changedCommand, pod, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.False(t, eligible)
	assert.Contains(t, reason, "non-image")

	changedAnnotation := newRole.DeepCopy()
	changedAnnotation.Template.Annotations = map[string]string{"rollout.example.com/config": "v2"}
	eligible, reason, err = canInPlaceUpdatePod(roleSet, changedAnnotation, pod, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.False(t, eligible)
	assert.Contains(t, reason, "non-image")

	changedFinalizers := newRole.DeepCopy()
	changedFinalizers.Template.Finalizers = []string{"example.com/finalizer"}
	eligible, reason, err = canInPlaceUpdatePod(roleSet, changedFinalizers, pod, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.False(t, eligible)
	assert.Contains(t, reason, "non-image")

	podWithEphemeral := pod.DeepCopy()
	podWithEphemeral.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:  "debugger",
			Image: "debug:v1",
		},
	}}
	changedEphemeral := oldRole.DeepCopy()
	changedEphemeral.Template.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:  "debugger",
			Image: "debug:v2",
		},
	}}
	eligible, reason, err = canInPlaceUpdatePod(roleSet, changedEphemeral, podWithEphemeral, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.False(t, eligible)
	assert.Contains(t, reason, "non-image")

	podWithInitHashEnv := pod.DeepCopy()
	podWithInitHashEnv.Spec.InitContainers = []corev1.Container{{
		Name:  "init-cache",
		Image: "init:v1",
		Env: []corev1.EnvVar{{
			Name:  constants.RoleTemplateHashEnvKey,
			Value: oldHash,
		}},
	}}
	changedInitEnv := oldRole.DeepCopy()
	changedInitEnv.Template.Spec.InitContainers = []corev1.Container{{
		Name:  "init-cache",
		Image: "init:v1",
		Env: []corev1.EnvVar{{
			Name:  constants.RoleTemplateHashEnvKey,
			Value: newHash,
		}},
	}}
	eligible, reason, err = canInPlaceUpdatePod(roleSet, changedInitEnv, podWithInitHashEnv, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.False(t, eligible)
	assert.Contains(t, reason, "image")

	podWithInitImage := pod.DeepCopy()
	podWithInitImage.Spec.InitContainers = []corev1.Container{{
		Name:  "init-cache",
		Image: "init:v1",
	}}
	changedInitImage := newRole.DeepCopy()
	changedInitImage.Template.Spec.InitContainers = []corev1.Container{{
		Name:  "init-cache",
		Image: "init:v2",
	}}
	eligible, reason, err = canInPlaceUpdatePod(roleSet, changedInitImage, podWithInitImage, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.False(t, eligible)
	assert.Contains(t, reason, "init container image")

	stormRoleSet := newTestRoleSet("test-roleset", "test-ns")
	stormRoleSet.Annotations = map[string]string{
		constants.RoleRevisionAnnotationPrefix + ".worker":     "2",
		constants.RoleRevisionNameAnnotationPrefix + ".worker": "stormservice-rev-v2",
	}
	stormPod := pod.DeepCopy()
	stormPod.Labels[constants.RoleRevisionLabelKey] = "1"
	stormPod.Labels[constants.RoleRevisionNameLabelKey] = "stormservice-rev-v1"
	eligible, reason, err = canInPlaceUpdatePod(stormRoleSet, newRole, stormPod, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.True(t, eligible)
	assert.Empty(t, reason)

	roleWithUserDefaults := newRole.DeepCopy()
	roleWithUserDefaults.Template.Spec.Tolerations = []corev1.Toleration{{
		Key:      "dedicated",
		Operator: corev1.TolerationOpEqual,
		Value:    "inference",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	roleWithUserDefaults.Template.Spec.Volumes = []corev1.Volume{{
		Name: "model-cache",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}}
	roleWithUserDefaults.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name:      "model-cache",
		MountPath: "/models",
	}}
	podWithUserDefaults, err := buildRenderedPod(roleSet, roleWithUserDefaults, nil)
	require.NoError(t, err)
	podWithUserDefaults.Spec.Containers[0].Image = "vllm:v1"
	podWithUserDefaults.Spec.Tolerations = append(podWithUserDefaults.Spec.Tolerations, corev1.Toleration{
		Key:               "node.kubernetes.io/not-ready",
		Operator:          corev1.TolerationOpExists,
		Effect:            corev1.TaintEffectNoExecute,
		TolerationSeconds: ptr.To[int64](300),
	})
	podWithUserDefaults.Spec.Volumes = append(podWithUserDefaults.Spec.Volumes, corev1.Volume{
		Name: "kube-api-access-test",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"},
				}},
			},
		},
	})
	podWithUserDefaults.Spec.Containers[0].VolumeMounts = append(
		podWithUserDefaults.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{
			Name:      "kube-api-access-test",
			MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
			ReadOnly:  true,
		},
	)
	eligible, reason, err = canInPlaceUpdatePod(roleSet, roleWithUserDefaults, podWithUserDefaults, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.True(t, eligible)
	assert.Empty(t, reason)

	podWithUserTokenVolume := pod.DeepCopy()
	podWithUserTokenVolume.Spec.Volumes = []corev1.Volume{{
		Name: "custom-token",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"},
				}},
			},
		},
	}}
	eligible, reason, err = canInPlaceUpdatePod(roleSet, newRole, podWithUserTokenVolume, fakeComputeHashFunc)
	require.NoError(t, err)
	assert.False(t, eligible)
	assert.Contains(t, reason, "non-image")
}

func TestPatchPodImagesForInPlaceUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")
	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "vllm:v1"}}
	pod.Spec.InitContainers = []corev1.Container{{Name: "init-cache", Image: "init:v1"}}
	role.Template.Spec.InitContainers = []corev1.Container{{Name: "init-cache", Image: "init:v2"}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	changed, err := patchPodImagesForInPlaceUpdate(ctx, fakeClient, pod, role, newHash)
	require.NoError(t, err)
	assert.True(t, changed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, "vllm:v2", updated.Spec.Containers[0].Image)
	assert.Equal(t, "init:v1", updated.Spec.InitContainers[0].Image)
	assert.Equal(t, oldHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.Equal(t, newHash, updated.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey])
}

func TestPatchPodImagesForInPlaceUpdateRecordsStateAndMarksReadinessGateFalse(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")
	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{
		ConditionType: corev1.PodConditionType(constants.RoleInPlaceUpdateReadyCondition),
	}}
	pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "vllm:v1"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "vllm-openai",
		Image:   "vllm:v1",
		ImageID: "docker-pullable://vllm@sha256:old",
		Ready:   true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	changed, err := patchPodImagesForInPlaceUpdate(ctx, fakeClient, pod, role, newHash)
	require.NoError(t, err)
	assert.True(t, changed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Contains(t, updated.Annotations, constants.RoleInPlaceUpdateStateAnnotationKey)
	assert.Contains(t, updated.Annotations[constants.RoleInPlaceUpdateStateAnnotationKey], "docker-pullable://vllm@sha256:old")

	condition := findPodCondition(updated.Status.Conditions, corev1.PodConditionType(constants.RoleInPlaceUpdateReadyCondition))
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionFalse, condition.Status)
}

func TestHasInPlaceUpdateInProgress(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Labels[constants.RoleSetNameLabelKey] = "test-roleset"
	pod.Annotations = map[string]string{constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash}
	otherPod := newTestPodWithHash("pod-2", "test-ns", true, false, oldHash)
	otherPod.Labels[constants.RoleSetNameLabelKey] = "other-roleset"
	otherPod.Annotations = map[string]string{constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod, otherPod).
		Build()

	inProgress, err := hasInPlaceUpdateInProgress(ctx, fakeClient, "test-ns", "test-roleset")
	require.NoError(t, err)
	assert.True(t, inProgress)

	inProgress, err = hasInPlaceUpdateInProgress(ctx, fakeClient, "test-ns", "missing-roleset")
	require.NoError(t, err)
	assert.False(t, inProgress)
}

func TestMarkInPlaceUpdateComplete(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")

	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Annotations = map[string]string{constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash}
	pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "vllm:v2"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "vllm-openai",
		Image:   "vllm:v2",
		ImageID: "docker-pullable://vllm@sha256:2",
		Ready:   true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	completed, err := markInPlaceUpdateComplete(ctx, fakeClient, roleSet, role, pod, newHash)
	require.NoError(t, err)
	assert.True(t, completed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, newHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.NotContains(t, updated.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)
}

func TestMarkInPlaceUpdateCompleteWaitsForChangedImageID(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")

	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Annotations = map[string]string{
		constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash,
		constants.RoleInPlaceUpdateStateAnnotationKey:      `{"lastContainerStatuses":{"vllm-openai":{"imageID":"docker-pullable://vllm@sha256:old"}}}`,
	}
	pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "vllm:v2"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "vllm-openai",
		Image:   "vllm:v2",
		ImageID: "docker-pullable://vllm@sha256:old",
		Ready:   true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	completed, err := markInPlaceUpdateComplete(ctx, fakeClient, roleSet, role, pod, newHash)
	require.NoError(t, err)
	assert.False(t, completed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, oldHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.Equal(t, newHash, updated.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey])
	assert.Equal(t, constants.RoleInPlaceUpdatePendingReasonImageIDUnchanged, updated.Annotations[constants.RoleInPlaceUpdatePendingReasonAnnotationKey])
}

func TestMarkInPlaceUpdateCompleteRecordsPendingReason(t *testing.T) {
	tests := []struct {
		name              string
		ready             bool
		status            corev1.ContainerStatus
		wantPendingReason string
	}{
		{
			name:  "pod not ready",
			ready: false,
			status: corev1.ContainerStatus{
				Name:    "vllm-openai",
				Image:   "vllm:v2",
				ImageID: "docker-pullable://vllm@sha256:new",
				Ready:   true,
			},
			wantPendingReason: constants.RoleInPlaceUpdatePendingReasonPodNotReady,
		},
		{
			name:  "runtime image mismatch",
			ready: true,
			status: corev1.ContainerStatus{
				Name:    "vllm-openai",
				Image:   "vllm:v1",
				ImageID: "docker-pullable://vllm@sha256:new",
				Ready:   true,
			},
			wantPendingReason: constants.RoleInPlaceUpdatePendingReasonRuntimeImageMismatch,
		},
		{
			name:  "container status missing",
			ready: true,
			status: corev1.ContainerStatus{
				Name:    "sidecar",
				Image:   "sidecar:v1",
				ImageID: "docker-pullable://sidecar@sha256:1",
				Ready:   true,
			},
			wantPendingReason: constants.RoleInPlaceUpdatePendingReasonContainerStatusMissing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			roleSet := newTestRoleSet("test-roleset", "test-ns")
			role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")

			pod := newTestPodWithHash(testPodOne, "test-ns", tt.ready, false, oldHash)
			pod.Annotations = map[string]string{constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash}
			pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "vllm:v2"}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{tt.status}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(pod).
				Build()

			completed, err := markInPlaceUpdateComplete(ctx, fakeClient, roleSet, role, pod, newHash)
			require.NoError(t, err)
			assert.False(t, completed)

			updated := &corev1.Pod{}
			require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
			assert.Equal(t, tt.wantPendingReason, updated.Annotations[constants.RoleInPlaceUpdatePendingReasonAnnotationKey])
		})
	}
}

func TestMarkInPlaceUpdateCompleteAcceptsKubeletNormalizedRuntimeImage(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newRoleSpecWithImage("worker", "vllm-openai", "nginx:1.27-alpine")

	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Annotations = map[string]string{constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash}
	pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "nginx:1.27-alpine"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "vllm-openai",
		Image:   "docker.io/library/nginx:1.27-alpine",
		ImageID: "docker-pullable://docker.io/library/nginx@sha256:2",
		Ready:   true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	completed, err := markInPlaceUpdateComplete(ctx, fakeClient, roleSet, role, pod, newHash)
	require.NoError(t, err)
	assert.True(t, completed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, newHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.NotContains(t, updated.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)
	assert.NotContains(t, updated.Annotations, constants.RoleInPlaceUpdatePendingReasonAnnotationKey)
}

func findPodCondition(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) *corev1.PodCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func TestMarkInPlaceUpdateCompleteWaitsForRuntimeImage(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")

	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Annotations = map[string]string{constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash}
	pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "vllm:v2"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "vllm-openai",
		Image: "vllm:v1",
		Ready: true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	completed, err := markInPlaceUpdateComplete(ctx, fakeClient, roleSet, role, pod, newHash)
	require.NoError(t, err)
	assert.False(t, completed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, oldHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.Equal(t, newHash, updated.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey])
}

func TestMarkInPlaceUpdateCompleteRequiresTargetAnnotation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")

	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "vllm:v1"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "vllm-openai",
		Image: "vllm:v1",
		Ready: true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	completed, err := markInPlaceUpdateComplete(ctx, fakeClient, roleSet, role, pod, newHash)
	require.NoError(t, err)
	assert.False(t, completed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, oldHash, updated.Labels[constants.RoleTemplateHashLabelKey])
}

func TestMarkInPlaceUpdateCompleteClearsStaleTargetAnnotation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	roleSet := newTestRoleSet("test-roleset", "test-ns")
	role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")

	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, newHash)
	pod.Annotations = map[string]string{constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	completed, err := markInPlaceUpdateComplete(ctx, fakeClient, roleSet, role, pod, newHash)
	require.NoError(t, err)
	assert.True(t, completed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, newHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.NotContains(t, updated.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)
}

func TestMarkInPlaceUpdateCompletePromotesStormServiceRoleRevisionLabels(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	roleSet := newTestRoleSet("test-roleset", "test-ns")
	roleSet.Annotations = map[string]string{
		constants.RoleRevisionAnnotationPrefix + ".worker":     "2",
		constants.RoleRevisionNameAnnotationPrefix + ".worker": "stormservice-rev-v2",
	}
	role := newRoleSpecWithImage("worker", "vllm-openai", "vllm:v2")
	pod := newTestPodWithHash(testPodOne, "test-ns", true, false, oldHash)
	pod.Labels[constants.RoleRevisionLabelKey] = "1"
	pod.Labels[constants.RoleRevisionNameLabelKey] = "stormservice-rev-v1"
	pod.Annotations = map[string]string{constants.RoleInPlaceUpdateTargetHashAnnotationKey: newHash}
	pod.Spec.Containers = []corev1.Container{{Name: "vllm-openai", Image: "vllm:v2"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "vllm-openai",
		Image: "vllm:v2",
		Ready: true,
	}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		Build()

	completed, err := markInPlaceUpdateComplete(ctx, fakeClient, roleSet, role, pod, newHash)
	require.NoError(t, err)
	assert.True(t, completed)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), updated))
	assert.Equal(t, newHash, updated.Labels[constants.RoleTemplateHashLabelKey])
	assert.Equal(t, "2", updated.Labels[constants.RoleRevisionLabelKey])
	assert.Equal(t, "stormservice-rev-v2", updated.Labels[constants.RoleRevisionNameLabelKey])
	assert.NotContains(t, updated.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)
}

func newRoleSpecWithImage(roleName, containerName, image string) *orchestrationv1alpha1.RoleSpec {
	return &orchestrationv1alpha1.RoleSpec{
		Name: roleName,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": roleName},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  containerName,
					Image: image,
				}},
			},
		},
	}
}
