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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	webhookutils "github.com/vllm-project/aibrix/test/utils/webhook"
)

// These cases change only the ModelClaim admission entry to model upgrades and
// outages. Serial keeps those changes isolated from the other webhook specs.
var _ = ginkgo.Describe("ModelClaim admission", ginkgo.Serial, func() {
	var namespace *corev1.Namespace
	ginkgo.BeforeEach(func() {
		namespace = webhookutils.CreateNamespace(ctx, k8sClient, "modelclaim-")
		// Restore admission before deleting the namespace: recovery probes must
		// run while the namespace still accepts CREATE requests.
		ginkgo.DeferCleanup(func() { webhookutils.DeleteNamespace(ctx, k8sClient, namespace) })
	})
	newClaim := func(name string) *modelapi.ModelClaim {
		return &modelapi.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name},
			Spec: modelapi.ModelClaimSpec{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "warm"}},
				ArtifactURL: "huggingface://org/model",
				PerGPU: &modelapi.ModelClaimPerGPU{
					MaximumFootprint: resource.MustParse("2Gi"), KVFloor: resource.MustParse("1Gi"),
				},
			},
		}
	}

	ginkgo.DescribeTable("rejects static errors with field causes", func(change func(*modelapi.ModelClaim), field string) {
		claim := newClaim("invalid")
		change(claim)
		err := k8sClient.Create(ctx, claim)
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
		status, ok := err.(apierrors.APIStatus)
		gomega.Expect(ok).To(gomega.BeTrue())
		gomega.Expect(status.Status().Details).NotTo(gomega.BeNil())
		gomega.Expect(status.Status().Details.Causes).To(gomega.ContainElement(gomega.HaveField("Field", field)))
	},
		ginkgo.Entry("missing perGPU", func(c *modelapi.ModelClaim) {
			c.Spec.PerGPU = nil
		}, "spec.perGPU"),
		ginkgo.Entry("zero floor", func(c *modelapi.ModelClaim) {
			c.Spec.PerGPU.KVFloor = resource.MustParse("0")
		}, "spec.perGPU.kvFloor"),
		ginkgo.Entry("fractional bytes", func(c *modelapi.ModelClaim) {
			c.Spec.PerGPU.MaximumFootprint = resource.MustParse("30m")
		}, "spec.perGPU.maximumFootprint"),
		ginkgo.Entry("oversized footprint", func(c *modelapi.ModelClaim) {
			c.Spec.PerGPU.MaximumFootprint = resource.MustParse("2Pi")
		}, "spec.perGPU.maximumFootprint"),
		ginkgo.Entry("bad selector", func(c *modelapi.ModelClaim) {
			c.Spec.PodSelector.MatchLabels["pool"] = "bad value"
		}, "spec.podSelector.matchLabels"),
		ginkgo.Entry("blank artifact", func(c *modelapi.ModelClaim) { c.Spec.ArtifactURL = " \t" }, "spec.artifactURL"),
		ginkgo.Entry("blank model name", func(c *modelapi.ModelClaim) { c.Spec.ModelName = ptr.To(" ") }, "spec.modelName"),
		ginkgo.Entry("parallel alias", func(c *modelapi.ModelClaim) {
			c.Spec.EngineConfig = &modelapi.ModelClaimEngineConfig{Args: map[string]string{"-tp": "0"}}
		}, "spec.engineConfig.args"),
	)

	ginkgo.It("preserves defaults, perGPU updates and CEL immutability", func() {
		claim := newClaim("valid")
		claim.Spec.ArtifactURL = "/models/weights"
		gomega.Expect(k8sClient.Create(ctx, claim)).To(gomega.Succeed())
		gomega.Expect(claim.Spec.Engine).To(gomega.Equal("vllm"))
		gomega.Expect(claim.Spec.ModelName).To(gomega.BeNil())
		before := claim.DeepCopy()
		claim.Spec.PerGPU.MaximumFootprint = resource.MustParse("5.6Gi")
		gomega.Expect(k8sClient.Patch(ctx, claim, client.MergeFrom(before))).To(gomega.Succeed())
		changed := claim.DeepCopy()
		changed.Spec.ArtifactURL = "another/model"
		gomega.Expect(apierrors.IsInvalid(k8sClient.Patch(ctx, changed, client.MergeFrom(claim)))).To(gomega.BeTrue())
		claim.Status.Phase = modelapi.ModelClaimPending
		gomega.Expect(k8sClient.Status().Update(ctx, claim)).To(gomega.Succeed())
		before = claim.DeepCopy()
		claim.Spec.PerGPU = nil
		err := k8sClient.Patch(ctx, claim, client.MergeFrom(before))
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(gomega.Succeed())
		gomega.Expect(claim.Spec.PerGPU).NotTo(gomega.BeNil())
	})

	findConfiguration := func() (*admissionregistrationv1.ValidatingWebhookConfiguration, int) {
		configs := &admissionregistrationv1.ValidatingWebhookConfigurationList{}
		gomega.Expect(k8sClient.List(ctx, configs)).To(gomega.Succeed())
		for i := range configs.Items {
			for j := range configs.Items[i].Webhooks {
				if configs.Items[i].Webhooks[j].Name == "vmodelclaim.kb.io" {
					return configs.Items[i].DeepCopy(), j
				}
			}
		}
		ginkgo.Fail("ModelClaim webhook configuration is missing")
		return nil, 0
	}
	setWebhook := func(
		config *admissionregistrationv1.ValidatingWebhookConfiguration,
		index int, entry admissionregistrationv1.ValidatingWebhook,
	) {
		gomega.Eventually(func() error {
			current := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(config), current); err != nil {
				return err
			}
			current.Webhooks[index] = entry
			return k8sClient.Update(ctx, current)
		}, 10*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
	}
	waitForValidation := func() {
		gomega.Eventually(func() bool {
			probe := newClaim("probe")
			probe.Spec.PerGPU.KVFloor = resource.MustParse("0")
			return apierrors.IsInvalid(k8sClient.Create(ctx, probe, client.DryRunAll))
		}, 10*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	}

	ginkgo.DescribeTable("allows updates and deletion of claims stored before validation", func(missingPerGPU bool) {
		config, index := findConfiguration()
		original := *config.Webhooks[index].DeepCopy()
		ginkgo.DeferCleanup(func() { setWebhook(config, index, original); waitForValidation() })
		disabled := *original.DeepCopy()
		disabled.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"modelclaim-test-excluded": "true"}}
		setWebhook(config, index, disabled)
		legacy := newClaim("legacy")
		legacy.Spec.PerGPU.KVFloor = resource.MustParse("0")
		if missingPerGPU {
			legacy.Spec.PerGPU = nil
		}
		legacy.Spec.ArtifactURL = " "
		legacy.Finalizers = []string{"test.aibrix.ai/cleanup"}
		gomega.Eventually(func() error {
			return k8sClient.Create(ctx, legacy.DeepCopy(), client.DryRunAll)
		}, 10*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
		gomega.Expect(k8sClient.Create(ctx, legacy)).To(gomega.Succeed())
		setWebhook(config, index, original)
		waitForValidation()
		before := legacy.DeepCopy()
		legacy.Labels = map[string]string{"updated": "true"}
		gomega.Expect(k8sClient.Patch(ctx, legacy, client.MergeFrom(before))).To(gomega.Succeed())
		legacy.Status.Phase = modelapi.ModelClaimPending
		gomega.Expect(k8sClient.Status().Update(ctx, legacy)).To(gomega.Succeed())
		invalid := legacy.DeepCopy()
		if invalid.Spec.PerGPU == nil {
			invalid.Spec.PerGPU = newClaim("unused").Spec.PerGPU
		}
		invalid.Spec.PerGPU.KVFloor = resource.MustParse("-1Gi")
		gomega.Expect(apierrors.IsInvalid(k8sClient.Patch(ctx, invalid, client.MergeFrom(legacy)))).To(gomega.BeTrue())
		if !missingPerGPU {
			removed := legacy.DeepCopy()
			removed.Spec.PerGPU = nil
			gomega.Expect(apierrors.IsInvalid(k8sClient.Patch(ctx, removed, client.MergeFrom(legacy)))).To(gomega.BeTrue())
			before = legacy.DeepCopy()
			legacy.Spec.PerGPU.KVFloor = resource.MustParse("1Gi")
			gomega.Expect(k8sClient.Patch(ctx, legacy, client.MergeFrom(before))).To(gomega.Succeed())
		}
		gomega.Expect(k8sClient.Delete(ctx, legacy)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(legacy), legacy)).To(gomega.Succeed())
		before = legacy.DeepCopy()
		legacy.Finalizers = nil
		gomega.Expect(k8sClient.Patch(ctx, legacy, client.MergeFrom(before))).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(legacy), &modelapi.ModelClaim{}))
		}).Should(gomega.BeTrue())
	},
		ginkgo.Entry("with missing perGPU", true),
		ginkgo.Entry("with invalid perGPU", false),
	)

	ginkgo.It("admits writes while its endpoint is unavailable and resumes validation after recovery", func() {
		claim := newClaim("outage")
		claim.Finalizers = []string{"test.aibrix.ai/cleanup"}
		gomega.Expect(k8sClient.Create(ctx, claim)).To(gomega.Succeed())
		config, index := findConfiguration()
		original := *config.Webhooks[index].DeepCopy()
		ginkgo.DeferCleanup(func() { setWebhook(config, index, original); waitForValidation() })
		unavailable := *original.DeepCopy()
		gomega.Expect(unavailable.ClientConfig.URL).NotTo(gomega.BeNil())
		unavailable.ClientConfig.URL = ptr.To(*unavailable.ClientConfig.URL + "-unavailable")
		setWebhook(config, index, unavailable)
		// A healthy endpoint rejects this declaration. Its admission proves the
		// new configuration has propagated and endpoint errors are ignored.
		probe := newClaim("outage-probe")
		probe.Spec.PerGPU.KVFloor = resource.MustParse("0")
		gomega.Eventually(func() error {
			return k8sClient.Create(ctx, probe.DeepCopy(), client.DryRunAll)
		}, 10*time.Second, 100*time.Millisecond).Should(gomega.Succeed())
		gomega.Expect(k8sClient.Create(ctx, probe)).To(gomega.Succeed())
		before := claim.DeepCopy()
		claim.Spec.PerGPU.KVFloor = resource.MustParse("0")
		gomega.Expect(k8sClient.Patch(ctx, claim, client.MergeFrom(before))).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, claim)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(gomega.Succeed())
		before = claim.DeepCopy()
		claim.Finalizers = nil
		gomega.Expect(k8sClient.Patch(ctx, claim, client.MergeFrom(before))).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), &modelapi.ModelClaim{}))
		}).Should(gomega.BeTrue())
		setWebhook(config, index, original)
		waitForValidation()
		gomega.Expect(k8sClient.Create(ctx, newClaim("recovered"))).To(gomega.Succeed())
	})
})
