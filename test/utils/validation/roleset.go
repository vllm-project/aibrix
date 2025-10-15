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

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

// TODO: Put some common validation logic here: e.g ValidateRoleSet ValidateRoleSetStatusEqualTo ...

func FindRoleStatus(rs *orchestrationapi.RoleSet, roleName string) *orchestrationapi.RoleStatus {
	for i := range rs.Status.Roles {
		if rs.Status.Roles[i].Name == roleName {
			return &rs.Status.Roles[i]
		}
	}
	return nil
}

func FindCondition(condType string, c []orchestrationapi.Condition) *orchestrationapi.Condition {
	for i := range c {
		if string(c[i].Type) == condType {
			cond := c[i] // copy
			return &cond
		}
	}
	return nil
}

// ValidateRoleSetSpec verify the roleSet spec
func ValidateRoleSetSpec(roleSet *orchestrationapi.RoleSet,
	expectedRoleCount int,
	updateStrategyType orchestrationapi.RoleSetUpdateStrategyType) {
	gomega.Expect(roleSet.Spec.UpdateStrategy).To(gomega.Equal(updateStrategyType))
	gomega.Expect(roleSet.Spec.Roles).To(gomega.HaveLen(expectedRoleCount))
}

// ValidateRoleStatus verify RoleStatus of a role
func ValidateRoleStatus(rs *orchestrationapi.RoleSet, role string, expectedReplicas int32) error {
	roleStatus := FindRoleStatus(rs, role)

	if roleStatus == nil {
		return fmt.Errorf("%s role status not found", role)
	}

	// Validate master status
	if roleStatus.Replicas != expectedReplicas {
		return fmt.Errorf("expected %s.Replicas=%d, got %d", role, expectedReplicas, roleStatus.Replicas)
	}
	if roleStatus.ReadyReplicas != expectedReplicas {
		return fmt.Errorf("expected %s.ReadyReplicas=%d, got %d", role, expectedReplicas, roleStatus.ReadyReplicas)
	}
	if roleStatus.NotReadyReplicas != 0 {
		return fmt.Errorf("expected %s.NotReadyReplicas=0, got %d", role, roleStatus.NotReadyReplicas)
	}
	if roleStatus.UpdatedReplicas != expectedReplicas {
		return fmt.Errorf("expected %s.UpdatedReplicas=%d, got %d",
			role, expectedReplicas, roleStatus.UpdatedReplicas)
	}
	if roleStatus.UpdatedReadyReplicas != expectedReplicas {
		return fmt.Errorf("expected %s.UpdatedReadyReplicas=%d, got %d",
			role, expectedReplicas, roleStatus.UpdatedReadyReplicas)
	}

	return nil
}

// GetRoleSetPodForRole list the pod for certain role
func GetRoleSetPodForRole(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet,
	ns, roleName string) []*corev1.Pod {
	podList := &corev1.PodList{}
	gomega.Expect(k8sClient.List(ctx, podList,
		client.InNamespace(ns),
		client.MatchingLabels{
			constants.RoleSetNameLabelKey: rs.Name,
			constants.RoleNameLabelKey:    roleName,
		})).To(gomega.Succeed())

	pods := make([]*corev1.Pod, len(podList.Items))
	for i := range podList.Items {
		pods[i] = &podList.Items[i]
	}
	return pods
}

// ValidateRoleImage Verify all pods of a certain role have the new images
func ValidateRoleImage(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet,
	ns, role, expectedImage string) error {

	pods := GetRoleSetPodForRole(ctx, k8sClient, rs, ns, role)

	for _, pod := range pods {
		if pod.Spec.Containers[0].Image != expectedImage {
			return fmt.Errorf("%s pod still has old image: %s", role, pod.Spec.Containers[0].Image)
		}
	}

	return nil

}

// roleHasImage verify role have expected image
func roleHasImage(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet,
	ns, role, expectedImage string) (*corev1.Pod, bool) {

	pods := GetRoleSetPodForRole(ctx, k8sClient, rs, ns, role)
	for _, pod := range pods {
		if pod.Spec.Containers[0].Image == expectedImage {
			return pod, true
		}
	}

	return nil, false

}

// WaitForRoleImage verify role have expected image and make the pod ready
func WaitForRoleImage(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet,
	ns, role, expectedImage string, description ...interface{}) {
	gomega.Eventually(func(g gomega.Gomega) bool {

		_, ok := roleHasImage(ctx, k8sClient, rs, ns, role, expectedImage)

		return ok

	}, time.Second*15, time.Millisecond*500).Should(gomega.BeTrue(),
		description...)
}

// WaitForRoleImageAndReady verify role have expected image and make the pod ready
func WaitForRoleImageAndReady(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet,
	ns, role, expectedImage string, description ...interface{}) {
	gomega.Eventually(func(g gomega.Gomega) bool {

		pod, ok := roleHasImage(ctx, k8sClient, rs, ns, role, expectedImage)
		if ok {
			MakePodReady(pod)
			g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
		}
		return ok

	}, time.Second*15, time.Millisecond*500).Should(gomega.BeTrue(),
		description...)
}

// WaitForRolesReady verify role in the role list is ready
func WaitForRolesReady(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet, roleList []string) {
	gomega.Eventually(func() error {
		latest := &orchestrationapi.RoleSet{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest); err != nil {
			return err
		}

		for _, role := range roleList {
			roleStatus := FindRoleStatus(latest, role)
			if roleStatus == nil {
				return fmt.Errorf("%s role status not found", role)
			}
			if roleStatus.ReadyReplicas == 0 {
				return fmt.Errorf("%s not ready yet", role)
			}
		}

		return nil
	}, time.Second*30, time.Millisecond*250).Should(gomega.Succeed())
}

// UpdateRolesTemplate update the template of roles
func UpdateRolesTemplate(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet,
	roleTemplateMap map[string]string) {
	gomega.Eventually(func(g gomega.Gomega) {
		latest := &orchestrationapi.RoleSet{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())

		for i := range latest.Spec.Roles {
			if _, ok := roleTemplateMap[latest.Spec.Roles[i].Name]; !ok {
				continue
			}

			latest.Spec.Roles[i].Template = MakePodTemplate(roleTemplateMap[latest.Spec.Roles[i].Name])

		}

		g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())

	}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
}

// WaitForRoleUpgradeInOrder verify the role upgrade order
func WaitForRoleUpgradeInOrder(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet, ns,
	firstRole, firstImage,
	secondRole, secondImage string) {
	gomega.Eventually(func() bool {
		_, firstRoleHasNewImage := roleHasImage(ctx, k8sClient, rs, ns, firstRole, firstImage)
		_, secondRoleHasNewImage := roleHasImage(ctx, k8sClient, rs, ns, secondRole, secondImage)
		if secondRoleHasNewImage && !firstRoleHasNewImage {
			return false
		}
		if firstRoleHasNewImage && secondRoleHasNewImage {
			return true
		}
		return false

	}, time.Second*45, time.Millisecond*500).Should(gomega.BeTrue(),
		"%s should start upgrading after %s starts", secondRole, firstRole)
}

// WaitForRoleSetFinalState verify roleSet final state
func WaitForRoleSetFinalState(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet,
	ns string, roleImageMap map[string]string) {
	gomega.Eventually(func() error {
		latest := &orchestrationapi.RoleSet{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest); err != nil {
			return err
		}

		for role, image := range roleImageMap {
			roleStatus := FindRoleStatus(latest, role)
			if roleStatus == nil {
				return fmt.Errorf("role status for %s not found", role)
			}
			if roleStatus.UpdatedReadyReplicas != roleStatus.Replicas {
				return fmt.Errorf("role %s not fully upgraded: %d/%d updated ready",
					role, roleStatus.UpdatedReadyReplicas, roleStatus.Replicas)
			}
			// verify role have the new image
			if err := ValidateRoleImage(ctx, k8sClient, rs, ns, role, image); err != nil {
				return err
			}
		}

		return nil
	}, time.Second*45, time.Millisecond*500).Should(gomega.Succeed(),
		"All roles should be fully upgraded with new images")
}

func StartRoleSetPodReadyLoop(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet,
	ns string) chan struct{} {
	stopChan := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-ticker.C:
				podList := &corev1.PodList{}
				if err := k8sClient.List(ctx, podList,
					client.InNamespace(ns),
					client.MatchingLabels{constants.RoleSetNameLabelKey: rs.Name}); err != nil {
					ginkgo.GinkgoLogr.Error(err, "Failed to list pods in startPodReadyHelper")
					continue
				}
				for i := range podList.Items {
					pod := &podList.Items[i]
					if pod.DeletionTimestamp != nil {
						continue
					}
					if pod.Status.Phase != corev1.PodRunning {
						MakePodReady(pod)
						if err := k8sClient.Status().Update(ctx, pod); err != nil {
							ginkgo.GinkgoLogr.Error(err, "Failed to update pod status in startPodReadyHelper",
								"pod", pod.Name)
						}
					}
				}
			}
		}
	}()
	return stopChan
}
