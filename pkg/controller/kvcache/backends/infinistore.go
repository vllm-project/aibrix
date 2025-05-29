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
	"math"
	"strconv"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	RdmaPort         int
	AdminPort        int
	LinkType         string
	TotalSlots       int
	VirtualNodeCount int
	HintGIDIndex     int
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

func (b InfiniStoreBackend) BuildWatcherPodServiceAccount(kvCache *orchestrationv1alpha1.KVCache) *corev1.ServiceAccount {
	return buildServiceAccount(kvCache)
}

func (b InfiniStoreBackend) BuildWatcherPodRole(kvCache *orchestrationv1alpha1.KVCache) *rbacv1.Role {
	return buildRole(kvCache)
}

func (b InfiniStoreBackend) BuildWatcherPodRoleBinding(kvCache *orchestrationv1alpha1.KVCache) *rbacv1.RoleBinding {
	return buildRoleBinding(kvCache)
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

	if len(kvCache.Spec.Watcher.Env) != 0 {
		envs = append(envs, kvCache.Spec.Watcher.Env...)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-kvcache-watcher-pod", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleKVWatcher,
			},
			Annotations: map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/port":   "8000",
				"prometheus.io/path":   "/metrics",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "kvcache-watcher",
					Image:           kvCache.Spec.Watcher.Image,
					ImagePullPolicy: corev1.PullPolicy(kvCache.Spec.Watcher.ImagePullPolicy),
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
					Env: envs,
					Ports: []corev1.ContainerPort{
						{
							Name:          "metrics",
							ContainerPort: int32(8000),
							Protocol:      corev1.ProtocolTCP,
						},
					},
					Resources: kvCache.Spec.Watcher.Resources,
				},
			},
			ServiceAccountName: kvCache.Name,
		},
	}

	return pod
}

func buildCacheStatefulSetForInfiniStore(kvCache *orchestrationv1alpha1.KVCache) *appsv1.StatefulSet {
	if kvCache.Spec.Cache.Template != nil {
		// It will override all the configurations and we should use user given spec to build objects
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
				Replicas: &kvCache.Spec.Cache.Replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						constants.KVCacheLabelKeyIdentifier: kvCache.Name,
						constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
					},
				},
				Template: *kvCache.Spec.Cache.Template,
			},
		}

		// override the selector labels
		if ss.Spec.Template.Labels == nil {
			ss.Spec.Template.Labels = map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
			}
		} else {
			ss.Spec.Template.Labels[constants.KVCacheLabelKeyIdentifier] = kvCache.Name
			ss.Spec.Template.Labels[constants.KVCacheLabelKeyRole] = constants.KVCacheLabelValueRoleCache
		}

		return ss
	}

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
	if len(kvCache.Spec.Cache.Env) == 0 {
		envs = append(envs, kvCache.Spec.Cache.Env...)
	}

	annotations := map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   strconv.Itoa(params.AdminPort),
		"prometheus.io/path":   "/metrics",
	}
	rdmaKey := corev1.ResourceName("vke.volcengine.com/rdma")
	if _, ok := kvCache.Spec.Cache.Resources.Limits[rdmaKey]; ok {
		annotations["k8s.volcengine.com/pod-networks"] = `
[
  {
    "cniConf": {
      "name": "rdma"
    }
  }
]
`
	}

	preallocSize := getPreallocSizeFromResources(kvCache.Spec.Cache.Resources)
	kvCacheServerArgs := []string{
		fmt.Sprintf("--service-port=%s", strconv.Itoa(params.RdmaPort)),
		fmt.Sprintf("--manage-port=%s", strconv.Itoa(params.AdminPort)),
		fmt.Sprintf("--link-type=%s", params.LinkType),
		// this is volcano engine specific. subject to change to more flexible way in future.
		fmt.Sprintf("--hint-gid-index=%s", strconv.Itoa(params.HintGIDIndex)),
		fmt.Sprintf("--prealloc-size=%s", strconv.Itoa(preallocSize)),
	}
	privileged := false

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
			Replicas: &kvCache.Spec.Cache.Replicas,
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
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					//HostNetwork: true, // CNI doesn't need hostNetwork:true. in that case, RDMA ip won't be injected.
					HostIPC: true,
					Containers: []corev1.Container{
						{
							Name:            "kvcache-server",
							Image:           kvCache.Spec.Cache.Image,
							ImagePullPolicy: corev1.PullPolicy(kvCache.Spec.Cache.ImagePullPolicy),
							Ports: []corev1.ContainerPort{
								{Name: "service", ContainerPort: int32(params.RdmaPort), Protocol: corev1.ProtocolTCP},
								{Name: "manage", ContainerPort: int32(params.AdminPort), Protocol: corev1.ProtocolTCP},
							},
							Command: []string{
								"infinistore",
							},
							Args:      kvCacheServerArgs,
							Env:       envs,
							Resources: kvCache.Spec.Cache.Resources,
							SecurityContext: &corev1.SecurityContext{
								// required to use RDMA
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{
										"IPC_LOCK",
										"SYS_RESOURCE", // infinistore only
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
	ports := kvCache.Spec.Service.Ports
	if len(ports) == 0 {
		ports = []corev1.ServicePort{
			{Name: "service", Port: rdmaPort, TargetPort: intstr.FromInt32(rdmaPort), Protocol: corev1.ProtocolTCP},
			{Name: "manage", Port: managePort, TargetPort: intstr.FromInt32(managePort), Protocol: corev1.ProtocolTCP},
		}
	}

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
			Ports: ports,
			Selector: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
			},
			Type:      kvCache.Spec.Service.Type,
			ClusterIP: corev1.ClusterIPNone,
		},
	}
	return service
}

func getInfiniStoreParams(annotations map[string]string) *InfiniStoreParams {
	return &InfiniStoreParams{
		LinkType:     utils.GetStringAnnotationOrDefault(annotations, KVCacheAnnotationLinkType, defaultInfinistoreLinkType),
		HintGIDIndex: utils.GetPositiveIntAnnotationOrDefault(annotations, KVCacheAnnotationHintGidIndex, defaultInfinistoreHintGIDIndex),
		// doesn't support specify the annotations yet
		RdmaPort:         defaultInfinistoreRDMAPort,
		AdminPort:        defaultInfinistoreAdminPort,
		TotalSlots:       defaultInfinistoreTotalSlots,
		VirtualNodeCount: defaultInfinistoreVirtualNodeCount,
	}
}

// getPreallocSizeFromResources infers the preallocated memory size (in GiB) from container memory limits.
// It multiplies the limit by 0.9 and floors the result. If the computed value is less than 1, it returns 1 GiB.
// If limits are not specified, it falls back to requests. If neither is specified, it defaults to 1 GiB.
func getPreallocSizeFromResources(resources corev1.ResourceRequirements) int {
	memQty, ok := resources.Limits[corev1.ResourceMemory]
	if !ok {
		// fallback to requests if limits not set
		memQty, ok = resources.Requests[corev1.ResourceMemory]
		if !ok {
			return 1 // default to 1 GiB if neither is set
		}
	}

	// Convert to GiB (1 GiB = 1024^3 bytes)
	memBytes := memQty.Value()
	memGiB := float64(memBytes) / float64(1<<30)

	// Multiply by 0.9 and round down
	prealloc := int(math.Floor(memGiB * 0.9))
	if prealloc < 1 {
		return 1
	}
	return prealloc
}
