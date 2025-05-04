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

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildKVCacheWatcherPodForInfiniStore(t *testing.T) {
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-kvcache",
			Namespace: "default",
			Annotations: map[string]string{
				constants.KVCacheAnnotationContainerRegistry: "ghcr.io/aaahhh",
			},
		},
	}

	pod := buildKVCacheWatcherPodForInfiniStore(kv)

	assert.Equal(t, "my-kvcache-kvcache-watcher-pod", pod.Name)
	assert.Equal(t, "default", pod.Namespace)
	assert.Equal(t, "ghcr.io/aaahhh/aibrix/kvcache-watcher:nightly", pod.Spec.Containers[0].Image)

	envs := pod.Spec.Containers[0].Env
	assert.Contains(t, envs, corev1.EnvVar{Name: "REDIS_ADDR", Value: "my-kvcache-redis:6379"})
	assert.Contains(t, envs, corev1.EnvVar{Name: "AIBRIX_KVCACHE_RDMA_PORT", Value: strconv.Itoa(defaultInfinistoreRDMAPort)})
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
			Replicas: replicas,
			Cache: v1alpha1.CacheSpec{
				Image:           "aibrix/infinistore:nightly",
				CPU:             "2",
				Memory:          "4Gi",
				ImagePullPolicy: "Always",
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
	assert.True(t, *container.SecurityContext.Privileged)
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

func TestBuildHeadlessServiceForInfiniStore(t *testing.T) {
	kv := &v1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cache",
			Namespace: "default",
		},
	}

	svc := buildHeadlessServiceForInfiniStore(kv)

	assert.Equal(t, "my-cache-headless-service", svc.Name)
	assert.Equal(t, corev1.ClusterIPNone, svc.Spec.ClusterIP)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, defaultInfinistoreRDMAPort, int(svc.Spec.Ports[0].Port))
}

func TestGetInfiniStoreParams(t *testing.T) {
	annotations := map[string]string{
		constants.KVCacheAnnotationContainerRegistry: "docker.io/mirror",
	}

	params := getInfiniStoreParams(annotations)

	assert.Equal(t, "docker.io/mirror", params.ContainerRegistry)
	assert.Equal(t, defaultInfinistoreRDMAPort, params.RdmaPort)
	assert.Equal(t, defaultInfinistoreLinkType, params.LinkType)
}
