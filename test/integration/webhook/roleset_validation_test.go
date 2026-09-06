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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

var _ = ginkgo.Describe("RoleSet spec admission", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "roleset-validation-"},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
	})

	type invalidRolesCase struct {
		name  string
		roles []orchestrationapi.RoleSpec
	}

	invalidCases := []invalidRolesCase{
		{
			name:  "empty roles",
			roles: []orchestrationapi.RoleSpec{},
		},
		{
			name: "duplicate role names",
			roles: []orchestrationapi.RoleSpec{
				validRole("worker", 1),
				validRole("worker", 2),
			},
		},
		{
			name:  "non-DNS-1123 role name",
			roles: []orchestrationapi.RoleSpec{validRole("bad_role", 1)},
		},
		{
			name:  "negative replicas",
			roles: []orchestrationapi.RoleSpec{validRole("worker", -1)},
		},
		{
			name:  "negative drain timeout",
			roles: []orchestrationapi.RoleSpec{validRoleWithDrain("worker", 1, -1)},
		},
	}

	for _, tc := range invalidCases {
		tc := tc
		ginkgo.It("rejects "+tc.name+" on direct RoleSet creation", func() {
			roleSet := validRoleSet("direct-invalid", ns.Name, tc.roles)
			gomega.Expect(k8sClient.Create(ctx, roleSet)).To(gomega.HaveOccurred())
		})

		ginkgo.It("rejects "+tc.name+" on nested StormService creation", func() {
			stormService := validStormService("nested-invalid", ns.Name, tc.roles)
			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.HaveOccurred())
		})
	}

	ginkgo.It("accepts zero replicas and multiple distinct roles", func() {
		roles := []orchestrationapi.RoleSpec{
			validRole("leader", 0),
			validRole("worker", 2),
		}

		roleSet := validRoleSet("direct-valid", ns.Name, roles)
		gomega.Expect(k8sClient.Create(ctx, roleSet)).To(gomega.Succeed())

		stormService := validStormService("nested-valid", ns.Name, roles)
		gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())
	})

	ginkgo.It("accepts non-negative drain timeout on direct RoleSet and nested StormService creation", func() {
		roles := []orchestrationapi.RoleSpec{
			validRoleWithDrain("leader", 0, 0),
			validRoleWithDrain("worker", 2, 30),
		}

		roleSet := validRoleSet("direct-drain-valid", ns.Name, roles)
		gomega.Expect(k8sClient.Create(ctx, roleSet)).To(gomega.Succeed())

		stormService := validStormService("nested-drain-valid", ns.Name, roles)
		gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())
	})

	ginkgo.It("rejects an invalid direct RoleSet update", func() {
		roleSet := validRoleSet("direct-update", ns.Name, []orchestrationapi.RoleSpec{
			validRole("worker", 1),
		})
		gomega.Expect(k8sClient.Create(ctx, roleSet)).To(gomega.Succeed())

		roleSet.Spec.Roles = append(roleSet.Spec.Roles, validRole("worker", 2))
		gomega.Expect(k8sClient.Update(ctx, roleSet)).To(gomega.HaveOccurred())
	})

	ginkgo.It("rejects an invalid nested StormService update", func() {
		stormService := validStormService("nested-update", ns.Name, []orchestrationapi.RoleSpec{
			validRole("worker", 1),
		})
		gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

		stormService.Spec.Template.Spec.Roles[0].Replicas = ptr.To(int32(-1))
		gomega.Expect(k8sClient.Update(ctx, stormService)).To(gomega.HaveOccurred())
	})

	ginkgo.It("rejects a direct RoleSet update with negative drain timeout", func() {
		roleSet := validRoleSet("direct-drain-update", ns.Name, []orchestrationapi.RoleSpec{
			validRoleWithDrain("worker", 1, 1),
		})
		gomega.Expect(k8sClient.Create(ctx, roleSet)).To(gomega.Succeed())

		roleSet.Spec.Roles[0].Drain.TimeoutSeconds = ptr.To(int32(-1))
		gomega.Expect(k8sClient.Update(ctx, roleSet)).To(gomega.HaveOccurred())
	})

	ginkgo.It("rejects a nested StormService update with negative drain timeout", func() {
		stormService := validStormService("nested-drain-update", ns.Name, []orchestrationapi.RoleSpec{
			validRoleWithDrain("worker", 1, 1),
		})
		gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

		stormService.Spec.Template.Spec.Roles[0].Drain.TimeoutSeconds = ptr.To(int32(-1))
		gomega.Expect(k8sClient.Update(ctx, stormService)).To(gomega.HaveOccurred())
	})
})

func validRole(name string, replicas int32) orchestrationapi.RoleSpec {
	return orchestrationapi.RoleSpec{
		Name:     name,
		Replicas: ptr.To(replicas),
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "validation-test"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "runtime",
					Image: "example.invalid/runtime:latest",
				}},
			},
		},
	}
}

func validRoleWithDrain(name string, replicas, timeoutSeconds int32) orchestrationapi.RoleSpec {
	role := validRole(name, replicas)
	role.Drain = &orchestrationapi.RoleDrainSpec{
		TimeoutSeconds: ptr.To(timeoutSeconds),
	}
	return role
}

func validRoleSet(name, namespace string, roles []orchestrationapi.RoleSpec) *orchestrationapi.RoleSet {
	return &orchestrationapi.RoleSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orchestrationapi.RoleSetSpec{
			Roles:          copyRoles(roles),
			UpdateStrategy: orchestrationapi.SequentialRoleSetStrategyType,
		},
	}
}

func validStormService(name, namespace string, roles []orchestrationapi.RoleSpec) *orchestrationapi.StormService {
	stormService := wrapper.MakeStormService(name).
		Namespace(namespace).
		WithDefaultConfiguration().
		Obj()
	stormService.Spec.Template.Spec.Roles = copyRoles(roles)
	return stormService
}

func copyRoles(roles []orchestrationapi.RoleSpec) []orchestrationapi.RoleSpec {
	spec := &orchestrationapi.RoleSetSpec{Roles: roles}
	return spec.DeepCopy().Roles
}
