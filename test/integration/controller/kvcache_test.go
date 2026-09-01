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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/kvcache"
	"github.com/vllm-project/aibrix/pkg/controller/kvcache/backends"
)

const (
	kvCacheTimeout  = time.Second * 15
	kvCacheInterval = time.Millisecond * 250
	// kvCacheQuietWindow is how long a resource must stay absent before we accept
	// that nothing else re-triggered reconciliation. Reconciles in envtest land in
	// milliseconds, so this leaves a wide margin without slowing the suite.
	kvCacheQuietWindow = time.Second * 1
)

// makeKVCache builds a minimal KVCache. An empty backend leaves the annotation
// unset, which exercises the controller's default-backend path.
func makeKVCache(namespace, name, backend string) *orchestrationapi.KVCache {
	kv := &orchestrationapi.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: orchestrationapi.KVCacheSpec{
			Cache: orchestrationapi.RuntimeSpec{
				Replicas: 1,
				Image:    "aibrix/kvcache:test",
			},
			// spec.service.ports is required by the CRD schema.
			Service: orchestrationapi.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP,
				Ports: []corev1.ServicePort{
					{Name: "service", Port: 9600, Protocol: corev1.ProtocolTCP},
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

// makePod builds a schedulable-looking Pod. When cacheName is non-empty the Pod
// carries the KVCache identifier label the controller's Pod watch filters on.
func makePod(namespace, name, cacheName string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:stable"},
			},
		},
	}
	if cacheName != "" {
		pod.Labels = map[string]string{
			constants.KVCacheLabelKeyIdentifier: cacheName,
		}
	}
	return pod
}

// newKVCacheReconciler mirrors the backend wiring the controller uses in
// production, so error paths asserted here match what the manager would hit.
func newKVCacheReconciler() *kvcache.KVCacheReconciler {
	hpkv := backends.NewDistributedReconciler(k8sClient, constants.KVCacheBackendHPKV)
	infinistore := backends.NewDistributedReconciler(k8sClient, constants.KVCacheBackendInfinistore)

	return &kvcache.KVCacheReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		Backends: map[string]backends.BackendReconciler{
			constants.KVCacheBackendVineyard:    backends.NewVineyardReconciler(k8sClient),
			constants.KVCacheBackendHPKV:        hpkv,
			constants.KVCacheBackendInfinistore: infinistore,
		},
	}
}

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
		// Tolerate NotFound so a failure in BeforeEach surfaces as itself rather
		// than being masked by a cleanup error.
		err := k8sClient.Delete(ctx, ns)
		if err != nil && !apierrors.IsNotFound(err) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
	})

	// expectControllerOwnedBy asserts obj carries exactly one owner reference and
	// that it is a controller reference back to the given KVCache.
	expectControllerOwnedBy := func(obj metav1.Object, owner *orchestrationapi.KVCache) {
		// Report failures at the caller's line rather than inside this helper.
		ginkgo.GinkgoHelper()

		refs := obj.GetOwnerReferences()
		gomega.Expect(refs).To(gomega.HaveLen(1))

		ref := refs[0]
		gomega.Expect(ref.Kind).To(gomega.Equal("KVCache"))
		gomega.Expect(ref.APIVersion).To(gomega.Equal(orchestrationapi.GroupVersion.String()))
		gomega.Expect(ref.Name).To(gomega.Equal(owner.Name))
		gomega.Expect(ref.UID).To(gomega.Equal(owner.UID))
		gomega.Expect(ref.Controller).NotTo(gomega.BeNil())
		gomega.Expect(*ref.Controller).To(gomega.BeTrue())
		gomega.Expect(ref.BlockOwnerDeletion).NotTo(gomega.BeNil())
		gomega.Expect(*ref.BlockOwnerDeletion).To(gomega.BeTrue())
	}

	// eventuallyGet waits until the named object exists.
	eventuallyGet := func(name string, obj client.Object) {
		ginkgo.GinkgoHelper()

		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: name}, obj)
		}, kvCacheTimeout, kvCacheInterval).Should(gomega.Succeed())
	}

	// consistentlyAbsent asserts the named object is not (re)created for the
	// duration of the quiet window.
	consistentlyAbsent := func(name string, obj client.Object) {
		ginkgo.GinkgoHelper()

		gomega.Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: name}, obj)
			return apierrors.IsNotFound(err)
		}, kvCacheQuietWindow, kvCacheInterval).Should(gomega.BeTrue())
	}

	ginkgo.Context("backend selection", func() {
		ginkgo.It("defaults to the vineyard backend when no backend annotation is set", func() {
			kv := makeKVCache(ns.Name, "default-backend", "")
			gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())

			// The vineyard backend is the configured default, so it must produce
			// the vineyard deployment and its rpc service without any annotation.
			deploy := &appsv1.Deployment{}
			eventuallyGet(kv.Name, deploy)
			gomega.Expect(deploy.Labels).To(gomega.HaveKeyWithValue(constants.KVCacheLabelKeyIdentifier, kv.Name))
			gomega.Expect(deploy.Labels).To(gomega.HaveKeyWithValue(
				constants.KVCacheLabelKeyRole, constants.KVCacheLabelValueRoleCache))
			gomega.Expect(deploy.Spec.Replicas).NotTo(gomega.BeNil())
			gomega.Expect(*deploy.Spec.Replicas).To(gomega.Equal(int32(1)))
			gomega.Expect(deploy.Spec.Selector.MatchLabels).To(
				gomega.HaveKeyWithValue(constants.KVCacheLabelKeyIdentifier, kv.Name))

			svc := &corev1.Service{}
			eventuallyGet(fmt.Sprintf("%s-rpc", kv.Name), svc)
			// The rpc Service must select the cache pods this KVCache owns.
			gomega.Expect(svc.Spec.Selector).To(gomega.HaveKeyWithValue(constants.KVCacheLabelKeyIdentifier, kv.Name))
			gomega.Expect(svc.Spec.Selector).To(gomega.HaveKeyWithValue(
				constants.KVCacheLabelKeyRole, constants.KVCacheLabelValueRoleCache))

			// The default path must not fall through to a distributed backend.
			sts := &appsv1.StatefulSet{}
			consistentlyAbsent(kv.Name, sts)
		})

		ginkgo.It("produces the same resources when vineyard is requested explicitly", func() {
			kv := makeKVCache(ns.Name, "explicit-vineyard", constants.KVCacheBackendVineyard)
			gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())

			deploy := &appsv1.Deployment{}
			eventuallyGet(kv.Name, deploy)

			svc := &corev1.Service{}
			eventuallyGet(fmt.Sprintf("%s-rpc", kv.Name), svc)
		})

		ginkgo.It("returns a reconcile error for an unsupported backend and creates nothing", func() {
			kv := makeKVCache(ns.Name, "bad-backend", "not-a-real-backend")
			gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())

			// Drive the reconciler directly: the manager retries this object with
			// backoff, so the returned error is only observable from a direct call.
			_, err := newKVCacheReconciler().Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: ns.Name, Name: kv.Name},
			})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.Equal("unsupported backend: not-a-real-backend"))

			// An unsupported backend must be inert, not partially applied.
			consistentlyAbsent(kv.Name, &appsv1.Deployment{})
			consistentlyAbsent(kv.Name, &appsv1.StatefulSet{})
			consistentlyAbsent(fmt.Sprintf("%s-rpc", kv.Name), &corev1.Service{})

			// Stop the manager's retry loop so it cannot add noise to later specs.
			gomega.Expect(k8sClient.Delete(ctx, kv)).To(gomega.Succeed())
		})
	})

	ginkgo.Context("owned resource ownership", func() {
		ginkgo.It("sets controller owner references on the vineyard Deployment and Service", func() {
			kv := makeKVCache(ns.Name, "owner-vineyard", constants.KVCacheBackendVineyard)
			gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())

			deploy := &appsv1.Deployment{}
			eventuallyGet(kv.Name, deploy)
			expectControllerOwnedBy(deploy, kv)

			svc := &corev1.Service{}
			eventuallyGet(fmt.Sprintf("%s-rpc", kv.Name), svc)
			expectControllerOwnedBy(svc, kv)
		})

		ginkgo.It("sets controller owner references on the infinistore StatefulSet and Service", func() {
			kv := makeKVCache(ns.Name, "owner-infinistore", constants.KVCacheBackendInfinistore)
			gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())

			sts := &appsv1.StatefulSet{}
			eventuallyGet(kv.Name, sts)
			expectControllerOwnedBy(sts, kv)
			gomega.Expect(sts.Labels).To(gomega.HaveKeyWithValue(constants.KVCacheLabelKeyIdentifier, kv.Name))
			gomega.Expect(sts.Labels).To(gomega.HaveKeyWithValue(
				constants.KVCacheLabelKeyRole, constants.KVCacheLabelValueRoleCache))
			gomega.Expect(sts.Spec.Replicas).NotTo(gomega.BeNil())
			gomega.Expect(*sts.Spec.Replicas).To(gomega.Equal(int32(1)))

			svc := &corev1.Service{}
			eventuallyGet(fmt.Sprintf("%s-headless-service", kv.Name), svc)
			expectControllerOwnedBy(svc, kv)
			// The distributed backends front their pods with a headless Service.
			gomega.Expect(svc.Spec.ClusterIP).To(gomega.Equal(corev1.ClusterIPNone))
			gomega.Expect(svc.Spec.Selector).To(gomega.HaveKeyWithValue(constants.KVCacheLabelKeyIdentifier, kv.Name))
		})
	})

	// The controller watches Pods carrying the KVCache identifier label and maps
	// them back to the named KVCache. StatefulSets are deliberately absent from
	// the controller's Owns() list, so deleting one produces no self-healing
	// event -- that makes its recreation an unambiguous signal that the Pod
	// event, and nothing else, drove the reconcile.
	ginkgo.Context("pod-triggered reconciliation", func() {
		ginkgo.It("reconciles the KVCache named by the Pod's identifier label", func() {
			kv := makeKVCache(ns.Name, "pod-trigger", constants.KVCacheBackendInfinistore)
			gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())

			sts := &appsv1.StatefulSet{}
			eventuallyGet(kv.Name, sts)

			gomega.Expect(k8sClient.Delete(ctx, sts)).To(gomega.Succeed())
			consistentlyAbsent(kv.Name, &appsv1.StatefulSet{})

			gomega.Expect(k8sClient.Create(ctx, makePod(ns.Name, "labeled-pod", kv.Name))).To(gomega.Succeed())

			eventuallyGet(kv.Name, &appsv1.StatefulSet{})
		})

		ginkgo.It("reconciles the matching KVCache when a labeled Pod is deleted", func() {
			kv := makeKVCache(ns.Name, "pod-delete", constants.KVCacheBackendInfinistore)
			gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())

			sts := &appsv1.StatefulSet{}
			eventuallyGet(kv.Name, sts)

			// Create the Pod first; its create event is not what we are testing.
			// Note: removing a Pod emits both an update (deletionTimestamp set)
			// and a delete event, and either is enough to enqueue the KVCache, so
			// this spec proves removal reconciles but cannot single out DeleteFunc.
			// Test_podWithLabelFilter pins each predicate branch individually.
			pod := makePod(ns.Name, "doomed-pod", kv.Name)
			gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())
			eventuallyGet(pod.Name, &corev1.Pod{})

			// Remove the unwatched StatefulSet and confirm nothing restores it,
			// so the only thing left that can is the Pod delete event.
			gomega.Expect(k8sClient.Delete(ctx, sts)).To(gomega.Succeed())
			consistentlyAbsent(kv.Name, &appsv1.StatefulSet{})

			gomega.Expect(k8sClient.Delete(ctx, pod)).To(gomega.Succeed())

			eventuallyGet(kv.Name, &appsv1.StatefulSet{})
		})

		ginkgo.It("ignores Pods without the identifier label", func() {
			kv := makeKVCache(ns.Name, "pod-no-label", constants.KVCacheBackendInfinistore)
			gomega.Expect(k8sClient.Create(ctx, kv)).To(gomega.Succeed())

			sts := &appsv1.StatefulSet{}
			eventuallyGet(kv.Name, sts)

			gomega.Expect(k8sClient.Delete(ctx, sts)).To(gomega.Succeed())

			gomega.Expect(k8sClient.Create(ctx, makePod(ns.Name, "unlabeled-pod", ""))).To(gomega.Succeed())

			// The predicate drops this Pod, so no reconcile is enqueued.
			consistentlyAbsent(kv.Name, &appsv1.StatefulSet{})
		})

		ginkgo.It("routes the Pod event only to the KVCache named by the label", func() {
			target := makeKVCache(ns.Name, "route-target", constants.KVCacheBackendInfinistore)
			other := makeKVCache(ns.Name, "route-other", constants.KVCacheBackendInfinistore)
			gomega.Expect(k8sClient.Create(ctx, target)).To(gomega.Succeed())
			gomega.Expect(k8sClient.Create(ctx, other)).To(gomega.Succeed())

			targetSts := &appsv1.StatefulSet{}
			otherSts := &appsv1.StatefulSet{}
			eventuallyGet(target.Name, targetSts)
			eventuallyGet(other.Name, otherSts)

			gomega.Expect(k8sClient.Delete(ctx, targetSts)).To(gomega.Succeed())
			gomega.Expect(k8sClient.Delete(ctx, otherSts)).To(gomega.Succeed())

			gomega.Expect(k8sClient.Create(ctx, makePod(ns.Name, "target-pod", target.Name))).To(gomega.Succeed())

			// Only the KVCache named by the label is reconciled.
			eventuallyGet(target.Name, &appsv1.StatefulSet{})
			consistentlyAbsent(other.Name, &appsv1.StatefulSet{})
		})
	})
})
