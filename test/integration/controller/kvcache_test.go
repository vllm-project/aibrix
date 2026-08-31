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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	kvcacheTimeout  = time.Second * 10
	kvcacheInterval = time.Millisecond * 250
)

var _ = ginkgo.Describe("KVCache controller test", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-kvcache-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)
		}, time.Second*3).Should(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.It("uses the default vineyard backend when the backend annotation is omitted", func() {
		kv := newIntegrationKVCache("default-backend", ns.Name, "")
		gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())
		kv = waitForKVCache(kv)

		deploy := waitForDeployment(ns.Name, kv.Name)
		expectControlledByKVCache(deploy, kv)
		gomega.Expect(deploy.Labels[constants.KVCacheLabelKeyIdentifier]).To(gomega.Equal(kv.Name))
		gomega.Expect(deploy.Labels[constants.KVCacheLabelKeyRole]).To(gomega.Equal(constants.KVCacheLabelValueRoleCache))
		gomega.Expect(deploy.Spec.Replicas).ToNot(gomega.BeNil())
		gomega.Expect(*deploy.Spec.Replicas).To(gomega.Equal(int32(1)))

		svc := waitForService(ns.Name, fmt.Sprintf("%s-rpc", kv.Name))
		expectControlledByKVCache(svc, kv)
		gomega.Expect(svc.Spec.Selector[constants.KVCacheLabelKeyIdentifier]).To(gomega.Equal(kv.Name))

		gomega.Consistently(func() bool {
			return isNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: kv.Name}, &appsv1.StatefulSet{}))
		}, time.Second*2, kvcacheInterval).Should(gomega.BeTrue())
	})

	ginkgo.It("does not create owned workloads for an unsupported backend", func() {
		kv := newIntegrationKVCache("unsupported-backend", ns.Name, "not-a-backend")
		gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())
		_ = waitForKVCache(kv)

		gomega.Consistently(func(g gomega.Gomega) {
			g.Expect(isNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: kv.Name}, &appsv1.Deployment{}))).To(gomega.BeTrue())
			g.Expect(isNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: kv.Name}, &appsv1.StatefulSet{}))).To(gomega.BeTrue())
			g.Expect(isNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: kv.Name + "-rpc"}, &corev1.Service{}))).To(gomega.BeTrue())
			g.Expect(isNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: kv.Name + "-headless-service"}, &corev1.Service{}))).To(gomega.BeTrue())
		}, time.Second*3, kvcacheInterval).Should(gomega.Succeed())
	})

	ginkgo.It("creates owned Service, Deployment, and StatefulSet resources with controller owner references", func() {
		vineyard := newIntegrationKVCache("owned-vineyard", ns.Name, constants.KVCacheBackendVineyard)
		gomega.Expect(k8sClient.Create(ctx, vineyard)).To(gomega.Succeed())
		vineyard = waitForKVCache(vineyard)

		vineyardDeploy := waitForDeployment(ns.Name, vineyard.Name)
		expectControlledByKVCache(vineyardDeploy, vineyard)
		vineyardSvc := waitForService(ns.Name, vineyard.Name+"-rpc")
		expectControlledByKVCache(vineyardSvc, vineyard)

		hpkv := newIntegrationKVCache("owned-hpkv", ns.Name, constants.KVCacheBackendHPKV)
		gomega.Expect(k8sClient.Create(ctx, hpkv)).To(gomega.Succeed())
		hpkv = waitForKVCache(hpkv)

		sts := waitForStatefulSet(ns.Name, hpkv.Name)
		expectControlledByKVCache(sts, hpkv)
		gomega.Expect(sts.Labels[constants.KVCacheLabelKeyIdentifier]).To(gomega.Equal(hpkv.Name))
		gomega.Expect(sts.Labels[constants.KVCacheLabelKeyRole]).To(gomega.Equal(constants.KVCacheLabelValueRoleCache))

		headless := waitForService(ns.Name, hpkv.Name+"-headless-service")
		expectControlledByKVCache(headless, hpkv)
		gomega.Expect(headless.Spec.ClusterIP).To(gomega.Equal(corev1.ClusterIPNone))
		gomega.Expect(headless.Spec.Selector[constants.KVCacheLabelKeyIdentifier]).To(gomega.Equal(hpkv.Name))
	})

	ginkgo.It("reconciles the matching KVCache when a labeled Pod is created or deleted", func() {
		kv := newIntegrationKVCache("pod-events", ns.Name, constants.KVCacheBackendVineyard)
		kv.Spec.Metadata = &orchestrationapi.MetadataSpec{
			Etcd: &orchestrationapi.MetadataConfig{
				Runtime: &orchestrationapi.RuntimeSpec{
					Replicas:        1,
					Image:           "etcd:3.5",
					ImagePullPolicy: "IfNotPresent",
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())
		kv = waitForKVCache(kv)

		deploy := waitForDeployment(ns.Name, kv.Name)
		expectControlledByKVCache(deploy, kv)
		rpc := waitForService(ns.Name, kv.Name+"-rpc")
		expectControlledByKVCache(rpc, kv)

		etcdPodName := fmt.Sprintf("%s-etcd-0", kv.Name)
		etcdPod := waitForPod(ns.Name, etcdPodName)
		gomega.Expect(etcdPod.Labels[constants.KVCacheLabelKeyIdentifier]).To(gomega.Equal(kv.Name))
		expectControlledByKVCache(etcdPod, kv)

		gomega.Expect(k8sClient.Delete(ctx, etcdPod)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			return isNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: etcdPodName}, &corev1.Pod{}))
		}, kvcacheTimeout, kvcacheInterval).Should(gomega.BeTrue())

		recreated := waitForPod(ns.Name, etcdPodName)
		gomega.Expect(recreated.Labels[constants.KVCacheLabelKeyIdentifier]).To(gomega.Equal(kv.Name))
		expectControlledByKVCache(recreated, kv)

		rpcCopy := rpc.DeepCopy()
		gomega.Expect(k8sClient.Delete(ctx, rpcCopy)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			return isNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: rpc.Name}, &corev1.Service{}))
		}, kvcacheTimeout, kvcacheInterval).Should(gomega.BeTrue())

		trigger := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "labeled-trigger",
				Namespace: ns.Name,
				Labels: map[string]string{
					constants.KVCacheLabelKeyIdentifier: kv.Name,
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "pause",
						Image: "registry.k8s.io/pause:3.9",
					},
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, trigger)).To(gomega.Succeed())

		recreatedSvc := waitForService(ns.Name, rpc.Name)
		expectControlledByKVCache(recreatedSvc, kv)
		gomega.Expect(recreatedSvc.Spec.Selector[constants.KVCacheLabelKeyIdentifier]).To(gomega.Equal(kv.Name))
	})
})

func newIntegrationKVCache(name, namespace, backend string) *orchestrationapi.KVCache {
	kv := &orchestrationapi.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: orchestrationapi.KVCacheSpec{
			Cache: orchestrationapi.RuntimeSpec{
				Replicas:        1,
				Image:           "aibrix/vineyardd:20241120",
				ImagePullPolicy: "IfNotPresent",
			},
			Service: orchestrationapi.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP,
				Ports: []corev1.ServicePort{
					{
						Name:       "rpc",
						Port:       9600,
						TargetPort: intstr.FromInt32(9600),
						Protocol:   corev1.ProtocolTCP,
					},
				},
			},
		},
	}
	if backend != "" {
		kv.Annotations = map[string]string{
			constants.KVCacheLabelKeyBackend: backend,
		}
	}
	return kv
}

func waitForKVCache(kv *orchestrationapi.KVCache) *orchestrationapi.KVCache {
	fetched := &orchestrationapi.KVCache{}
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(kv), fetched)
	}, kvcacheTimeout, kvcacheInterval).Should(gomega.Succeed())
	return fetched
}

func waitForDeployment(namespace, name string) *appsv1.Deployment {
	deploy := &appsv1.Deployment{}
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, deploy)
	}, kvcacheTimeout, kvcacheInterval).Should(gomega.Succeed())
	return deploy
}

func waitForStatefulSet(namespace, name string) *appsv1.StatefulSet {
	sts := &appsv1.StatefulSet{}
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, sts)
	}, kvcacheTimeout, kvcacheInterval).Should(gomega.Succeed())
	return sts
}

func waitForService(namespace, name string) *corev1.Service {
	svc := &corev1.Service{}
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, svc)
	}, kvcacheTimeout, kvcacheInterval).Should(gomega.Succeed())
	return svc
}

func waitForPod(namespace, name string) *corev1.Pod {
	pod := &corev1.Pod{}
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod)
	}, kvcacheTimeout, kvcacheInterval).Should(gomega.Succeed())
	return pod
}

func expectControlledByKVCache(obj metav1.Object, kv *orchestrationapi.KVCache) {
	gomega.Expect(obj.GetOwnerReferences()).ToNot(gomega.BeEmpty())
	gomega.Expect(metav1.IsControlledBy(obj, kv)).To(gomega.BeTrue())
	gomega.Expect(obj.GetOwnerReferences()[0].Kind).To(gomega.Equal("KVCache"))
	gomega.Expect(obj.GetOwnerReferences()[0].Name).To(gomega.Equal(kv.Name))
	gomega.Expect(obj.GetOwnerReferences()[0].UID).To(gomega.Equal(kv.UID))
	gomega.Expect(obj.GetOwnerReferences()[0].APIVersion).To(gomega.Equal(orchestrationapi.GroupVersion.String()))
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
