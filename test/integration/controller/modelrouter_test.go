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

package controller

import (
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

const (
	modelRouterTimeout  = time.Second * 10
	modelRouterInterval = time.Millisecond * 250
	aibrixSystemNS      = "aibrix-system"
)

var _ = ginkgo.Describe("ModelRouter controller test", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ensureAibrixSystemNamespace()
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-modelrouter-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)
		}, time.Second*3).Should(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		cleanupHTTPRoutesInAibrixSystem(ns.Name)
		err := k8sClient.Delete(ctx, ns)
		gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
	})

	ginkgo.It("creates an HTTPRoute from Deployment, ModelAdapter, and RayClusterFleet informer events", func() {
		deploymentModel := uniqueModelName(ns.Name, "deploy")
		adapterModel := uniqueModelName(ns.Name, "adapter")
		fleetModel := uniqueModelName(ns.Name, "fleet")

		createModelDeployment(ns.Name, "model-deploy", deploymentModel, nil)
		createModelAdapter(ns.Name, "model-adapter", adapterModel, nil)
		createModelRayClusterFleet(ns.Name, "model-fleet", fleetModel)

		for _, modelName := range []string{deploymentModel, adapterModel, fleetModel} {
			route := waitForHTTPRoute(modelName)
			gomega.Expect(route.Spec.ParentRefs).ToNot(gomega.BeEmpty())
			gomega.Expect(string(route.Spec.ParentRefs[0].Name)).To(gomega.Equal("aibrix-eg"))
			gomega.Expect(route.Spec.Rules).To(gomega.HaveLen(1))
			gomega.Expect(route.Spec.Rules[0].BackendRefs).To(gomega.HaveLen(1))
			backend := route.Spec.Rules[0].BackendRefs[0]
			gomega.Expect(string(backend.Name)).To(gomega.Equal(modelName))
			gomega.Expect(backend.Namespace).ToNot(gomega.BeNil())
			gomega.Expect(string(*backend.Namespace)).To(gomega.Equal(ns.Name))
			gomega.Expect(backend.Port).ToNot(gomega.BeNil())
			gomega.Expect(int32(*backend.Port)).To(gomega.Equal(int32(8000)))
			gomega.Expect(httpRoutePaths(route)).To(gomega.ContainElement("/v1/chat/completions"))
			gomega.Expect(route.Spec.Rules[0].Matches[0].Headers).To(gomega.ContainElement(gomega.HaveField("Value", modelName)))
		}
	})

	ginkgo.It("creates a cross-namespace ReferenceGrant for workloads outside aibrix-system", func() {
		modelName := uniqueModelName(ns.Name, "grant")
		createModelDeployment(ns.Name, "grant-deploy", modelName, nil)
		_ = waitForHTTPRoute(modelName)

		grant := waitForReferenceGrant(ns.Name)
		gomega.Expect(grant.Spec.From).To(gomega.HaveLen(1))
		gomega.Expect(string(grant.Spec.From[0].Group)).To(gomega.Equal(gatewayv1.GroupName))
		gomega.Expect(string(grant.Spec.From[0].Kind)).To(gomega.Equal("HTTPRoute"))
		gomega.Expect(string(grant.Spec.From[0].Namespace)).To(gomega.Equal(aibrixSystemNS))
		gomega.Expect(grant.Spec.To).To(gomega.HaveLen(1))
		gomega.Expect(string(grant.Spec.To[0].Group)).To(gomega.Equal(""))
		gomega.Expect(string(grant.Spec.To[0].Kind)).To(gomega.Equal("Service"))
	})

	ginkgo.It("cleans up HTTPRoute and ReferenceGrant after the last model workload is deleted", func() {
		modelName := uniqueModelName(ns.Name, "cleanup")
		deploy := createModelDeployment(ns.Name, "cleanup-deploy", modelName, nil)
		_ = waitForHTTPRoute(modelName)
		_ = waitForReferenceGrant(ns.Name)

		gomega.Expect(k8sClient.Delete(ctx, deploy)).To(gomega.Succeed())
		waitForHTTPRouteDeleted(modelName)
		waitForReferenceGrantDeleted(ns.Name)
	})

	ginkgo.It("keeps the ReferenceGrant while another model deployment remains in the namespace", func() {
		firstModel := uniqueModelName(ns.Name, "keep-a")
		secondModel := uniqueModelName(ns.Name, "keep-b")
		first := createModelDeployment(ns.Name, "keep-deploy-a", firstModel, nil)
		_ = createModelDeployment(ns.Name, "keep-deploy-b", secondModel, nil)
		_ = waitForHTTPRoute(firstModel)
		_ = waitForHTTPRoute(secondModel)
		_ = waitForReferenceGrant(ns.Name)

		gomega.Expect(k8sClient.Delete(ctx, first)).To(gomega.Succeed())
		waitForHTTPRouteDeleted(firstModel)
		gomega.Consistently(func() error {
			return k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns.Name,
				Name:      referenceGrantName(ns.Name),
			}, &gatewayv1beta1.ReferenceGrant{})
		}, time.Second*2, modelRouterInterval).Should(gomega.Succeed())
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKey{
				Namespace: aibrixSystemNS,
				Name:      utils.ModelRouterName(secondModel),
			}, &gatewayv1.HTTPRoute{})
		}, modelRouterTimeout, modelRouterInterval).Should(gomega.Succeed())
	})

	ginkgo.It("appends custom model router paths onto the HTTPRoute matches", func() {
		modelName := uniqueModelName(ns.Name, "paths")
		createModelDeployment(ns.Name, "paths-deploy", modelName, map[string]string{
			constants.ModelAnnoRouterCustomPath: "/score, /version",
		})
		route := waitForHTTPRoute(modelName)
		paths := httpRoutePaths(route)
		gomega.Expect(paths).To(gomega.ContainElement("/v1/completions"))
		gomega.Expect(paths).To(gomega.ContainElement("/v1/chat/completions"))
		gomega.Expect(paths[len(paths)-2:]).To(gomega.Equal([]string{"/score", "/version"}))
		for _, match := range route.Spec.Rules[0].Matches {
			gomega.Expect(match.Headers).ToNot(gomega.BeEmpty())
			gomega.Expect(match.Headers[0].Value).To(gomega.Equal(modelName))
		}
	})
})

func ensureAibrixSystemNamespace() {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: aibrixSystemNS,
		},
	}
	err := k8sClient.Create(ctx, ns)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
}

func cleanupHTTPRoutesInAibrixSystem(namespace string) {
	routes := &gatewayv1.HTTPRouteList{}
	if err := k8sClient.List(ctx, routes, client.InNamespace(aibrixSystemNS)); err != nil {
		return
	}
	for i := range routes.Items {
		route := &routes.Items[i]
		shouldDelete := false
		for _, rule := range route.Spec.Rules {
			for _, backend := range rule.BackendRefs {
				if backend.Namespace != nil && string(*backend.Namespace) == namespace {
					shouldDelete = true
					break
				}
			}
			if shouldDelete {
				break
			}
		}
		if shouldDelete {
			_ = k8sClient.Delete(ctx, route)
		}
	}
}

func uniqueModelName(namespace, suffix string) string {
	return fmt.Sprintf("%s-%s", suffix, namespace)
}

func modelLabels(modelName string) map[string]string {
	return map[string]string{
		constants.ModelLabelName: modelName,
		constants.ModelLabelPort: "8000",
	}
}

func createModelDeployment(namespace, name, modelName string, annotations map[string]string) *appsv1.Deployment {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      modelLabels(modelName),
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "vllm",
							Image: "vllm/vllm-openai:latest",
						},
					},
				},
			},
		},
	}
	gomega.Expect(k8sClient.Create(ctx, deploy)).To(gomega.Succeed())
	return deploy
}

func createModelAdapter(namespace, name, modelName string, annotations map[string]string) *modelapi.ModelAdapter {
	adapter := wrapper.MakeModelAdapter(name).
		Namespace(namespace).
		ArtifactURL("s3://test-bucket/test-adapter").
		PodSelector(&metav1.LabelSelector{
			MatchLabels: map[string]string{"app": "vllm"},
		}).
		Obj()
	adapter.Labels = modelLabels(modelName)
	adapter.Annotations = annotations
	gomega.Expect(k8sClient.Create(ctx, adapter)).To(gomega.Succeed())
	return adapter
}

func createModelRayClusterFleet(namespace, name, modelName string) *orchestrationapi.RayClusterFleet {
	matchLabels := map[string]string{"app": name}
	fleet := &orchestrationapi.RayClusterFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    modelLabels(modelName),
		},
		Spec: orchestrationapi.RayClusterFleetSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Template: orchestrationapi.RayClusterTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: matchLabels,
				},
				Spec: makeIntegrationRayClusterSpec(),
			},
		},
	}
	gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())
	return fleet
}

func waitForHTTPRoute(modelName string) *gatewayv1.HTTPRoute {
	route := &gatewayv1.HTTPRoute{}
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKey{
			Namespace: aibrixSystemNS,
			Name:      utils.ModelRouterName(modelName),
		}, route)
	}, modelRouterTimeout, modelRouterInterval).Should(gomega.Succeed())
	return route
}

func waitForHTTPRouteDeleted(modelName string) {
	gomega.Eventually(func() bool {
		err := k8sClient.Get(ctx, client.ObjectKey{
			Namespace: aibrixSystemNS,
			Name:      utils.ModelRouterName(modelName),
		}, &gatewayv1.HTTPRoute{})
		return apierrors.IsNotFound(err)
	}, modelRouterTimeout, modelRouterInterval).Should(gomega.BeTrue())
}

func referenceGrantName(namespace string) string {
	return fmt.Sprintf("%s-reserved-referencegrant-in-%s", aibrixSystemNS, namespace)
}

func waitForReferenceGrant(namespace string) *gatewayv1beta1.ReferenceGrant {
	grant := &gatewayv1beta1.ReferenceGrant{}
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      referenceGrantName(namespace),
		}, grant)
	}, modelRouterTimeout, modelRouterInterval).Should(gomega.Succeed())
	return grant
}

func waitForReferenceGrantDeleted(namespace string) {
	gomega.Eventually(func() bool {
		err := k8sClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      referenceGrantName(namespace),
		}, &gatewayv1beta1.ReferenceGrant{})
		return apierrors.IsNotFound(err)
	}, modelRouterTimeout, modelRouterInterval).Should(gomega.BeTrue())
}

func httpRoutePaths(route *gatewayv1.HTTPRoute) []string {
	if len(route.Spec.Rules) == 0 {
		return nil
	}
	paths := make([]string, 0, len(route.Spec.Rules[0].Matches))
	for _, match := range route.Spec.Rules[0].Matches {
		if match.Path != nil && match.Path.Value != nil {
			paths = append(paths, *match.Path.Value)
		}
	}
	return paths
}
