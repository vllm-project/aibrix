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
	"strconv"
	"strings"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	defaultRDMAPort         = 18512
	defaultAdminPort        = 9100
	defaultBlockSizeInBytes = 4096
	defaultBlockCount       = 1048576
	defaultTotalSlots       = 4096
	defaultVirtualNodeCount = 100
)

type ClusterParams struct {
	RdmaPort         int
	AdminPort        int
	BlockSizeInBytes int
	BlockCount       int
	TotalSlots       int
	VirtualNodeCount int
}

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
	params := getKVCacheParams(kvCache.GetAnnotations())

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
			Name: "WATCH_KVCACHE_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name:  "WATCH_KVCACHE_CLUSTER",
			Value: kvCache.Name,
		},
		{
			Name:  "AIBRIX_KVCACHE_RDMA_PORT",
			Value: strconv.Itoa(params.RdmaPort),
		},
		{
			Name:  "AIBRIX_KVCACHE_TOTAL_SLOTS",
			Value: strconv.Itoa(params.TotalSlots),
		},
		{
			Name:  "AIBRIX_KVCACHE_VIRTUAL_NODE_COUNT",
			Value: strconv.Itoa(params.VirtualNodeCount),
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
	params := getKVCacheParams(kvCache.GetAnnotations())

	metadataEnvVars := []corev1.EnvVar{
		{Name: "AIBRIX_KVCACHE_UID", Value: string(kvCache.UID)},
		{Name: "AIBRIX_KVCACHE_NAME", Value: kvCache.Name},
		{Name: "AIBRIX_KVCACHE_SERVER_NAMESPACE", Value: kvCache.Namespace},
	}

	fieldRefEnvVars := []corev1.EnvVar{
		{Name: "MY_HOST_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "MY_NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "MY_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "MY_POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "MY_POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "MY_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
	}

	kvCacheServerEnvVars := []corev1.EnvVar{
		{Name: "AIBRIX_KVCACHE_RDMA_PORT", Value: strconv.Itoa(params.RdmaPort)},
		{Name: "AIBRIX_KVCACHE_ADMIN_PORT", Value: strconv.Itoa(params.AdminPort)},
		{Name: "AIBRIX_KVCACHE_BLOCK_SIZE_IN_BYTES", Value: strconv.Itoa(params.BlockSizeInBytes)},
		{Name: "AIBRIX_KVCACHE_BLOCK_COUNT", Value: strconv.Itoa(params.BlockCount)},
	}

	envs := append(metadataEnvVars, fieldRefEnvVars...)
	envs = append(envs, kvCacheServerEnvVars...)

	kvCacheServerArgs := []string{
		"-a", "$AIBRIX_KVCACHE_RDMA_IP",
		"-p", "$AIBRIX_KVCACHE_RDMA_PORT",
		"-v", "$AIBRIX_KVCACHE_BLOCK_SIZE_IN_BYTES",
		"-b", "$AIBRIX_KVCACHE_BLOCK_COUNT",
		"--acl", "any",
	}
	kvCacheServerArgsStr := strings.Join(kvCacheServerArgs, " ")
	//privileged := true

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
					HostNetwork: true,
					HostIPC:     true,
					Containers: []corev1.Container{
						{
							Name:  "kvcache-server",
							Image: kvCache.Spec.Cache.Image,
							//Ports: []corev1.ContainerPort{
							//	{Name: "service", ContainerPort: 9600, Protocol: corev1.ProtocolTCP},
							//},
							Command: []string{
								"/bin/bash",
								"-c",
								// Support two modes:
								// option 1: read from host addr
								// option 2: read from annotations, allocated RDMA network card (TODO://)
								fmt.Sprintf(strings.TrimSpace(`
									AIBRIX_KVCACHE_RDMA_IP=$(ip addr show dev eth1 | grep 'inet ' | awk '{print $2}' | awk -F/ '{print $1}')
									echo "Binding to RDMA IP: $AIBRIX_KVCACHE_RDMA_IP"
									./hpkv-server %s`), kvCacheServerArgsStr),
							},
							Env: append(envs, kvCache.Spec.Cache.Env...),
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(kvCache.Spec.Cache.CPU),
									corev1.ResourceMemory: resource.MustParse(kvCache.Spec.Cache.Memory),
									// TODO: add rdma device resource
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(kvCache.Spec.Cache.CPU),
									corev1.ResourceMemory: resource.MustParse(kvCache.Spec.Cache.Memory),
								},
							},
							ImagePullPolicy: corev1.PullPolicy(kvCache.Spec.Cache.ImagePullPolicy),
							SecurityContext: &corev1.SecurityContext{
								// required to use RDMA
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{
										"IPC_LOCK",
									},
								},
								// if IPC_LOCK doesn't work, then we can consider privileged
								// Privileged: &privileged,
							},
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

func getKVCacheParams(annotations map[string]string) *ClusterParams {
	return &ClusterParams{
		RdmaPort:         getPortAnnotation(annotations, KVCacheAnnotationRDMAPort, defaultRDMAPort),
		AdminPort:        getPortAnnotation(annotations, KVCacheAnnotationAdminPort, defaultAdminPort),
		BlockSizeInBytes: getPositiveIntAnnotation(annotations, KVCacheAnnotationBlockSize, defaultBlockSizeInBytes),
		BlockCount:       getPositiveIntAnnotation(annotations, KVCacheAnnotationBlockCount, defaultBlockCount),
		TotalSlots:       getPositiveIntAnnotation(annotations, KVCacheAnnotationTotalSlots, defaultTotalSlots),
		VirtualNodeCount: getPositiveIntAnnotation(annotations, KVCacheAnnotationVirtualNodeCount, defaultVirtualNodeCount),
	}
}

func getPortAnnotation(annotations map[string]string, key string, defaultValue int) int {
	if val, ok := annotations[key]; ok {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 && parsed <= 65535 {
			return parsed
		}
		klog.Warningf("Invalid port for annotation %s: %s, using default %d", key, val, defaultValue)
	}
	return defaultValue
}

func getPositiveIntAnnotation(annotations map[string]string, key string, defaultValue int) int {
	if val, ok := annotations[key]; ok {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			return parsed
		}
		klog.Warningf("Invalid integer for annotation %s: %s, using default %d", key, val, defaultValue)
	}
	return defaultValue
}
