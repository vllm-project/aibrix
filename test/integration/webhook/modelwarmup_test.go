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
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	webhookutils "github.com/vllm-project/aibrix/test/utils/webhook"
)

var _ = ginkgo.Describe("ModelWarmup admission", func() {
	var namespace *corev1.Namespace

	ginkgo.BeforeEach(func() {
		namespace = webhookutils.CreateNamespace(ctx, k8sClient, "modelwarmup-")
	})

	ginkgo.AfterEach(func() {
		webhookutils.DeleteNamespace(ctx, k8sClient, namespace)
	})

	newWarmup := func(name string) *modelapi.ModelWarmup {
		return &modelapi.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace.Name,
		}, Spec: modelapi.ModelWarmupSpec{
			Targets: []modelapi.ModelWarmupTarget{{Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{"node-a"}}}},
			ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
				Image: "busybox:1.36", Command: []string{"sh", "-c", "exit 0"},
			}}},
		}}
	}

	ginkgo.It("preserves omitted policies and image pull policy", func() {
		valid := newWarmup("defaults")
		gomega.Expect(k8sClient.Create(ctx, valid)).To(gomega.Succeed())
		gomega.Expect(valid.Spec.Policies).To(gomega.BeNil())
		gomega.Expect(valid.Spec.ImagePreload.Images[0].ImagePullPolicy).To(gomega.BeEmpty())
	})

	ginkgo.DescribeTable("preserves custom actions through admission and storage", func(customOnly bool) {
		warmup := newWarmup("custom")
		if customOnly {
			warmup.Spec.ImagePreload.Images = nil
		}
		custom := &modelapi.ModelWarmupCustomAction{
			InitContainers: []corev1.Container{{
				Name: "prepare", Image: "busybox:1.36", Command: []string{"sh", "-c", "touch /cache/ready"},
				VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
			}},
			Containers: []corev1.Container{{
				Name: "warm", Image: "busybox:1.36", Command: []string{"sh", "-c", "test -f /cache/ready"},
				Env: []corev1.EnvVar{{Name: "MODEL", Value: "example"}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: ptr.To[int64](1000), AllowPrivilegeEscalation: ptr.To(false),
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
			}},
			Volumes: []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("128Mi"))},
			}}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "custom-registry"}},
		}
		warmup.Spec.Custom = custom
		expectedCustom := custom.DeepCopy()
		gomega.Expect(k8sClient.Create(ctx, warmup)).To(gomega.Succeed())
		gomega.Expect(warmup.Spec.Custom).To(gomega.Equal(expectedCustom))

		stored := &modelapi.ModelWarmup{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(warmup), stored)).To(gomega.Succeed())
		gomega.Expect(stored.Spec.Custom).To(gomega.Equal(expectedCustom))
		if customOnly {
			gomega.Expect(stored.Spec.ImagePreload.Images).To(gomega.BeEmpty())
		} else {
			gomega.Expect(stored.Spec.ImagePreload.Images).To(gomega.Equal(warmup.Spec.ImagePreload.Images))
		}
	},
		ginkgo.Entry("custom-only", true),
		ginkgo.Entry("combined image and custom", false),
	)

	ginkgo.DescribeTable("rejects invalid specifications", func(mutate func(*modelapi.ModelWarmup)) {
		warmup := newWarmup("invalid")
		mutate(warmup)
		gomega.Expect(k8sClient.Create(ctx, warmup)).To(gomega.HaveOccurred())
	},
		ginkgo.Entry("empty target list", func(w *modelapi.ModelWarmup) {
			w.Spec.Targets = nil
		}),
		ginkgo.Entry("empty selector", func(w *modelapi.ModelWarmup) {
			w.Spec.Targets = []modelapi.ModelWarmupTarget{{NodeSelector: &metav1.LabelSelector{}}}
		}),
		ginkgo.Entry("missing image", func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images[0].Image = ""
		}),
		ginkgo.Entry("missing command", func(w *modelapi.ModelWarmup) { w.Spec.ImagePreload.Images[0].Command = nil }),
		ginkgo.Entry("invalid pull policy", func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images[0].ImagePullPolicy = "Invalid"
		}),
		ginkgo.Entry("zero parallelism", func(w *modelapi.ModelWarmup) {
			w.Spec.Policies = &modelapi.ModelWarmupPolicies{Parallelism: ptr.To[int32](0)}
		}),
		ginkgo.Entry("negative job timeout", func(w *modelapi.ModelWarmup) {
			w.Spec.Policies = &modelapi.ModelWarmupPolicies{JobTimeoutSeconds: ptr.To[int64](-1)}
		}),
		ginkgo.Entry("negative retry limit", func(w *modelapi.ModelWarmup) {
			w.Spec.Policies = &modelapi.ModelWarmupPolicies{RetryLimit: ptr.To[int32](-1)}
		}),
		ginkgo.Entry("negative finished job TTL", func(w *modelapi.ModelWarmup) {
			w.Spec.Policies = &modelapi.ModelWarmupPolicies{TTLSecondsAfterFinished: ptr.To[int32](-1)}
		}),
		ginkgo.Entry("duplicate image", func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images = append(w.Spec.ImagePreload.Images, w.Spec.ImagePreload.Images[0])
		}),
		ginkgo.Entry("custom init containers without a regular container", func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images = nil
			w.Spec.Custom = &modelapi.ModelWarmupCustomAction{InitContainers: []corev1.Container{{
				Name: "prepare", Image: "busybox:1.36",
			}}}
		}),
		ginkgo.Entry("custom container collides with a generated image name", func(w *modelapi.ModelWarmup) {
			w.Spec.Custom = &modelapi.ModelWarmupCustomAction{Containers: []corev1.Container{{
				Name: "image-0", Image: "busybox:1.36",
			}}}
		}),
		ginkgo.Entry("custom container references an undeclared volume", func(w *modelapi.ModelWarmup) {
			w.Spec.Custom = &modelapi.ModelWarmupCustomAction{Containers: []corev1.Container{{
				Name: "warm", Image: "busybox:1.36",
				VolumeMounts: []corev1.VolumeMount{{Name: "missing", MountPath: "/cache"}},
			}}}
		}),
	)

	ginkgo.It("accepts a zero retry limit", func() {
		warmup := newWarmup("no-retry")
		warmup.Spec.Policies = &modelapi.ModelWarmupPolicies{RetryLimit: ptr.To[int32](0)}
		gomega.Expect(k8sClient.Create(ctx, warmup)).To(gomega.Succeed())
	})

	ginkgo.It("rejects invalid spec updates and permits status updates", func() {
		warmup := newWarmup("update")
		gomega.Expect(k8sClient.Create(ctx, warmup)).To(gomega.Succeed())
		warmup.Spec.ImagePreload.Images[0].Command = nil
		gomega.Expect(k8sClient.Update(ctx, warmup)).To(gomega.HaveOccurred())

		latest := &modelapi.ModelWarmup{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(warmup), latest)).To(gomega.Succeed())
		latest.Status.Phase = modelapi.ModelWarmupRunning
		gomega.Expect(k8sClient.Status().Update(ctx, latest)).To(gomega.Succeed())
	})

	ginkgo.It("rejects every spec update", func() {
		warmup := newWarmup("immutable-update")
		gomega.Expect(k8sClient.Create(ctx, warmup)).To(gomega.Succeed())
		warmup.Spec.ImagePreload.Images[0].Args = []string{"-c", "exit 0"}
		warmup.Spec.ImagePreload.Images[0].ImagePullPolicy = corev1.PullAlways
		gomega.Expect(k8sClient.Update(ctx, warmup)).To(gomega.HaveOccurred())
	})
})
