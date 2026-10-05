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

package validation

import (
	"context"
	"time"

	"github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WaitForHPACreated waits for an HPA to be created and returns it.
func WaitForHPACreated(ctx context.Context,
	k8sClient client.Client,
	namespace, name string) *autoscalingv2.HorizontalPodAutoscaler {

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	gomega.Eventually(func(g gomega.Gomega) {
		err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}, hpa)
		g.Expect(err).ToNot(gomega.HaveOccurred())
	}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())

	return hpa
}

// WaitForHPADeleted waits for an HPA to be deleted.
func WaitForHPADeleted(ctx context.Context,
	k8sClient client.Client,
	namespace, name string) {

	gomega.Eventually(func(g gomega.Gomega) {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}, hpa)
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
	}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
}

// ValidateHPAOwnerReference validates the HPA's OwnerReference.
func ValidateHPAOwnerReference(hpa *autoscalingv2.HorizontalPodAutoscaler,
	expectedOwnerName string,
	expectedOwnerKind string) {

	gomega.Expect(hpa.OwnerReferences).To(gomega.HaveLen(1),
		"HPA should have exactly one owner reference")

	ownerRef := hpa.OwnerReferences[0]
	gomega.Expect(ownerRef.Name).To(gomega.Equal(expectedOwnerName),
		"Owner name should be %s", expectedOwnerName)
	gomega.Expect(ownerRef.Kind).To(gomega.Equal(expectedOwnerKind),
		"Owner kind should be %s", expectedOwnerKind)
	gomega.Expect(ownerRef.Controller).ToNot(gomega.BeNil(),
		"Controller field should not be nil")
	gomega.Expect(*ownerRef.Controller).To(gomega.BeTrue(),
		"Controller field should be true")
}

// ValidateHPASpec validates the HPA spec fields.
func ValidateHPASpec(hpa *autoscalingv2.HorizontalPodAutoscaler,
	expectedMinReplicas, expectedMaxReplicas int32) {

	gomega.Expect(hpa.Spec.MinReplicas).ToNot(gomega.BeNil(),
		"HPA MinReplicas should not be nil")
	gomega.Expect(*hpa.Spec.MinReplicas).To(gomega.Equal(expectedMinReplicas),
		"HPA MinReplicas should be %d", expectedMinReplicas)
	gomega.Expect(hpa.Spec.MaxReplicas).To(gomega.Equal(expectedMaxReplicas),
		"HPA MaxReplicas should be %d", expectedMaxReplicas)
}

// ValidateHPAScaleTargetRef validates the HPA's scale target reference.
func ValidateHPAScaleTargetRef(hpa *autoscalingv2.HorizontalPodAutoscaler,
	expectedKind, expectedName string) {

	gomega.Expect(hpa.Spec.ScaleTargetRef.Kind).To(gomega.Equal(expectedKind),
		"HPA ScaleTargetRef kind should be %s", expectedKind)
	gomega.Expect(hpa.Spec.ScaleTargetRef.Name).To(gomega.Equal(expectedName),
		"HPA ScaleTargetRef name should be %s", expectedName)
}

// GetHPA fetches an HPA by namespace and name.
func GetHPA(ctx context.Context,
	k8sClient client.Client,
	namespace, name string) *autoscalingv2.HorizontalPodAutoscaler {

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, hpa)).To(gomega.Succeed())
	return hpa
}
