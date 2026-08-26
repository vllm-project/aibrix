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

package webhook

import (
	"strings"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/webhook"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

const (
	testRuntimeImage = "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.5.0"
)

var _ = ginkgo.Describe("stormservice default webhook", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		// Create test namespace before each test.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}

		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
		var stormservices orchestrationapi.StormServiceList
		gomega.Expect(k8sClient.List(ctx, &stormservices)).To(gomega.Succeed())

		for _, item := range stormservices.Items {
			gomega.Expect(k8sClient.Delete(ctx, &item)).To(gomega.Succeed())
		}
	})

	type testDefaultingCase struct {
		stormservice     func() *orchestrationapi.StormService
		wantStormService func() *orchestrationapi.StormService
	}

	ginkgo.DescribeTable("Defaulting test",
		func(tc *testDefaultingCase) {
			model := tc.stormservice()
			gomega.Expect(k8sClient.Create(ctx, model)).To(gomega.Succeed())
			want := tc.wantStormService()
			want.Spec.ProgressDeadlineSeconds = ptr.To(int32(600))
			gomega.Expect(model).To(gomega.BeComparableTo(want,
				cmpopts.IgnoreTypes(orchestrationapi.StormServiceStatus{}),
				cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID",
					"ResourceVersion", "Generation", "CreationTimestamp", "ManagedFields")),
			)
		},
		ginkgo.Entry("apply StormService with no sidecar injection annotation", &testDefaultingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("st-with-no-inject-sidecar").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
			},
			wantStormService: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("st-with-no-inject-sidecar").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
			},
		}),
		ginkgo.Entry("apply StormService with sidecar injection annotation", &testDefaultingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("st-with-inject-sidecar").
					Namespace(ns.Name).
					Annotations(map[string]string{webhook.SidecarInjectionAnnotation: "true"}).
					WithDefaultConfiguration().
					Obj()
			},
			wantStormService: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("st-with-inject-sidecar").
					Namespace(ns.Name).
					Annotations(map[string]string{webhook.SidecarInjectionAnnotation: "true"}).
					WithDefaultConfiguration().
					WithSidecarInjection("").
					Obj()
			},
		}),
		ginkgo.Entry("apply StormService with sidecar injection annotation "+
			"and sidecar runtime image annotation", &testDefaultingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("st-with-inject-sidecar").
					Namespace(ns.Name).
					Annotations(map[string]string{
						webhook.SidecarInjectionAnnotation:             "true",
						webhook.SidecarInjectionRuntimeImageAnnotation: testRuntimeImage,
					}).
					WithDefaultConfiguration().
					Obj()
			},
			wantStormService: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("st-with-inject-sidecar").
					Namespace(ns.Name).
					Annotations(map[string]string{
						webhook.SidecarInjectionAnnotation:             "true",
						webhook.SidecarInjectionRuntimeImageAnnotation: testRuntimeImage,
					}).
					WithDefaultConfiguration().
					WithSidecarInjection(testRuntimeImage).
					Obj()
			},
		}),
	)

	type testValidatingCase struct {
		stormservice  func() *orchestrationapi.StormService
		failed        bool
		expectInvalid bool
		// wantErr, when set, must appear in the admission error message.
		wantErr string
	}
	ginkgo.DescribeTable("test validating",
		func(tc *testValidatingCase) {
			err := k8sClient.Create(ctx, tc.stormservice())
			if tc.failed {
				gomega.Expect(err).Should(gomega.HaveOccurred())
				if tc.expectInvalid {
					gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(),
						"expected schema validation error, got %v", err)
				}
				if tc.wantErr != "" {
					gomega.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantErr))
				}
			} else {
				gomega.Expect(err).To(gomega.Succeed())
			}
		},
		// Valid StormService with short name and default config (includes sidecar annotations).
		ginkgo.Entry("accepts valid configuration", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("valid-storm").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
			},
			failed: false,
		}),

		ginkgo.Entry("rejects zero progress deadline", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				stormService := wrapper.MakeStormService("zero-progress-deadline").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
				stormService.Spec.ProgressDeadlineSeconds = ptr.To(int32(0))
				return stormService
			},
			failed:        true,
			expectInvalid: true,
		}),

		ginkgo.Entry("rejects negative progress deadline", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				stormService := wrapper.MakeStormService("negative-progress-deadline").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
				stormService.Spec.ProgressDeadlineSeconds = ptr.To(int32(-1))
				return stormService
			},
			failed:        true,
			expectInvalid: true,
		}),

		ginkgo.Entry("rejects a missing nested RoleSet spec", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				stormService := wrapper.MakeStormService("missing-nested-spec").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
				stormService.Spec.Template.Spec = nil
				return stormService
			},
			failed:        true,
			expectInvalid: true,
		}),

		// StormService name exceeds 63 characters → rejected by Kubernetes naming rules.
		ginkgo.Entry("rejects StormService name longer than 63 chars", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService(strings.Repeat("x", 64)).
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
			},
			failed: true,
		}),

		// Combined length of StormService name (50) + role name (20) + estimated suffix (~40)
		// exceeds 63 → rejected because podGroupSize=2 triggers PodSet creation.
		ginkgo.Entry("rejects combined service+role name too long (PodSet enabled)", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				podGroupSize := int32(2)
				return wrapper.MakeStormService(strings.Repeat("s", 50)).
					Namespace(ns.Name).
					WithDefaultConfiguration().
					WithRole(strings.Repeat("r", 20), false, &podGroupSize).
					Obj()
			},
			failed: true,
		}),

		// Boundary case: service name (12) + role name (11) + suffix (~40) = 63 → accepted.
		ginkgo.Entry("accepts boundary case (estimated length exactly 63)", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				podGroupSize := int32(2)
				return wrapper.MakeStormService(strings.Repeat("a", 12)).
					Namespace(ns.Name).
					WithDefaultConfiguration().
					WithRole(strings.Repeat("b", 11), false, &podGroupSize).
					Obj()
			},
			failed: false,
		}),

		// Multiple roles: validation uses the longest role name (35 chars).
		// Estimated PodSet name length = 30 (service) + 35 (role) + 40 > 63,
		// exceeds 63 → rejected because podGroupSize=2 triggers PodSet creation.
		ginkgo.Entry("rejects when longest role name causes overflow", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				podGroupSize := int32(2)
				return wrapper.MakeStormService(strings.Repeat("s", 30)).
					Namespace(ns.Name).
					WithDefaultConfiguration().
					WithRole("short", false, &podGroupSize).                 // len=5
					WithRole(strings.Repeat("l", 35), false, &podGroupSize). // len=35 → determines outcome
					Obj()
			},
			failed: true,
		}),

		// Explicit Pooled mode runs a single RoleSet, so spec.replicas > 1 is rejected.
		ginkgo.Entry("rejects explicit Pooled mode with replicas > 1", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("pooled-multi-replica").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Mode(orchestrationapi.StormServicePooledMode).
					Replicas(ptr.To(int32(2))).
					Obj()
			},
			failed: true,
		}),

		ginkgo.Entry("accepts explicit Pooled mode with a single replica", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("pooled-single-replica").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Mode(orchestrationapi.StormServicePooledMode).
					Obj()
			},
			failed: false,
		}),

		// Objects that leave spec.mode empty keep the inferred behavior and scale freely.
		ginkgo.Entry("accepts replicas > 1 when mode is not declared", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("undeclared-mode-multi").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Replicas(ptr.To(int32(3))).
					Obj()
			},
			failed: false,
		}),

		// Declared Replica mode drives the rolling update path, so a declared
		// InPlaceUpdate strategy is a contradiction and is rejected.
		ginkgo.Entry("rejects explicit Replica mode with InPlaceUpdate strategy", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("replica-inplace-conflict").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Mode(orchestrationapi.StormServiceReplicaMode).
					Replicas(ptr.To(int32(3))).
					UpdateStrategyType(orchestrationapi.InPlaceUpdateStormServiceStrategyType).
					Obj()
			},
			failed: true,
		}),

		ginkgo.Entry("accepts explicit Replica mode with RollingUpdate strategy", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("replica-rolling").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Mode(orchestrationapi.StormServiceReplicaMode).
					Replicas(ptr.To(int32(3))).
					Obj()
			},
			failed: false,
		}),

		// A RollingUpdate type may come from CRD defaulting, so an explicit Pooled
		// mode does not reject it; the controller resolves the path from the mode.
		ginkgo.Entry("accepts explicit Pooled mode with defaulted RollingUpdate strategy", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("pooled-defaulted-rolling").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Mode(orchestrationapi.StormServicePooledMode).
					Obj()
			},
			failed: false,
		}),

		// Undeclared mode keeps choosing InPlaceUpdate freely (legacy in-place
		// updates for inferred replica mode stay valid).
		ginkgo.Entry("accepts InPlaceUpdate with replicas > 1 when mode is not declared", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("undeclared-mode-inplace").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Replicas(ptr.To(int32(3))).
					UpdateStrategyType(orchestrationapi.InPlaceUpdateStormServiceStrategyType).
					Obj()
			},
			failed: false,
		}),

		// Volcano gang scheduling: the webhook rejects configurations that the RoleSet controller
		// or Volcano cannot honour. The default roles are "worker" (roles[0]) and "master" (roles[1]).
		ginkgo.Entry("accepts a valid Volcano gang", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-valid", ns.Name,
					volcanoStrategy(3, map[string]int32{"master": 1, "worker": 2}), nil)
			},
			failed: false,
		}),
		ginkgo.Entry("accepts a Volcano gang without minTaskMember", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-no-task-member", ns.Name, volcanoStrategy(2, nil), nil)
			},
			failed: false,
		}),
		ginkgo.Entry("rejects Volcano minMember of zero", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-min-member-zero", ns.Name, volcanoStrategy(0, nil), nil)
			},
			failed:        true,
			expectInvalid: true,
			wantErr:       "spec.template.spec.schedulingStrategy.volcanoSchedulingStrategy.minMember",
		}),
		ginkgo.Entry("rejects a Volcano minTaskMember value of zero", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-task-member-zero", ns.Name,
					volcanoStrategy(2, map[string]int32{"master": 0, "worker": 2}), nil)
			},
			failed:        true,
			expectInvalid: true,
			wantErr:       "volcanoSchedulingStrategy.minTaskMember[master]",
		}),
		ginkgo.Entry("rejects a Volcano minTaskMember key that is not a role name", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-unknown-role", ns.Name,
					volcanoStrategy(2, map[string]int32{"prefill": 2}), nil)
			},
			failed:        true,
			expectInvalid: true,
			wantErr:       "volcanoSchedulingStrategy.minTaskMember[prefill]",
		}),
		ginkgo.Entry("rejects Volcano minMember below the sum of minTaskMember", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-min-member-low", ns.Name,
					volcanoStrategy(2, map[string]int32{"master": 1, "worker": 2}), nil)
			},
			failed:        true,
			expectInvalid: true,
			wantErr:       "sum of minTaskMember values (3)",
		}),
		ginkgo.Entry("rejects RoleSet-level and Role-level scheduling strategies together", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-both-levels", ns.Name, volcanoStrategy(2, nil),
					map[string]*orchestrationapi.SchedulingStrategy{"worker": volcanoStrategy(1, nil)})
			},
			failed:        true,
			expectInvalid: true,
			wantErr:       "spec.template.spec.roles[0].schedulingStrategy: Forbidden",
		}),
		ginkgo.Entry("accepts a Role-level Volcano gang keyed by its own role", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-role-level", ns.Name, nil,
					map[string]*orchestrationapi.SchedulingStrategy{"worker": volcanoStrategy(1, map[string]int32{"worker": 1})})
			},
			failed: false,
		}),
		ginkgo.Entry("rejects a Role-level Volcano gang keyed by another role", &testValidatingCase{
			stormservice: func() *orchestrationapi.StormService {
				return gangStormService("gang-role-level-wrong-key", ns.Name, nil,
					map[string]*orchestrationapi.SchedulingStrategy{"worker": volcanoStrategy(1, map[string]int32{"master": 1})})
			},
			failed:        true,
			expectInvalid: true,
			wantErr:       "spec.template.spec.roles[0].schedulingStrategy.volcanoSchedulingStrategy.minTaskMember[master]",
		}),
	)

	ginkgo.It("rejects scaling an explicit Pooled StormService above one replica", func() {
		stormService := wrapper.MakeStormService("pooled-scale-up").
			Namespace(ns.Name).
			WithDefaultConfiguration().
			Mode(orchestrationapi.StormServicePooledMode).
			Obj()
		gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

		stormService.Spec.Replicas = ptr.To(int32(3))
		gomega.Expect(k8sClient.Update(ctx, stormService)).ShouldNot(gomega.Succeed())
	})

	ginkgo.It("rejects an update that makes the Volcano gang invalid", func() {
		stormService := gangStormService("gang-update-invalid", ns.Name,
			volcanoStrategy(3, map[string]int32{"master": 1, "worker": 2}), nil)
		gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

		stormService.Spec.Template.Spec.SchedulingStrategy.VolcanoSchedulingStrategy.MinMember = 0
		err := k8sClient.Update(ctx, stormService)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "expected an Invalid error, got %v", err)
	})

	ginkgo.It("accepts an update that keeps the Volcano gang valid", func() {
		stormService := gangStormService("gang-update-valid", ns.Name,
			volcanoStrategy(3, map[string]int32{"master": 1, "worker": 2}), nil)
		gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

		stormService.Spec.Template.Spec.SchedulingStrategy.VolcanoSchedulingStrategy.MinMember = 4
		gomega.Expect(k8sClient.Update(ctx, stormService)).To(gomega.Succeed())
	})
})

// volcanoStrategy returns a scheduling strategy that gang schedules through Volcano.
func volcanoStrategy(minMember int32, minTaskMember map[string]int32) *orchestrationapi.SchedulingStrategy {
	return &orchestrationapi.SchedulingStrategy{
		VolcanoSchedulingStrategy: &orchestrationapi.VolcanoSchedulingStrategySpec{
			MinMember:     minMember,
			MinTaskMember: minTaskMember,
		},
	}
}

// gangStormService builds a StormService with the default "worker" (roles[0]) and "master"
// (roles[1]) roles, a RoleSet-level strategy, and per-role strategies keyed by role name.
func gangStormService(name, namespace string, roleSetStrategy *orchestrationapi.SchedulingStrategy,
	roleStrategies map[string]*orchestrationapi.SchedulingStrategy) *orchestrationapi.StormService {
	stormService := wrapper.MakeStormService(name).
		Namespace(namespace).
		WithDefaultConfiguration().
		Obj()
	stormService.Spec.Template.Spec.SchedulingStrategy = roleSetStrategy
	for i := range stormService.Spec.Template.Spec.Roles {
		role := &stormService.Spec.Template.Spec.Roles[i]
		role.SchedulingStrategy = roleStrategies[role.Name]
	}
	return stormService
}
