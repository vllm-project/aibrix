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
	"errors"
	"fmt"
	"strconv"
	"strings"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	KVCacheAnnotationLinkType     = "infinistore.kvcache.orchestration.aibrix.ai/link-type"
	KVCacheAnnotationHintGidIndex = "infinistore.kvcache.orchestration.aibrix.ai/hint-gid-index"
)

const (
	defaultInfinistoreRDMAPort         = 12345
	defaultInfinistoreAdminPort        = 8088
	defaultInfinistoreLinkType         = "Ethernet"
	defaultInfinistoreTotalSlots       = 4096
	defaultInfinistoreVirtualNodeCount = 100
	defaultInfinistoreHintGIDIndex     = 7
)

type InfiniStoreParams struct {
	RdmaPort          int
	AdminPort         int
	LinkType          string
	ContainerRegistry string
	TotalSlots        int
	VirtualNodeCount  int
	HintGIDIndex      int
}

type InfiniStoreBackend struct{}

func (b InfiniStoreBackend) BuildMetadataPod(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
	return buildRedisPod(kvCache)
}

func (b InfiniStoreBackend) BuildMetadataService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	return buildRedisService(kvCache)
}

func (InfiniStoreBackend) Name() string { return constants.KVCacheBackendInfinistore }

func (InfiniStoreBackend) ValidateObject(kvCache *orchestrationv1alpha1.KVCache) error {
	if kvCache.Spec.Metadata != nil && kvCache.Spec.Metadata.Etcd == nil && kvCache.Spec.Metadata.Redis == nil {
		return errors.New("either etcd or redis configuration is required")
	}
	return nil
}

func (InfiniStoreBackend) BuildWatcherPod(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
	return buildKVCacheWatcherPodForInfiniStore(kvCache)
}

func (InfiniStoreBackend) BuildCacheStatefulSet(kvCache *orchestrationv1alpha1.KVCache) *appsv1.StatefulSet {
	return buildCacheStatefulSetForInfiniStore(kvCache)
}

func (InfiniStoreBackend) BuildService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	return buildHeadlessServiceForInfiniStore(kvCache)
}

func buildKVCacheWatcherPodForInfiniStore(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
	params := getInfiniStoreParams(kvCache.GetAnnotations())
	kvCacheWatcherPodImage := "aibrix/kvcache-watcher:nightly"
	if params.ContainerRegistry != "" {
		kvCacheWatcherPodImage = fmt.Sprintf("%s/%s", params.ContainerRegistry, kvCacheWatcherPodImage)
	}

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
			Name: "AIBRIX_KVCACHE_WATCH_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name:  "AIBRIX_KVCACHE_WATCH_CLUSTER",
			Value: kvCache.Name,
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-kvcache-watcher-pod", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleKVWatcher,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "kvcache-watcher",
					Image: kvCacheWatcherPodImage,
					Command: []string{
						"/kvcache-watcher",
					},
					Args: []string{
						"--kvcache-backend", constants.KVCacheBackendInfinistore,
						"--kvcache-server-rdma-port", strconv.Itoa(params.RdmaPort),
						"--kvcache-server-admin-port", strconv.Itoa(params.AdminPort),
						"--consistent-hashing-total-slots", strconv.Itoa(params.TotalSlots),
						"--consistent-hashing-virtual-node-count", strconv.Itoa(params.VirtualNodeCount),
					},
					// You can also add volumeMounts, env vars, etc. if needed.
					Env:             envs,
					ImagePullPolicy: corev1.PullAlways,
				},
			},
			// TODO: refactor the permission management here.
			ServiceAccountName: "kvcache-watcher-sa",
		},
	}

	return pod
}

func buildCacheStatefulSetForInfiniStore(kvCache *orchestrationv1alpha1.KVCache) *appsv1.StatefulSet {
	params := getInfiniStoreParams(kvCache.GetAnnotations())
	metadataEnvVars := []corev1.EnvVar{
		{Name: "AIBRIX_KVCACHE_UID", Value: string(kvCache.UID)},
		{Name: "AIBRIX_KVCACHE_NAME", Value: kvCache.Name},
		{Name: "AIBRIX_KVCACHE_NAMESPACE", Value: kvCache.Namespace},
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
		{Name: "AIBRIX_KVCACHE_BACKEND", Value: constants.KVCacheBackendInfinistore},
		{Name: "AIBRIX_KVCACHE_RDMA_PORT", Value: strconv.Itoa(params.RdmaPort)},
		{Name: "AIBRIX_KVCACHE_ADMIN_PORT", Value: strconv.Itoa(params.AdminPort)},
	}

	envs := append(fieldRefEnvVars, metadataEnvVars...)
	envs = append(envs, kvCacheServerEnvVars...)

	kvCacheServerArgs := []string{
		"--service-port", "$AIBRIX_KVCACHE_RDMA_IP",
		"--manage-port", "$AIBRIX_KVCACHE_ADMIN_PORT",
		"--link-type", "Ethernet",
		// this is volcano engine specific. subject to change to more flexible way in future.
		"--hint-gid-index", strconv.Itoa(params.HintGIDIndex),
	}
	kvCacheServerArgsStr := strings.Join(kvCacheServerArgs, " ")
	privileged := true

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kvCache.Name,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &kvCache.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					constants.KVCacheLabelKeyIdentifier: kvCache.Name,
					constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.KVCacheLabelKeyIdentifier: kvCache.Name,
						constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
					},
					// TODO: if there's rdma enabled, then we should attach resources.
					// use an annotation to control it? enable RDMA
					Annotations: map[string]string{
						"k8s.volcengine.com/pod-networks": `
[
  {
    "cniConf": {
      "name": "rdma"
    }
  }
]
`,
					},
				},
				Spec: corev1.PodSpec{
					//HostNetwork: true, // CNI doesn't need hostNetwork:true. in that case, RDMA ip won't be injected.
					HostIPC: true,
					Containers: []corev1.Container{
						{
							Name:  "kvcache-server",
							Image: kvCache.Spec.Cache.Image,
							Ports: []corev1.ContainerPort{
								{Name: "service", ContainerPort: int32(params.RdmaPort), Protocol: corev1.ProtocolTCP},
								{Name: "manage", ContainerPort: int32(params.AdminPort), Protocol: corev1.ProtocolTCP},
							},
							Command: []string{
								"/bin/bash",
								"-c",
								"infinistore",
								kvCacheServerArgsStr,
							},
							Env: append(envs, kvCache.Spec.Cache.Env...),
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(kvCache.Spec.Cache.CPU),
									corev1.ResourceMemory: resource.MustParse(kvCache.Spec.Cache.Memory),
									// TODO: this should read from KVCache api spec.
									corev1.ResourceName("vke.volcengine.com/rdma"): resource.MustParse("1"),
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
								Privileged: &privileged,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "shared-mem",
									MountPath: "/dev/shm",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "shared-mem",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									Medium: corev1.StorageMediumMemory,
								},
							},
						},
					},
				},
			},
		},
	}
	return ss
}

func buildHeadlessServiceForInfiniStore(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	params := getInfiniStoreParams(kvCache.GetAnnotations())
	rdmaPort := int32(params.RdmaPort)
	managePort := int32(params.AdminPort)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-headless-service", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "service", Port: rdmaPort, TargetPort: intstr.FromInt32(rdmaPort), Protocol: corev1.ProtocolTCP},
				{Name: "manage", Port: managePort, TargetPort: intstr.FromInt32(managePort), Protocol: corev1.ProtocolTCP},
			},
			Selector: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
			},
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
		},
	}
	return service
}

func getInfiniStoreParams(annotations map[string]string) *InfiniStoreParams {
	return &InfiniStoreParams{
		LinkType:          utils.GetStringAnnotationOrDefault(annotations, KVCacheAnnotationLinkType, defaultInfinistoreLinkType),
		HintGIDIndex:      utils.GetPositiveIntAnnotationOrDefault(annotations, KVCacheAnnotationHintGidIndex, defaultInfinistoreHintGIDIndex),
		ContainerRegistry: utils.GetStringAnnotationOrDefault(annotations, constants.KVCacheAnnotationContainerRegistry, ""),
		// doesn't support specify the annotations yet
		RdmaPort:         defaultInfinistoreRDMAPort,
		AdminPort:        defaultInfinistoreAdminPort,
		TotalSlots:       defaultInfinistoreTotalSlots,
		VirtualNodeCount: defaultInfinistoreVirtualNodeCount,
	}
}
