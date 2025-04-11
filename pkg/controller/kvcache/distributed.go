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
	"fmt"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *KVCacheReconciler) reconcileDistributedMode(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) (ctrl.Result, error) {
	err := r.reconcileMetadataService(ctx, kvCache)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Handle Hpkv/infinistore kvCache Deployment
	sts := buildCacheStatefulSet(kvCache)
	if err := r.ReconcileStatefulsetObject(ctx, sts); err != nil {
		return ctrl.Result{}, err
	}

	// Handle Hpkv/infinistore Services
	service := buildHeadlessService(kvCache)
	if err := r.ReconcileServiceObject(ctx, service); err != nil {
		return ctrl.Result{}, err
	}

	kvcacheWatcherPod := buildKVCacheWatcherPod(kvCache)
	if err := r.ReconcilePodObject(ctx, kvcacheWatcherPod); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func buildKVCacheWatcherPod(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
	envs := []corev1.EnvVar{
		{
			Name:  "REDIS_ADDR",
			Value: fmt.Sprintf("%s-redis:%d", kvCache.Name, 6379),
		},
		{
			Name:  "REDIS_PASSWORD",
			Value: "",
		},
		{
			Name:  "REDIS_DATABASE",
			Value: "0",
		},
		{
			Name: "WATCH_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name:  "WATCH_KV_CLUSTER",
			Value: kvCache.Name,
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-kvcache-watcher-pod", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleKVWatcher,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "kvcache-watcher",
					Image: "aibrix/kvcache-watcher:nightly",
					Command: []string{
						"/kvcache-watcher",
					},
					// You can also add volumeMounts, env vars, etc. if needed.
					Env: envs,
				},
			},
			ServiceAccountName: "kvcache-watcher-sa",
		},
	}

	return pod
}

func (r *KVCacheReconciler) reconcileRedisService(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	// We only support etcd at this moment, redis will be supported later.
	replicas := int(kvCache.Spec.Metadata.Redis.Replicas)
	if replicas != 1 {
		klog.Warningf("replica %d > 1 is not supported at this moment, we will change to single replica", replicas)
	}

	pod := buildRedisPod(kvCache)
	if err := r.ReconcilePodObject(ctx, pod); err != nil {
		return err
	}

	// Create or update the etcd service for each pod
	etcdService := buildRedisService(kvCache)
	if err := r.ReconcileServiceObject(ctx, etcdService); err != nil {
		return err
	}

	return nil
}

func buildRedisService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	redisServiceName := fmt.Sprintf("%s-redis", kvCache.Name)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisServiceName,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleMetadata,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleMetadata,
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

func buildRedisPod(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
	image := kvCache.Spec.Cache.Image
	if kvCache.Spec.Metadata.Redis.Image != "" {
		image = kvCache.Spec.Metadata.Redis.Image
	}

	redisPodName := fmt.Sprintf("%s-redis", kvCache.Name)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisPodName,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleMetadata,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "redis",
					Image: image,
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
				},
			},
		},
	}

	return pod
}

func buildCacheStatefulSet(kvCache *orchestrationv1alpha1.KVCache) *appsv1.StatefulSet {
	envs := []corev1.EnvVar{
		{Name: "AIBRIX_KVCACHE_UID", Value: string(kvCache.ObjectMeta.UID)},
		{Name: "AIBRIX_KVCACHE_NAME", Value: kvCache.Name},
		{Name: "AIBRIX_KVCACHE_SERVER_NAMESPACE", Value: kvCache.Namespace},
		{Name: "MY_HOST_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "MY_NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "MY_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "MY_POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "MY_POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "MY_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
	}

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kvCache.Name,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       "cache",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &kvCache.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					KVCacheLabelKeyIdentifier: kvCache.Name,
					KVCacheLabelKeyRole:       KVCacheLabelValueRoleCache,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						KVCacheLabelKeyIdentifier: kvCache.Name,
						KVCacheLabelKeyRole:       KVCacheLabelValueRoleCache,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "kvcache",
							Image: kvCache.Spec.Cache.Image,
							Ports: []corev1.ContainerPort{
								{Name: "service", ContainerPort: 9600, Protocol: corev1.ProtocolTCP},
							},
							Command: []string{
								"/bin/bash",
								"-c",
								//fmt.Sprintf(`xxx %s `, kvCache.Name),
								"redis-server",
							},
							Env: append(envs, kvCache.Spec.Cache.Env...),
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(kvCache.Spec.Cache.CPU),
									corev1.ResourceMemory: resource.MustParse(kvCache.Spec.Cache.Memory),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(kvCache.Spec.Cache.CPU),
									corev1.ResourceMemory: resource.MustParse(kvCache.Spec.Cache.Memory),
								},
							},
							ImagePullPolicy: corev1.PullPolicy(kvCache.Spec.Cache.ImagePullPolicy),
						},
					},
				},
			},
		},
	}
	return ss
}

func buildHeadlessService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	// TODO: update
	port := int32(9600)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-headless-service", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "service", Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP},
			},
			Selector: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleCache,
			},
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
		},
	}
	return service
}
