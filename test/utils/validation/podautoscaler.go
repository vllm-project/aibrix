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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

// ValidatePodAutoscalerSpec validates the spec fields of a PodAutoscaler.
func ValidatePodAutoscalerSpec(pa *autoscalingv1alpha1.PodAutoscaler,
	expectedMin, expectedMax int32,
	expectedStrategy autoscalingv1alpha1.ScalingStrategyType) {

	gomega.Expect(pa.Spec.MinReplicas).ToNot(gomega.BeNil(), "MinReplicas should not be nil")
	gomega.Expect(*pa.Spec.MinReplicas).To(gomega.Equal(expectedMin), "MinReplicas should match expected value")
	gomega.Expect(pa.Spec.MaxReplicas).To(gomega.Equal(expectedMax), "MaxReplicas should match expected value")
	gomega.Expect(pa.Spec.ScalingStrategy).To(
		gomega.Equal(expectedStrategy), "ScalingStrategy should match expected value")
}

// ValidatePodAutoscalerCondition validates a specific condition in a PodAutoscaler.
func ValidatePodAutoscalerCondition(pa *autoscalingv1alpha1.PodAutoscaler,
	conditionType string,
	expectedStatus metav1.ConditionStatus,
	expectedReason string) {

	var found *metav1.Condition
	for i := range pa.Status.Conditions {
		if pa.Status.Conditions[i].Type == conditionType {
			found = &pa.Status.Conditions[i]
			break
		}
	}

	gomega.Expect(found).ToNot(gomega.BeNil(),
		"condition %s should exist", conditionType)
	gomega.Expect(found.Status).To(gomega.Equal(expectedStatus),
		"condition %s status should be %s", conditionType, expectedStatus)
	if expectedReason != "" {
		gomega.Expect(found.Reason).To(gomega.Equal(expectedReason),
			"condition %s reason should be %s", conditionType, expectedReason)
	}
}

// ValidatePodAutoscalerConditionExists validates that a condition exists.
func ValidatePodAutoscalerConditionExists(pa *autoscalingv1alpha1.PodAutoscaler,
	conditionType string) {

	var found *metav1.Condition
	for i := range pa.Status.Conditions {
		if pa.Status.Conditions[i].Type == conditionType {
			found = &pa.Status.Conditions[i]
			break
		}
	}

	gomega.Expect(found).ToNot(gomega.BeNil(),
		"condition %s should exist", conditionType)
}

// ValidatePodAutoscalerConditionNotExists validates that a condition does not exist.
func ValidatePodAutoscalerConditionNotExists(pa *autoscalingv1alpha1.PodAutoscaler,
	conditionType string) {

	var found *metav1.Condition
	for i := range pa.Status.Conditions {
		if pa.Status.Conditions[i].Type == conditionType {
			found = &pa.Status.Conditions[i]
			break
		}
	}

	gomega.Expect(found).To(gomega.BeNil(),
		"condition %s should not exist", conditionType)
}

// ValidatePodAutoscalerScaling validates the scaling status (desiredScale and actualScale).
func ValidatePodAutoscalerScaling(ctx context.Context,
	k8sClient client.Client,
	pa *autoscalingv1alpha1.PodAutoscaler,
	expectedDesired, expectedActual int32) {

	gomega.Eventually(func(g gomega.Gomega) {
		fetched := &autoscalingv1alpha1.PodAutoscaler{}
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), fetched)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		g.Expect(fetched.Status.DesiredScale).To(gomega.Equal(expectedDesired),
			"DesiredScale should be %d", expectedDesired)
		g.Expect(fetched.Status.ActualScale).To(gomega.Equal(expectedActual),
			"ActualScale should be %d", expectedActual)
	}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
}

// ValidateScalingHistory validates the scalingHistory in status.
func ValidateScalingHistory(pa *autoscalingv1alpha1.PodAutoscaler,
	expectedCount int,
	checkLatest func(autoscalingv1alpha1.ScalingDecision)) {

	gomega.Expect(len(pa.Status.ScalingHistory)).To(gomega.Equal(expectedCount),
		"ScalingHistory should have %d entries", expectedCount)

	if checkLatest != nil && len(pa.Status.ScalingHistory) > 0 {
		latest := pa.Status.ScalingHistory[len(pa.Status.ScalingHistory)-1]
		checkLatest(latest)
	}
}

// WaitForPodAutoscalerCondition waits for a specific condition to reach expected status.
func WaitForPodAutoscalerCondition(ctx context.Context,
	k8sClient client.Client,
	pa *autoscalingv1alpha1.PodAutoscaler,
	conditionType string,
	expectedStatus metav1.ConditionStatus) {

	gomega.Eventually(func(g gomega.Gomega) {
		fetched := &autoscalingv1alpha1.PodAutoscaler{}
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), fetched)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		ValidatePodAutoscalerCondition(fetched, conditionType, expectedStatus, "")
	}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
}

// WaitForPodAutoscalerConditionWithReason waits for a condition with specific reason.
func WaitForPodAutoscalerConditionWithReason(ctx context.Context,
	k8sClient client.Client,
	pa *autoscalingv1alpha1.PodAutoscaler,
	conditionType string,
	expectedStatus metav1.ConditionStatus,
	expectedReason string) {

	gomega.Eventually(func(g gomega.Gomega) {
		fetched := &autoscalingv1alpha1.PodAutoscaler{}
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), fetched)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		ValidatePodAutoscalerCondition(fetched, conditionType, expectedStatus, expectedReason)
	}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
}

// WaitForPodAutoscalerCreated waits for PodAutoscaler to be created and returns the latest version.
func WaitForPodAutoscalerCreated(ctx context.Context,
	k8sClient client.Client,
	pa *autoscalingv1alpha1.PodAutoscaler) *autoscalingv1alpha1.PodAutoscaler {

	fetched := &autoscalingv1alpha1.PodAutoscaler{}
	gomega.Eventually(func(g gomega.Gomega) {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), fetched)
		g.Expect(err).ToNot(gomega.HaveOccurred())
	}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())

	return fetched
}

// WaitForPodAutoscalerDeleted waits for PodAutoscaler to be deleted.
func WaitForPodAutoscalerDeleted(ctx context.Context,
	k8sClient client.Client,
	pa *autoscalingv1alpha1.PodAutoscaler) {

	gomega.Eventually(func(g gomega.Gomega) {
		fetched := &autoscalingv1alpha1.PodAutoscaler{}
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), fetched)
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
	}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
}

// GetPodAutoscaler fetches the latest version of a PodAutoscaler.
func GetPodAutoscaler(ctx context.Context,
	k8sClient client.Client,
	pa *autoscalingv1alpha1.PodAutoscaler) *autoscalingv1alpha1.PodAutoscaler {

	fetched := &autoscalingv1alpha1.PodAutoscaler{}
	gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), fetched)).To(gomega.Succeed())
	return fetched
}
