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
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

var _ = ginkgo.Describe("modelAdapter default and validation", func() {
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
		var adapters modelapi.ModelAdapterList
		gomega.Expect(k8sClient.List(ctx, &adapters)).To(gomega.Succeed())

		for _, item := range adapters.Items {
			gomega.Expect(k8sClient.Delete(ctx, &item)).To(gomega.Succeed())
		}
	})

	type testValidatingCase struct {
		adapter func() *modelapi.ModelAdapter
		failed  bool
	}
	ginkgo.DescribeTable("test validating",
		func(tc *testValidatingCase) {
			if tc.failed {
				gomega.Expect(k8sClient.Create(ctx, tc.adapter())).Should(gomega.HaveOccurred())
			} else {
				gomega.Expect(k8sClient.Create(ctx, tc.adapter())).To(gomega.Succeed())
			}
		},
		ginkgo.Entry("adapter creation with invalid artifactURL should be failed", &testValidatingCase{
			adapter: func() *modelapi.ModelAdapter {
				adapter := modelapi.ModelAdapter{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-adapter",
						Namespace: ns.Name,
					},
					Spec: modelapi.ModelAdapterSpec{
						ArtifactURL: "aabbcc",
						PodSelector: &metav1.LabelSelector{},
					},
				}
				return &adapter
			},
			failed: true,
		}),
	)
})
