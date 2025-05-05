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

package backends

import (
	"fmt"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// buildRedisPod constructs a Kubernetes Pod object for running a Redis instance
// as part of the KVCache deployment. This Redis Pod is typically used as metadata
// service for kvcache backends like HPKV or InfiniStore to store their membership details.
// TODO: pod needs to be changed to statefulsets or deployment in future.
func buildRedisPod(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
	image := kvCache.Spec.Metadata.Redis.Runtime.Image
	redisPodName := fmt.Sprintf("%s-redis", kvCache.Name)
	envs := []corev1.EnvVar{}
	if len(kvCache.Spec.Metadata.Redis.Runtime.Env) != 0 {
		envs = append(envs, kvCache.Spec.Metadata.Redis.Runtime.Env...)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisPodName,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "redis",
					Image:           image,
					ImagePullPolicy: corev1.PullPolicy(kvCache.Spec.Metadata.Redis.Runtime.ImagePullPolicy),
					Ports: []corev1.ContainerPort{
						{
							Name:          "redis",
							ContainerPort: 6379,
							Protocol:      corev1.ProtocolTCP,
						},
					},
					Command: []string{
						"redis-server",
					},
					// You can also add volumeMounts, env vars, etc. if needed.
					Resources: kvCache.Spec.Metadata.Redis.Runtime.Resources,
					Env:       envs,
				},
			},
		},
	}

	return pod
}

// buildRedisService constructs a Kubernetes Service object for the Redis instance
// associated with the given KVCache custom resource. This Service is typically
// used to expose the Redis Pod(s) to other components in the system.
func buildRedisService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	redisServiceName := fmt.Sprintf("%s-redis", kvCache.Name)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisServiceName,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "service",
					Port:       6379,
					TargetPort: intstr.FromInt32(6379),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	return svc
}
