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
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/webhook"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

const (
	testRuntimeImage = "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.4.0"
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
			gomega.Expect(model).To(gomega.BeComparableTo(tc.wantStormService(),
				cmpopts.IgnoreTypes(orchestrationapi.StormServiceStatus{}),
				cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID",
					"ResourceVersion", "Generation", "CreationTimestamp", "ManagedFields")),
			)
		},
		ginkgo.Entry("apply StormService with no sidecar injection annotation", &testDefaultingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("stormservice-with-no-inject-sidecar").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
			},
			wantStormService: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("stormservice-with-no-inject-sidecar").
					Namespace(ns.Name).
					WithDefaultConfiguration().
					Obj()
			},
		}),
		ginkgo.Entry("apply StormService with sidecar injection annotation", &testDefaultingCase{
			stormservice: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("stormservice-with-inject-sidecar").
					Namespace(ns.Name).
					Annotations(map[string]string{webhook.SidecarInjectionAnnotation: "true"}).
					WithDefaultConfiguration().
					Obj()
			},
			wantStormService: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("stormservice-with-inject-sidecar").
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
				return wrapper.MakeStormService("stormservice-with-inject-sidecar").
					Namespace(ns.Name).
					Annotations(map[string]string{
						webhook.SidecarInjectionAnnotation:             "true",
						webhook.SidecarInjectionRuntimeImageAnnotation: testRuntimeImage,
					}).
					WithDefaultConfiguration().
					Obj()
			},
			wantStormService: func() *orchestrationapi.StormService {
				return wrapper.MakeStormService("stormservice-with-inject-sidecar").
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
})
