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
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func ValidatePodSetSpec(podset *orchestrationapi.PodSet, expectedPodGroupSize int32, expectedStateful bool) {
	gomega.Expect(podset.Spec.PodGroupSize).To(gomega.Equal(expectedPodGroupSize))
	gomega.Expect(podset.Spec.Stateful).To(gomega.Equal(expectedStateful))
}

func ValidatePodSetStatus(ctx context.Context, k8sClient client.Client,
	podset *orchestrationapi.PodSet, expectedPhase orchestrationapi.PodSetPhase, expectedTotal, expectedReady int32) {
	gomega.Eventually(func() error {
		latest := &orchestrationapi.PodSet{}
		key := client.ObjectKeyFromObject(podset)
		if err := k8sClient.Get(ctx, key, latest); err != nil {
			return fmt.Errorf("failed to get latest PodSet: %w", err)
		}
		if latest.Status.Phase != expectedPhase {
			return fmt.Errorf("expected Phase=%s, got %s", expectedPhase, latest.Status.Phase)
		}
		if latest.Status.TotalPods != expectedTotal {
			return fmt.Errorf("expected TotalPods=%d, got %d", expectedTotal, latest.Status.TotalPods)
		}
		if latest.Status.ReadyPods != expectedReady {
			return fmt.Errorf("expected ReadyPods=%d, got %d", expectedReady, latest.Status.ReadyPods)
		}
		return nil
	}, time.Second*30, time.Millisecond*250).Should(
		gomega.Succeed(), "PodSet status validation failed")
}
