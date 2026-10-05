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
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildKVCacheWatcherPodForInfiniStore(t *testing.T) {
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-kvcache",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.KVCacheSpec{
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
		},
	}

	pod := buildKVCacheWatcherPodForInfiniStore(kv)

	assert.Equal(t, "my-kvcache-kvcache-watcher-pod", pod.Name)
	assert.Equal(t, "default", pod.Namespace)

	envs := pod.Spec.Containers[0].Env
	assert.Contains(t, envs, corev1.EnvVar{Name: "REDIS_ADDR", Value: "my-kvcache-redis:6379"})
}

func TestBuildCacheStatefulSetForInfiniStore(t *testing.T) {
	replicas := int32(2)
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cache",
			Namespace: "default",
			UID:       "1234-uid",
		},
		Spec: v1alpha1.KVCacheSpec{
			Cache: v1alpha1.RuntimeSpec{
				Replicas:        replicas,
				Image:           "aibrix/infinistore:nightly",
				ImagePullPolicy: "Always",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:                             resource.MustParse("2"),
						corev1.ResourceMemory:                          resource.MustParse("4Gi"),
						corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:                             resource.MustParse("2"),
						corev1.ResourceMemory:                          resource.MustParse("4Gi"),
						corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
					},
				},
			},
		},
	}

	sts := buildCacheStatefulSetForInfiniStore(kv)

	assert.Equal(t, "test-cache", sts.Name)
	assert.Equal(t, &replicas, sts.Spec.Replicas)

	// annotation validation
	annotations := sts.Spec.Template.Annotations
	assert.Contains(t, annotations, "k8s.volcengine.com/pod-networks")
	assert.Contains(t, annotations["k8s.volcengine.com/pod-networks"], `"cniConf"`)

	// container validation
	container := sts.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "aibrix/infinistore:nightly", container.Image)
	assert.Equal(t, "Always", string(container.ImagePullPolicy))
	assert.Equal(t, "kvcache-server", container.Name)
	assert.False(t, *container.SecurityContext.Privileged)
	assert.NotEmpty(t, container.Command)
	assert.NotEmpty(t, container.Env)

	// resource validation
	res := container.Resources
	assert.Equal(t, "2", res.Limits.Cpu().String())
	assert.Equal(t, "4Gi", res.Limits.Memory().String())

	rdmaKey := corev1.ResourceName("vke.volcengine.com/rdma")
	rdmaQuantity, exists := res.Limits[rdmaKey]
	assert.True(t, exists, "RDMA resource should exist in limits")
	assert.Equal(t, "1", rdmaQuantity.String())

	// env validation
	expectedEnvVars := map[string]string{
		"AIBRIX_KVCACHE_UID":        "1234-uid",
		"AIBRIX_KVCACHE_NAME":       "test-cache",
		"AIBRIX_KVCACHE_NAMESPACE":  "default",
		"AIBRIX_KVCACHE_BACKEND":    constants.KVCacheBackendInfinistore,
		"AIBRIX_KVCACHE_RDMA_PORT":  strconv.Itoa(defaultInfinistoreRDMAPort),
		"AIBRIX_KVCACHE_ADMIN_PORT": strconv.Itoa(defaultInfinistoreAdminPort),
	}

	envMap := map[string]string{}
	for _, env := range container.Env {
		if env.Value != "" {
			envMap[env.Name] = env.Value
		}
	}

	for k, v := range expectedEnvVars {
		assert.Equal(t, v, envMap[k], "env var %s should equal %s", k, v)
	}

	// field env validation
	fieldRefEnvPaths := map[string]string{
		"MY_HOST_NAME":     "status.podIP",
		"MY_NODE_NAME":     "spec.nodeName",
		"MY_POD_NAME":      "metadata.name",
		"MY_POD_NAMESPACE": "metadata.namespace",
		"MY_POD_IP":        "status.podIP",
		"MY_UID":           "metadata.uid",
	}

	for _, env := range container.Env {
		if fieldRef, ok := fieldRefEnvPaths[env.Name]; ok {
			assert.NotNil(t, env.ValueFrom)
			assert.Equal(t, fieldRef, env.ValueFrom.FieldRef.FieldPath, "FieldPath for %s should match", env.Name)
		}
	}
}

func TestBuildCacheStatefulSetForInfiniStoreWithCustomPodTemplateSpec(t *testing.T) {
	replicas := int32(2)
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cache",
			Namespace: "default",
			UID:       "1234-uid",
		},
		Spec: v1alpha1.KVCacheSpec{
			Cache: v1alpha1.RuntimeSpec{
				Replicas: replicas,
				// even we specify them here, but they will not take effect
				Image:           "aibrix/infinistore:nightly",
				ImagePullPolicy: "Always",
				// podTemplateSpec will be used instead
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
						Labels: map[string]string{
							"test-label": "test-value",
						},
						Annotations: map[string]string{
							"test-annotation": "test-value",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "test-image",
								Env: []corev1.EnvVar{
									{
										Name:  "TEST_ENV",
										Value: "test-value",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	sts := buildCacheStatefulSetForInfiniStore(kv)

	assert.Equal(t, kv.Name, sts.Name)
	assert.Equal(t, &replicas, sts.Spec.Replicas)

	// label validation
	labels := sts.Spec.Template.Labels
	assert.Contains(t, labels, "test-label")
	assert.Equal(t, labels["test-label"], "test-value")
	// it has to contains the override labels
	assert.Contains(t, labels, constants.KVCacheLabelKeyIdentifier)
	assert.Equal(t, labels[constants.KVCacheLabelKeyIdentifier], kv.Name)
	assert.Contains(t, labels, constants.KVCacheLabelKeyRole)
	assert.Equal(t, labels[constants.KVCacheLabelKeyRole], constants.KVCacheLabelValueRoleCache)

	// annotation validation
	annotations := sts.Spec.Template.Annotations
	assert.Contains(t, annotations, "test-annotation")
	assert.Equal(t, annotations["test-annotation"], "test-value")

	// container validation
	container := sts.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "test-image", container.Image)
	assert.Equal(t, "test-container", container.Name)
	assert.Empty(t, container.Command)
	assert.NotEmpty(t, container.Env)

	// resource validation
	assert.Empty(t, container.Resources)

	// env validation
	expectedEnvVars := map[string]string{
		"TEST_ENV": "test-value",
	}

	envMap := map[string]string{}
	for _, env := range container.Env {
		if env.Value != "" {
			envMap[env.Name] = env.Value
		}
	}

	for k, v := range expectedEnvVars {
		assert.Equal(t, v, envMap[k], "env var %s should equal %s", k, v)
	}
}

func TestBuildHeadlessServiceForInfiniStore(t *testing.T) {
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cache",
			Namespace: "default",
		},
		Spec: v1alpha1.KVCacheSpec{
			Service: v1alpha1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP,
				Ports: []corev1.ServicePort{
					{
						Name: "rdma",
						Port: 12345,
					},
				},
			},
		},
	}

	svc := buildHeadlessServiceForInfiniStore(kv)

	assert.Equal(t, "my-cache-headless-service", svc.Name)
	assert.Equal(t, corev1.ClusterIPNone, svc.Spec.ClusterIP)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, defaultInfinistoreRDMAPort, int(svc.Spec.Ports[0].Port))
}

func TestGetInfiniStoreParams(t *testing.T) {
	annotations := map[string]string{}

	params := getInfiniStoreParams(annotations)

	assert.Equal(t, defaultInfinistoreRDMAPort, params.RdmaPort)
	assert.Equal(t, defaultInfinistoreLinkType, params.LinkType)
}

func TestGetPreallocSizeFromResources(t *testing.T) {
	tests := []struct {
		name      string
		limits    string
		expectVal int
	}{
		{
			name:      "10Gi limit",
			limits:    "10Gi",
			expectVal: 9, // 10 * 0.9 = 9
		},
		{
			name:      "1Gi limit",
			limits:    "1Gi",
			expectVal: 1, // 1 * 0.9 = 0.9 => floor = 0 => return 1
		},
		{
			name:      "2Gi limit",
			limits:    "2Gi",
			expectVal: 1, // 2 * 0.9 = 1.8 => floor = 1
		},
		{
			name:      "512Mi limit",
			limits:    "512Mi",
			expectVal: 1, // ~0.5Gi * 0.9 = ~0.45 => floor = 0 => return 1
		},
		{
			name:      "4096Mi limit (4Gi)",
			limits:    "4096Mi",
			expectVal: 3, // 4 * 0.9 = 3.6 => floor = 3
		},
		{
			name:      "8192Mi limit (8Gi)",
			limits:    "8192Mi",
			expectVal: 7, // 8 * 0.9 = 7.2 => floor = 7
		},
		{
			name:      "empty limit uses default",
			limits:    "",
			expectVal: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := corev1.ResourceRequirements{}
			if tt.limits != "" {
				qty := resource.MustParse(tt.limits)
				res.Limits = corev1.ResourceList{
					corev1.ResourceMemory: qty,
				}
			}

			val := getPreallocSizeFromResources(res)
			if val != tt.expectVal {
				t.Errorf("expected %d, got %d", tt.expectVal, val)
			}
		})
	}
}
