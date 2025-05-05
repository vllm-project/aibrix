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
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestBuildKVCacheWatcherPod_HP(t *testing.T) {
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-cache",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.KVCacheSpec{
			Metadata: &v1alpha1.MetadataSpec{
				Redis: &v1alpha1.MetadataConfig{
					Runtime: &v1alpha1.RuntimeSpec{
						Replicas: 1,
						Image:    "redis:7.4.1",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
			Cache: v1alpha1.RuntimeSpec{
				Replicas: 3,
				Image:    "aibrix/infinistore:20250502",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:                             resource.MustParse("8"),
						corev1.ResourceMemory:                          resource.MustParse("50Gi"),
						corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:                             resource.MustParse("8"),
						corev1.ResourceMemory:                          resource.MustParse("50Gi"),
						corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
					},
				},
			},
			Watcher: &v1alpha1.RuntimeSpec{
				Replicas: 1,
				Image:    "aibrix/kvcache-watcher:nightly",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256"),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256"),
					},
				},
			},
			Service: v1alpha1.ServiceSpec{
				Type: corev1.ClusterIPNone,
				Ports: []corev1.ServicePort{
					{
						Name: "rdma",
						Port: 12345,
					},
				},
			},
		},
	}

	pod := buildKVCacheWatcherPod(kv)

	assert.Equal(t, "test-cache-kvcache-watcher-pod", pod.Name)
	assert.Equal(t, "default", pod.Namespace)

	envMap := map[string]string{}
	for _, env := range pod.Spec.Containers[0].Env {
		envMap[env.Name] = env.Value
	}

	assert.Equal(t, "test-cache-redis:6379", envMap["REDIS_ADDR"])
	assert.Equal(t, "test-cache", envMap["AIBRIX_KVCACHE_WATCH_CLUSTER"])
}

func TestBuildCacheStatefulSet_HP(t *testing.T) {
	replicas := int32(1)
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-hpkv",
			Namespace: "default",
			UID:       "1234-uid",
			Annotations: map[string]string{
				KVCacheAnnotationBlockCount:       "2048",
				KVCacheAnnotationBlockSize:        "8192",
				KVCacheAnnotationRDMAPort:         "18500",
				KVCacheAnnotationAdminPort:        "9101",
				KVCacheAnnotationTotalSlots:       "512",
				KVCacheAnnotationVirtualNodeCount: "64",
			},
		},
		Spec: v1alpha1.KVCacheSpec{
			Cache: v1alpha1.RuntimeSpec{
				Replicas:        replicas,
				Image:           "aibrix/hpkv-server:nightly",
				ImagePullPolicy: "Always",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:                             resource.MustParse("4"),
						corev1.ResourceMemory:                          resource.MustParse("8Gi"),
						corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:                             resource.MustParse("4"),
						corev1.ResourceMemory:                          resource.MustParse("8Gi"),
						corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
					},
				},
			},
		},
	}

	ss := buildCacheStatefulSet(kv)
	assert.Equal(t, "test-hpkv", ss.Name)
	assert.Equal(t, &replicas, ss.Spec.Replicas)

	// Annotations
	annotations := ss.Spec.Template.Annotations
	assert.Contains(t, annotations["k8s.volcengine.com/pod-networks"], `"name": "rdma"`)

	// Container basic
	container := ss.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "aibrix/hpkv-server:nightly", container.Image)
	assert.Equal(t, "Always", string(container.ImagePullPolicy))
	assert.Equal(t, "kvcache-server", container.Name)
	assert.False(t, *container.SecurityContext.Privileged)

	// Command check
	assert.Len(t, container.Command, 3)
	assert.Contains(t, container.Command[2], "./hpkv-server")

	// Resource checks
	assert.Equal(t, "4", container.Resources.Limits.Cpu().String())
	assert.Equal(t, "8Gi", container.Resources.Limits.Memory().String())
	rdmaQty := container.Resources.Limits[corev1.ResourceName("vke.volcengine.com/rdma")]
	assert.Equal(t, "1", rdmaQty.String())

	// Env value assertions
	env := map[string]string{}
	for _, e := range container.Env {
		if e.Value != "" {
			env[e.Name] = e.Value
		}
	}
	assert.Equal(t, "test-hpkv", env["AIBRIX_KVCACHE_NAME"])
	assert.Equal(t, "default", env["AIBRIX_KVCACHE_NAMESPACE"])
	assert.Equal(t, "8192", env["AIBRIX_KVCACHE_BLOCK_SIZE_IN_BYTES"])
	assert.Equal(t, "2048", env["AIBRIX_KVCACHE_BLOCK_COUNT"])
	assert.Equal(t, "18500", env["AIBRIX_KVCACHE_RDMA_PORT"])
	assert.Equal(t, "9101", env["AIBRIX_KVCACHE_ADMIN_PORT"])
	assert.Equal(t, constants.KVCacheBackendHPKV, env["AIBRIX_KVCACHE_BACKEND"])

	// VolumeMount check
	assert.Equal(t, "/dev/shm", container.VolumeMounts[0].MountPath)
	assert.Equal(t, "shared-mem", container.VolumeMounts[0].Name)

	// Volumes
	vol := ss.Spec.Template.Spec.Volumes[0]
	assert.Equal(t, corev1.StorageMediumMemory, vol.VolumeSource.EmptyDir.Medium)
	assert.Equal(t, "shared-mem", vol.Name)
}

func TestBuildHeadlessService_HP(t *testing.T) {
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
		},
		Spec: v1alpha1.KVCacheSpec{
			Service: v1alpha1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP,
				Ports: []corev1.ServicePort{
					{
						Name:       "rdma",
						Port:       defaultHPKVRDMAPort,
						TargetPort: intstr.FromInt(defaultHPKVRDMAPort),
					},
				},
			},
		},
	}

	svc := buildHeadlessService(kv)
	assert.Equal(t, "test-svc-headless-service", svc.Name)
	assert.Equal(t, corev1.ClusterIPNone, svc.Spec.ClusterIP)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)

	port := svc.Spec.Ports[0]
	assert.Equal(t, int32(defaultHPKVRDMAPort), port.Port)
	assert.Equal(t, intstr.FromInt(defaultHPKVRDMAPort), port.TargetPort)
}
