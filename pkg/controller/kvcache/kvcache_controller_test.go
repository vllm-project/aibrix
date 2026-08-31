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

package kvcache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/kvcache/backends"
)

func Test_getKVCacheBackendFromMetadata(t *testing.T) {
	testCases := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		expected    string
	}{
		{
			name: "valid backend annotation - vineyard",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: constants.KVCacheBackendVineyard,
			},
			expected: constants.KVCacheBackendVineyard,
		},
		{
			name: "valid backend annotation - infinistore",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: constants.KVCacheBackendInfinistore,
			},
			expected: constants.KVCacheBackendInfinistore,
		},
		{
			name: "valid backend annotation - hpkv",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: constants.KVCacheBackendHPKV,
			},
			expected: constants.KVCacheBackendHPKV,
		},
		{
			name:        "empty annotations uses default backend",
			annotations: map[string]string{},
			expected:    constants.KVCacheBackendDefault,
		},
		{
			name:        "missing annotations uses default backend",
			annotations: nil,
			expected:    constants.KVCacheBackendDefault,
		},
		{
			name: "empty backend annotation uses default backend",
			annotations: map[string]string{
				constants.KVCacheLabelKeyBackend: "",
			},
			expected: constants.KVCacheBackendDefault,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kv := &orchestrationv1alpha1.KVCache{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tc.annotations,
				},
			}
			result := getKVCacheBackendFromAnnotations(kv)
			assert.Equal(t, tc.expected, result)
			if tc.expected == constants.KVCacheBackendDefault {
				assert.Equal(t, constants.KVCacheBackendVineyard, result)
			}
		})
	}
}

func TestKVCacheReconciler_UnsupportedBackend(t *testing.T) {
	scheme := newKVCacheTestScheme(t)
	kv := newUnitTestKVCache("unsupported-cache", "default")
	kv.Annotations = map[string]string{
		constants.KVCacheLabelKeyBackend: "not-a-backend",
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kv).Build()
	r := newUnitTestReconciler(c, scheme)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: kv.Name, Namespace: kv.Namespace},
	})
	require.Error(t, err)
	assert.EqualError(t, err, "unsupported backend: not-a-backend")

	assertNoOwnedWorkloads(t, c, kv.Namespace, kv.Name)
}

func TestKVCacheReconciler_DefaultBackendCreatesOwnedResources(t *testing.T) {
	scheme := newKVCacheTestScheme(t)
	kv := newUnitTestKVCache("default-cache", "default")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kv).Build()
	r := newUnitTestReconciler(c, scheme)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: kv.Name, Namespace: kv.Namespace},
	})
	require.NoError(t, err)

	deploy := &appsv1.Deployment{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: kv.Name, Namespace: kv.Namespace}, deploy))
	assertControllerOwnerRef(t, deploy, kv)

	svc := &corev1.Service{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: kv.Name + "-rpc", Namespace: kv.Namespace}, svc))
	assertControllerOwnerRef(t, svc, kv)

	sts := &appsv1.StatefulSet{}
	err = c.Get(context.Background(), types.NamespacedName{Name: kv.Name, Namespace: kv.Namespace}, sts)
	assert.True(t, apierrors.IsNotFound(err), "default vineyard backend should not create a StatefulSet")
}

func TestKVCacheRequestsForPod(t *testing.T) {
	t.Run("maps identifier label to KVCache request", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cache-member",
				Namespace: "kv-ns",
				Labels: map[string]string{
					constants.KVCacheLabelKeyIdentifier: "my-kvcache",
				},
			},
		}

		reqs := kvCacheRequestsForPod(context.Background(), pod)
		require.Equal(t, []reconcile.Request{{
			NamespacedName: types.NamespacedName{Namespace: "kv-ns", Name: "my-kvcache"},
		}}, reqs)
	})

	t.Run("returns nil without identifier label", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unrelated",
				Namespace: "kv-ns",
				Labels: map[string]string{
					"app": "other",
				},
			},
		}
		assert.Nil(t, kvCacheRequestsForPod(context.Background(), pod))
	})

	t.Run("returns nil when labels are empty", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unlabeled",
				Namespace: "kv-ns",
			},
		}
		assert.Nil(t, kvCacheRequestsForPod(context.Background(), pod))
	})
}

func TestPodWithLabelFilter(t *testing.T) {
	pred := podWithLabelFilter(constants.KVCacheLabelKeyIdentifier)
	labeled := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "labeled",
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: "my-kvcache",
			},
		},
	}
	unlabeled := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unlabeled",
			Labels: map[string]string{
				"app": "other",
			},
		},
	}

	assert.True(t, pred.Create(event.CreateEvent{Object: labeled}))
	assert.False(t, pred.Create(event.CreateEvent{Object: unlabeled}))
	assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: labeled}))
	assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: unlabeled}))
	assert.True(t, pred.Delete(event.DeleteEvent{Object: labeled}))
	assert.False(t, pred.Delete(event.DeleteEvent{Object: unlabeled}))
	assert.True(t, pred.Generic(event.GenericEvent{Object: labeled}))
	assert.False(t, pred.Generic(event.GenericEvent{Object: unlabeled}))
}

func newKVCacheTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	return scheme
}

func newUnitTestReconciler(c client.Client, scheme *runtime.Scheme) *KVCacheReconciler {
	return &KVCacheReconciler{
		Client: c,
		Scheme: scheme,
		Backends: map[string]backends.BackendReconciler{
			constants.KVCacheBackendVineyard:    backends.NewVineyardReconciler(c),
			constants.KVCacheBackendHPKV:        backends.NewDistributedReconciler(c, constants.KVCacheBackendHPKV),
			constants.KVCacheBackendInfinistore: backends.NewDistributedReconciler(c, constants.KVCacheBackendInfinistore),
		},
	}
}

func newUnitTestKVCache(name, namespace string) *orchestrationv1alpha1.KVCache {
	return &orchestrationv1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name + "-uid"),
		},
		Spec: orchestrationv1alpha1.KVCacheSpec{
			Cache: orchestrationv1alpha1.RuntimeSpec{
				Replicas:        1,
				Image:           "aibrix/vineyardd:20241120",
				ImagePullPolicy: "IfNotPresent",
			},
			Service: orchestrationv1alpha1.ServiceSpec{
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
}

func assertControllerOwnerRef(t *testing.T, obj metav1.Object, kv *orchestrationv1alpha1.KVCache) {
	t.Helper()
	require.True(t, metav1.IsControlledBy(obj, kv))
	require.NotEmpty(t, obj.GetOwnerReferences())
	assert.Equal(t, "KVCache", obj.GetOwnerReferences()[0].Kind)
	assert.Equal(t, kv.Name, obj.GetOwnerReferences()[0].Name)
	assert.Equal(t, kv.UID, obj.GetOwnerReferences()[0].UID)
}

func assertNoOwnedWorkloads(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	ctx := context.Background()

	deploy := &appsv1.Deployment{}
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, deploy)
	assert.True(t, apierrors.IsNotFound(err), "expected no Deployment for unsupported backend")

	sts := &appsv1.StatefulSet{}
	err = c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, sts)
	assert.True(t, apierrors.IsNotFound(err), "expected no StatefulSet for unsupported backend")

	for _, svcName := range []string{name + "-rpc", name + "-headless-service"} {
		svc := &corev1.Service{}
		err = c.Get(ctx, types.NamespacedName{Name: svcName, Namespace: namespace}, svc)
		assert.True(t, apierrors.IsNotFound(err), "expected no Service %s for unsupported backend", svcName)
	}
}
