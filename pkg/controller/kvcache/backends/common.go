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
	"strings"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

// buildServiceAccount creates a new ServiceAccount for Distributed kv cache solution.
func buildServiceAccount(kvCache *orchestrationv1alpha1.KVCache) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kvCache.Name,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
			},
		},
	}

	return sa
}

// buildRole creates a new Role for a KVCache resource.
func buildRole(kvCache *orchestrationv1alpha1.KVCache) *rbacv1.Role {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kvCache.Name,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/exec"},
				Verbs:     []string{"create"},
			},
		},
	}

	return role
}

// buildRoleBinding creates rolebinding for a kvCache object
func buildRoleBinding(kvCache *orchestrationv1alpha1.KVCache) *rbacv1.RoleBinding {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kvCache.Name,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      kvCache.Name,
				Namespace: kvCache.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     kvCache.Name,
		},
	}

	return rb
}

// parseSecretRef parses a secret reference in "secretName/key" format.
// If no key is specified, defaults to "password".
func parseSecretRef(ref string) (name, key string) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], "password"
}

// buildRedisWatcherEnvVars constructs the REDIS_ADDR and REDIS_PASSWORD env vars
// for a watcher pod. If an ExternalConnection is configured, it uses the external
// address and wires the password via SecretKeyRef. Otherwise falls back to the
// in-cluster Redis service at <name>-redis:6379 with an empty password.
//
// Limitation: ReconcilePodObject only reconciles the container image, not env vars.
// If an operator later changes which secret/key passwordSecretRef points at or changes
// the external address, the running watcher pod retains the old env vars. The secret
// *value* is re-read by kubelet on pod restart (SecretKeyRef is a reference, not a
// snapshot), but a change to the secret *name* or the address field requires deleting
// and recreating the watcher pod for the new config to take effect.
func buildRedisWatcherEnvVars(kvCache *orchestrationv1alpha1.KVCache) []corev1.EnvVar {
	redisAddr := fmt.Sprintf("%s-redis:%d", kvCache.Name, 6379)
	var redisPasswordEnv corev1.EnvVar

	if kvCache.Spec.Metadata != nil && kvCache.Spec.Metadata.Redis != nil &&
		kvCache.Spec.Metadata.Redis.ExternalConnection != nil &&
		kvCache.Spec.Metadata.Redis.ExternalConnection.Address != "" {
		extConn := kvCache.Spec.Metadata.Redis.ExternalConnection
		redisAddr = extConn.Address

		// Wire PasswordSecretRef into the pod env via SecretKeyRef.
		if extConn.PasswordSecretRef != "" {
			secretName, secretKey := parseSecretRef(extConn.PasswordSecretRef)
			redisPasswordEnv = corev1.EnvVar{
				Name: "REDIS_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  secretKey,
					},
				},
			}
		} else {
			redisPasswordEnv = corev1.EnvVar{Name: "REDIS_PASSWORD", Value: ""}
		}
	} else {
		redisPasswordEnv = corev1.EnvVar{Name: "REDIS_PASSWORD", Value: ""}
	}

	return []corev1.EnvVar{
		{
			Name:  "REDIS_ADDR",
			Value: redisAddr,
		},
		redisPasswordEnv,
	}
}
