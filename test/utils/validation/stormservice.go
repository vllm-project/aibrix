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
	"fmt"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

func WaitForRoleSetsCreated(ctx context.Context, k8sClient client.Client, ns, ssName string, expected int) {
	gomega.Eventually(func(g gomega.Gomega) int {
		list := &orchestrationapi.RoleSetList{}
		g.Expect(k8sClient.List(ctx, list,
			client.InNamespace(ns),
			client.MatchingLabels{constants.StormServiceNameLabelKey: ssName})).To(gomega.Succeed())
		return len(list.Items)
	}, time.Second*10, time.Millisecond*250).Should(gomega.Equal(expected))
}

//nolint:dupl
func WaitForStormServicePodsCreated(ctx context.Context, k8sClient client.Client, ns,
	stormServiceLabel string, expected int) {
	gomega.Eventually(func(g gomega.Gomega) int {
		podList := &corev1.PodList{}
		g.Expect(k8sClient.List(ctx, podList,
			client.InNamespace(ns),
			client.MatchingLabels{constants.StormServiceNameLabelKey: stormServiceLabel},
		)).To(gomega.Succeed())
		return len(podList.Items)
	}, time.Second*10, time.Millisecond*250).Should(gomega.Equal(expected))
}

//nolint:dupl
func MarkStormServicePodsReady(ctx context.Context, k8sClient client.Client, ns, ssName string) {
	gomega.Eventually(func(g gomega.Gomega) {
		podList := &corev1.PodList{}
		g.Expect(k8sClient.List(ctx, podList,
			client.InNamespace(ns),
			client.MatchingLabels{constants.StormServiceNameLabelKey: ssName})).To(gomega.Succeed())

		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
					Reason: "TestReady",
				},
			}
			g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
		}
	}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
}

func ValidateStormServiceSpec(stormService *orchestrationapi.StormService, expectedReplicas int32,
	updateStrategyType orchestrationapi.StormServiceUpdateStrategyType) {
	gomega.Expect(stormService.Spec.Replicas).ToNot(gomega.BeNil())
	gomega.Expect(*stormService.Spec.Replicas).To(gomega.Equal(expectedReplicas))
	gomega.Expect(stormService.Spec.UpdateStrategy.Type).To(gomega.Equal(updateStrategyType))
}

func ValidateStormServiceStatus(ctx context.Context, k8sClient client.Client,
	stormService *orchestrationapi.StormService,
	expectedReplicas, expectedReady, expectedNotReady,
	expectedCurrent, expectedUpdated, expectedUpdatedReady int32,
	checkRevisions bool) {

	gomega.Eventually(func() error {
		latest := &orchestrationapi.StormService{}
		key := client.ObjectKeyFromObject(stormService)
		if err := k8sClient.Get(ctx, key, latest); err != nil {
			return fmt.Errorf("failed to get latest StormService: %w", err)
		}

		// Validate status
		if latest.Status.Replicas != expectedReplicas {
			return fmt.Errorf("expected Status.Replicas=%d, got %d", expectedReplicas, latest.Status.Replicas)
		}
		if latest.Status.ReadyReplicas != expectedReady {
			return fmt.Errorf("expected Status.ReadyReplicas=%d, got %d",
				expectedReady, latest.Status.ReadyReplicas)
		}
		if latest.Status.NotReadyReplicas != expectedNotReady {
			return fmt.Errorf("expected Status.NotReadyReplicas=%d, got %d",
				expectedNotReady, latest.Status.NotReadyReplicas)
		}
		if latest.Status.CurrentReplicas != expectedCurrent {
			return fmt.Errorf("expected Status.CurrentReplicas=%d, got %d",
				expectedCurrent, latest.Status.CurrentReplicas)
		}
		if latest.Status.UpdatedReplicas != expectedUpdated {
			return fmt.Errorf("expected Status.UpdatedReplicas=%d, got %d",
				expectedUpdated, latest.Status.UpdatedReplicas)
		}
		if latest.Status.UpdatedReadyReplicas != expectedUpdatedReady {
			return fmt.Errorf("expected Status.UpdatedReadyReplicas=%d, got %d",
				expectedUpdatedReady, latest.Status.UpdatedReadyReplicas)
		}

		// validate revision consistency
		if checkRevisions {
			if latest.Status.CurrentRevision == "" {
				return fmt.Errorf("current revision should not be empty")
			}
			if latest.Status.UpdateRevision == "" {
				return fmt.Errorf("update revision should not be empty")
			}
			if latest.Status.CurrentRevision != latest.Status.UpdateRevision {
				return fmt.Errorf("current revision %s should equal update revision %s",
					latest.Status.CurrentRevision, latest.Status.UpdateRevision)
			}
		}
		// validate Conditions
		if err := ValidateStormServiceConditions(latest,
			orchestrationapi.StormServiceReady, corev1.ConditionTrue); err != nil {
			return err
		}

		return nil
	}, time.Second*30, time.Second).Should(
		gomega.Succeed(), "StormService status validation failed")
}

func ValidateStormServiceConditions(ss *orchestrationapi.StormService,
	condition orchestrationapi.ConditionType, readyStatus corev1.ConditionStatus) error {
	readyCond := FindCondition(string(condition), ss.Status.Conditions)
	if readyCond == nil {
		return fmt.Errorf("StormServiceReady condition not found")
	}
	if readyCond.Status != readyStatus {
		return fmt.Errorf("expected StormServiceReady condition to be %s, got %s",
			readyStatus, readyCond.Status)
	}
	return nil
}

func ValidateStormServiceReplicas(
	ctx context.Context,
	k8sClient client.Client,
	stormService *orchestrationapi.StormService,
	expectedReplicas int32,
) {
	gomega.Eventually(func() (int32, error) {
		latest := &orchestrationapi.StormService{}
		key := client.ObjectKeyFromObject(stormService)
		if err := k8sClient.Get(ctx, key, latest); err != nil {
			return 0, fmt.Errorf("failed to get StormService: %w", err)
		}
		// todo: we should validate roleset and pod
		return latest.Status.Replicas, nil
	}, time.Second*10, time.Millisecond*250).Should(
		gomega.Equal(expectedReplicas),
		fmt.Sprintf("StormService %s/%s did not scale to %d replicas",
			stormService.Namespace, stormService.Name, expectedReplicas),
	)
}
