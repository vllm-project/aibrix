/*
Copyright 2024 The Aibrix Team.

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

package backends

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// -- Test for NewDistributedReconciler --

func TestNewDistributedReconciler(t *testing.T) {
	c := fake.NewClientBuilder().Build()

	t.Run("infinistore backend", func(t *testing.T) {
		reconciler := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)
		assert.Equal(t, "infinistore", reconciler.Backend.Name())
	})

	t.Run("hpkv backend", func(t *testing.T) {
		reconciler := NewDistributedReconciler(c, constants.KVCacheBackendHPKV)
		assert.Equal(t, "hpkv", reconciler.Backend.Name())
	})

	t.Run("invalid backend panics", func(t *testing.T) {
		assert.Panics(t, func() {
			NewDistributedReconciler(c, "invalid-backend")
		})
	})
}

// -- Test for reconcileMetadataService --

func TestReconcileMetadataService(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)

	t.Run("fails when neither etcd nor redis configured", func(t *testing.T) {
		kv := &v1alpha1.KVCache{
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{},
			},
		}
		err := r.reconcileMetadataService(context.Background(), kv)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "either etcd or redis configuration is required")
	})

	t.Run("succeeds when redis is configured", func(t *testing.T) {
		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "redis-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						Runtime: &v1alpha1.RuntimeSpec{
							Replicas: 1,
						},
					},
				},
			},
		}

		// Replace backend with mock
		r.Backend = mockBackend{
			redis: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mock-redis",
					Namespace: "default",
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mock-redis-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "redis",
							Port:     6379,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
		}

		err := r.reconcileMetadataService(context.Background(), kv)
		assert.NoError(t, err)
	})
}

// -- Test for reconcileRedisService (partial stubbed) --

func TestReconcileRedisService_SingleReplicaAllowed(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)

	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redis-test",
			Namespace: "default",
		},
		Spec: v1alpha1.KVCacheSpec{
			Metadata: &v1alpha1.MetadataSpec{
				Redis: &v1alpha1.MetadataConfig{
					Runtime: &v1alpha1.RuntimeSpec{
						Replicas: 1,
					},
				},
			},
		},
	}

	// Stub backend logic
	r.Backend = mockBackend{
		// dummy objects
		redis: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mock-redis",
				Namespace: "default",
			},
		},
		svc: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mock-redis-service",
				Namespace: "default",
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{
						Name:     "redis",
						Port:     6379,
						Protocol: corev1.ProtocolTCP,
					},
				},
			},
		},
	}

	err := r.ReconcilePodObject(context.Background(), r.Backend.BuildMetadataPod(kv))
	assert.NoError(t, err)

	err = r.reconcileRedisService(context.Background(), kv)
	assert.NoError(t, err)
}

// -- Test for reconcileRedisService with ExternalConnection --

func TestReconcileRedisService_ExternalConnection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	t.Run("skips in-cluster deployment when external connection is set", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "valkey-credentials",
				Namespace: "default",
			},
			Data: map[string][]byte{
				"password": []byte("my-secret-password"),
			},
		}
		// Pre-create an in-cluster Redis Pod and Service to verify they get cleaned up.
		existingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-test-redis",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "redis", Image: "redis:7"}},
			},
		}
		existingSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-test-redis",
				Namespace: "default",
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 6379, Protocol: corev1.ProtocolTCP}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, existingPod, existingSvc).Build()
		r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)
		r.Backend = mockBackend{}

		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						ExternalConnection: &v1alpha1.ExternalConnectionConfig{
							Address:           "valkey.example.com:6379",
							PasswordSecretRef: "valkey-credentials",
						},
					},
				},
			},
		}

		err := r.reconcileRedisService(context.Background(), kv)
		assert.NoError(t, err)

		// Verify in-cluster Redis Pod was deleted.
		pod := &corev1.Pod{}
		err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "ext-test-redis"}, pod)
		assert.True(t, apierrors.IsNotFound(err), "expected in-cluster Redis Pod to be deleted")

		// Verify in-cluster Redis Service was deleted.
		svc := &corev1.Service{}
		err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "ext-test-redis"}, svc)
		assert.True(t, apierrors.IsNotFound(err), "expected in-cluster Redis Service to be deleted")
	})

	t.Run("fails when external address has no port", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)
		r.Backend = mockBackend{}

		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						ExternalConnection: &v1alpha1.ExternalConnectionConfig{
							Address: "valkey.example.com",
						},
					},
				},
			},
		}

		err := r.reconcileRedisService(context.Background(), kv)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must be in host:port format")
	})

	t.Run("fails when password secret does not exist", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)
		r.Backend = mockBackend{}

		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						ExternalConnection: &v1alpha1.ExternalConnectionConfig{
							Address:           "valkey.example.com:6379",
							PasswordSecretRef: "nonexistent-secret",
						},
					},
				},
			},
		}

		err := r.reconcileRedisService(context.Background(), kv)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to resolve PasswordSecretRef")
	})

	t.Run("fails when neither external connection nor runtime is set", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)
		r.Backend = mockBackend{}

		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{},
				},
			},
		}

		err := r.reconcileRedisService(context.Background(), kv)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "requires either externalConnection or runtime to be set")
	})

	t.Run("resolves secret with custom key format", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-secret",
				Namespace: "default",
			},
			Data: map[string][]byte{
				"redis-pass": []byte("custom-key-password"),
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)
		r.Backend = mockBackend{}

		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						ExternalConnection: &v1alpha1.ExternalConnectionConfig{
							Address:           "valkey.example.com:6379",
							PasswordSecretRef: "my-secret/redis-pass",
						},
					},
				},
			},
		}

		err := r.reconcileRedisService(context.Background(), kv)
		assert.NoError(t, err)
	})
}

// -- Test for cleanupInClusterRedis --

func TestCleanupInClusterRedis(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	t.Run("deletes existing in-cluster Redis Pod and Service", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cleanup-test-redis",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "redis", Image: "redis:7"}},
			},
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cleanup-test-redis",
				Namespace: "default",
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 6379, Protocol: corev1.ProtocolTCP}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, svc).Build()
		r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)

		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cleanup-test",
				Namespace: "default",
			},
		}

		err := r.cleanupInClusterRedis(context.Background(), kv)
		assert.NoError(t, err)

		// Verify Pod is gone.
		err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "cleanup-test-redis"}, &corev1.Pod{})
		assert.True(t, apierrors.IsNotFound(err), "expected Redis Pod to be deleted")

		// Verify Service is gone.
		err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "cleanup-test-redis"}, &corev1.Service{})
		assert.True(t, apierrors.IsNotFound(err), "expected Redis Service to be deleted")
	})

	t.Run("no-op when resources do not exist", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)

		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "no-redis",
				Namespace: "default",
			},
		}

		err := r.cleanupInClusterRedis(context.Background(), kv)
		assert.NoError(t, err)
	})
}

// -- Test for watcher pod env wiring --

func TestBuildRedisWatcherEnvVars(t *testing.T) {
	t.Run("uses external address and SecretKeyRef when ExternalConnection is set", func(t *testing.T) {
		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "env-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						ExternalConnection: &v1alpha1.ExternalConnectionConfig{
							Address:           "valkey.prod.example.com:6380",
							PasswordSecretRef: "prod-secret/auth-token",
						},
					},
				},
			},
		}

		envs := buildRedisWatcherEnvVars(kv)
		assert.Len(t, envs, 2)

		// REDIS_ADDR should be the external address.
		assert.Equal(t, "REDIS_ADDR", envs[0].Name)
		assert.Equal(t, "valkey.prod.example.com:6380", envs[0].Value)

		// REDIS_PASSWORD should use SecretKeyRef.
		assert.Equal(t, "REDIS_PASSWORD", envs[1].Name)
		assert.NotNil(t, envs[1].ValueFrom)
		assert.NotNil(t, envs[1].ValueFrom.SecretKeyRef)
		assert.Equal(t, "prod-secret", envs[1].ValueFrom.SecretKeyRef.Name)
		assert.Equal(t, "auth-token", envs[1].ValueFrom.SecretKeyRef.Key)
	})

	t.Run("uses external address with empty password when no PasswordSecretRef", func(t *testing.T) {
		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "env-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						ExternalConnection: &v1alpha1.ExternalConnectionConfig{
							Address: "valkey.dev.example.com:6379",
						},
					},
				},
			},
		}

		envs := buildRedisWatcherEnvVars(kv)
		assert.Equal(t, "valkey.dev.example.com:6379", envs[0].Value)
		assert.Equal(t, "REDIS_PASSWORD", envs[1].Name)
		assert.Nil(t, envs[1].ValueFrom)
		assert.Equal(t, "", envs[1].Value)
	})

	t.Run("falls back to in-cluster Redis when no ExternalConnection", func(t *testing.T) {
		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fallback-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						Runtime: &v1alpha1.RuntimeSpec{Replicas: 1},
					},
				},
			},
		}

		envs := buildRedisWatcherEnvVars(kv)
		assert.Equal(t, "REDIS_ADDR", envs[0].Name)
		assert.Equal(t, "fallback-test-redis:6379", envs[0].Value)
		assert.Equal(t, "REDIS_PASSWORD", envs[1].Name)
		assert.Equal(t, "", envs[1].Value)
		assert.Nil(t, envs[1].ValueFrom)
	})

	t.Run("defaults secret key to password when no slash in ref", func(t *testing.T) {
		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default-key-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						ExternalConnection: &v1alpha1.ExternalConnectionConfig{
							Address:           "valkey.example.com:6379",
							PasswordSecretRef: "my-secret",
						},
					},
				},
			},
		}

		envs := buildRedisWatcherEnvVars(kv)
		assert.Equal(t, "my-secret", envs[1].ValueFrom.SecretKeyRef.Name)
		assert.Equal(t, "password", envs[1].ValueFrom.SecretKeyRef.Key)
	})
}

// --Test for validation-before-cleanup ordering --

func TestReconcileRedisService_ValidationBeforeCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	t.Run("invalid external config does not delete existing in-cluster Redis", func(t *testing.T) {
		// Pre-create in-cluster Redis resources.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "safe-test-redis",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "redis", Image: "redis:7"}},
			},
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "safe-test-redis",
				Namespace: "default",
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 6379, Protocol: corev1.ProtocolTCP}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, svc).Build()
		r := NewDistributedReconciler(c, constants.KVCacheBackendInfinistore)
		r.Backend = mockBackend{}

		// Set an invalid external connection (no port in address).
		kv := &v1alpha1.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "safe-test",
				Namespace: "default",
			},
			Spec: v1alpha1.KVCacheSpec{
				Metadata: &v1alpha1.MetadataSpec{
					Redis: &v1alpha1.MetadataConfig{
						ExternalConnection: &v1alpha1.ExternalConnectionConfig{
							Address: "valkey.example.com",
						},
					},
				},
			},
		}

		err := r.reconcileRedisService(context.Background(), kv)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must be in host:port format")

		// Verify in-cluster Redis resources were NOT deleted.
		err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "safe-test-redis"}, &corev1.Pod{})
		assert.NoError(t, err, "in-cluster Redis Pod should still exist after validation failure")

		err = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "safe-test-redis"}, &corev1.Service{})
		assert.NoError(t, err, "in-cluster Redis Service should still exist after validation failure")
	})
}

// -- Mock Backend for isolation --

type mockBackend struct {
	redis   *corev1.Pod
	watcher *corev1.Pod
	svc     *corev1.Service
	sts     *appsv1.StatefulSet
	sa      *corev1.ServiceAccount
	role    *rbacv1.Role
	rb      *rbacv1.RoleBinding
}

func (m mockBackend) Name() string {
	return "mock-backend"
}

func (m mockBackend) ValidateObject(*v1alpha1.KVCache) error {
	return nil
}

func (m mockBackend) BuildMetadataPod(*v1alpha1.KVCache) *corev1.Pod {
	return m.redis
}

func (m mockBackend) BuildMetadataService(*v1alpha1.KVCache) *corev1.Service {
	return m.svc
}

func (m mockBackend) BuildWatcherPodServiceAccount(*v1alpha1.KVCache) *corev1.ServiceAccount {
	return m.sa
}

func (m mockBackend) BuildWatcherPodRole(*v1alpha1.KVCache) *rbacv1.Role {
	return m.role
}

func (m mockBackend) BuildWatcherPodRoleBinding(*v1alpha1.KVCache) *rbacv1.RoleBinding {
	return m.rb
}

func (m mockBackend) BuildWatcherPod(*v1alpha1.KVCache) *corev1.Pod {
	return m.watcher
}

func (m mockBackend) BuildCacheStatefulSet(*v1alpha1.KVCache) *appsv1.StatefulSet {
	return m.sts
}

func (m mockBackend) BuildService(*v1alpha1.KVCache) *corev1.Service {
	return m.svc
}
