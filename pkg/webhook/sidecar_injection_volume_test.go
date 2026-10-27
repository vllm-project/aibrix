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

package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// Sidecar injection must share the adapter volume without replacing storage that
// the workload declares explicitly, for example a PVC that keeps downloaded LoRA
// adapters across restarts.
func TestSidecarInjectionPreservesAdapterVolume(t *testing.T) {
	pvcSource := corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "adapter-cache"},
	}
	emptyDirSource := corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: ptr.To(resource.MustParse("64Mi")),
		},
	}

	tests := map[string]struct {
		volumes []corev1.Volume
		mounts  []corev1.VolumeMount
		want    corev1.VolumeSource
	}{
		"existing PVC volume is preserved": {
			volumes: []corev1.Volume{{Name: DefaultAdapterVolumeName, VolumeSource: pvcSource}},
			mounts:  []corev1.VolumeMount{{Name: DefaultAdapterVolumeName, MountPath: DefaultAdapterMountPath}},
			want:    pvcSource,
		},
		"existing emptyDir volume keeps its settings": {
			volumes: []corev1.Volume{{Name: DefaultAdapterVolumeName, VolumeSource: emptyDirSource}},
			mounts:  []corev1.VolumeMount{{Name: DefaultAdapterVolumeName, MountPath: DefaultAdapterMountPath}},
			want:    emptyDirSource,
		},
		"absent volume gets the default emptyDir": {
			want: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Run("Deployment", func(t *testing.T) {
				deployment := newTestDeployment(tc.volumes, tc.mounts)
				require.NoError(t, (&DeploymentCustomDefaulter{}).Default(context.Background(), deployment))

				assertAdapterVolume(t, deployment.Spec.Template.Spec.Volumes, tc.want)
				assertAdapterMounts(t, deployment.Spec.Template.Spec.Containers, 2)
			})

			t.Run("StormService", func(t *testing.T) {
				stormService := newTestStormService(tc.volumes, tc.mounts)
				require.NoError(t, (&StormServiceCustomDefaulter{}).Default(context.Background(), stormService))

				role := stormService.Spec.Template.Spec.Roles[0]
				assertAdapterVolume(t, role.Template.Spec.Volumes, tc.want)
				assertAdapterMounts(t, role.Template.Spec.Containers, 2)
			})
		})
	}
}

func newTestDeployment(volumes []corev1.Volume, mounts []corev1.VolumeMount) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "engine",
			Namespace:   "default",
			Annotations: map[string]string{SidecarInjectionAnnotation: "true"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         "engine",
						Image:        "vllm/vllm-openai:latest",
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

func newTestStormService(volumes []corev1.Volume, mounts []corev1.VolumeMount) *orchestrationv1alpha1.StormService {
	return &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "engine",
			Namespace:   "default",
			Annotations: map[string]string{SidecarInjectionAnnotation: "true"},
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To[int32](1),
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{{
						Name:     "inference",
						Replicas: ptr.To[int32](1),
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:         "engine",
									Image:        "vllm/vllm-openai:latest",
									VolumeMounts: mounts,
								}},
								Volumes: volumes,
							},
						},
					}},
				},
			},
		},
	}
}

func assertAdapterVolume(t *testing.T, volumes []corev1.Volume, want corev1.VolumeSource) {
	t.Helper()

	var found int
	for _, volume := range volumes {
		if volume.Name != DefaultAdapterVolumeName {
			continue
		}
		found++
		assert.Equal(t, want, volume.VolumeSource)
	}
	assert.Equal(t, 1, found, "expected exactly one %s volume", DefaultAdapterVolumeName)
}

// assertAdapterMounts checks that the engine container and the injected sidecar
// both mount the adapter volume at the shared path.
func assertAdapterMounts(t *testing.T, containers []corev1.Container, wantContainers int) {
	t.Helper()

	require.Len(t, containers, wantContainers)
	for _, container := range containers {
		assert.True(t, hasAdapterMount(container.VolumeMounts), "container %s does not mount %s at %s",
			container.Name, DefaultAdapterVolumeName, DefaultAdapterMountPath)
	}
}

func hasAdapterMount(mounts []corev1.VolumeMount) bool {
	for _, mount := range mounts {
		if mount.Name == DefaultAdapterVolumeName && mount.MountPath == DefaultAdapterMountPath {
			return true
		}
	}
	return false
}
