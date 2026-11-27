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

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestSidecarInjectionKeepsStorage(t *testing.T) {
	pvc := &corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: "adapter-cache",
	}}
	for _, workload := range []string{"Deployment", "StormService"} {
		for _, tc := range []struct {
			name          string
			source        *corev1.VolumeSource
			existingMount bool
		}{
			{name: "PVC", source: pvc},
			{name: "PVC with existing mount", source: pvc, existingMount: true},
			{name: "hostPath", source: &corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: "/var/lib/adapters", Type: ptr.To(corev1.HostPathDirectory),
			}}},
			{name: "configured emptyDir", source: &corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("64Mi")),
			}}},
			{name: "absent volume"},
		} {
			t.Run(workload+"/"+tc.name, func(t *testing.T) {
				podSpec := corev1.PodSpec{
					Containers: []corev1.Container{{Name: "engine", Image: "vllm/vllm-openai:test"}},
					Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "engine-config"}},
					}}},
				}
				if tc.source != nil {
					podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
						Name: DefaultAdapterVolumeName, VolumeSource: *tc.source.DeepCopy(),
					})
				}
				mount := corev1.VolumeMount{Name: DefaultAdapterVolumeName, MountPath: DefaultAdapterMountPath}
				if tc.existingMount {
					mount.ReadOnly = true
					podSpec.Containers[0].VolumeMounts = []corev1.VolumeMount{mount}
				}
				wantVolumes := podSpec.DeepCopy().Volumes
				if tc.source == nil {
					wantVolumes = append(wantVolumes, corev1.Volume{
						Name: DefaultAdapterVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					})
				}
				meta := metav1.ObjectMeta{Annotations: map[string]string{SidecarInjectionAnnotation: "true"}}
				var actual *corev1.PodSpec
				var inject func() error
				if workload == "Deployment" {
					deployment := &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{
						Template: corev1.PodTemplateSpec{Spec: podSpec},
					}}
					actual = &deployment.Spec.Template.Spec
					inject = func() error { return (&DeploymentCustomDefaulter{}).Default(context.Background(), deployment) }
				} else {
					service := &orchestrationv1alpha1.StormService{ObjectMeta: meta, Spec: orchestrationv1alpha1.StormServiceSpec{
						Template: orchestrationv1alpha1.RoleSetTemplateSpec{Spec: &orchestrationv1alpha1.RoleSetSpec{
							Roles: []orchestrationv1alpha1.RoleSpec{{Name: "inference", Template: corev1.PodTemplateSpec{Spec: podSpec}}},
						}},
					}}
					actual = &service.Spec.Template.Spec.Roles[0].Template.Spec
					inject = func() error { return (&StormServiceCustomDefaulter{}).Default(context.Background(), service) }
				}

				require.NoError(t, inject())
				require.Equal(t, wantVolumes, actual.Volumes, "injection must preserve declared storage")
				require.Len(t, actual.Containers, 2)
				require.Equal(t, SidecarName, actual.Containers[0].Name)
				require.Equal(t, []corev1.VolumeMount{{Name: DefaultAdapterVolumeName, MountPath: DefaultAdapterMountPath}},
					actual.Containers[0].VolumeMounts)
				require.Equal(t, []corev1.VolumeMount{mount}, actual.Containers[1].VolumeMounts)
				injected := actual.DeepCopy()
				require.NoError(t, inject())
				require.Equal(t, injected, actual, "repeated admission must not duplicate storage or containers")
			})
		}
	}
}
