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
		ginkgo.Entry("zero retry limit", func(w *modelapi.ModelWarmup) {
			w.Spec.Policies = &modelapi.ModelWarmupPolicies{RetryLimit: ptr.To[int32](0)}
		}),
		ginkgo.Entry("negative finished job TTL", func(w *modelapi.ModelWarmup) {
			w.Spec.Policies = &modelapi.ModelWarmupPolicies{TTLSecondsAfterFinished: ptr.To[int32](-1)}
		}),
		ginkgo.Entry("duplicate image", func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images = append(w.Spec.ImagePreload.Images, w.Spec.ImagePreload.Images[0])
		}),
	)

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
