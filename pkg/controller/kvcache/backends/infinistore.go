package backends

import (
	"fmt"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strings"
)

type InfiniStoreBackend struct{}

func (b InfiniStoreBackend) BuildMetadataWorkload(cache *orchestrationv1alpha1.KVCache) *appsv1.Deployment {
	//TODO implement me
	panic("implement me")
}

func (b InfiniStoreBackend) BuildMetadataService(cache *orchestrationv1alpha1.KVCache) *corev1.Service {
	//TODO implement me
	panic("implement me")
}

func (InfiniStoreBackend) Name() string { return "infinistore" }

func (InfiniStoreBackend) ValidateObject(kv *orchestrationv1alpha1.KVCache) error {
	// Always accept, since only Redis used
	return nil
}

func (InfiniStoreBackend) BuildWatcherPod(kv *orchestrationv1alpha1.KVCache) *corev1.Pod {
	return buildKVCacheWatcherPodForInfiniStore(kv)
}

func (InfiniStoreBackend) BuildCacheStatefulSet(kv *orchestrationv1alpha1.KVCache) *appsv1.StatefulSet {
	return buildCacheStatefulSetForInfiniStore(kv)
}

func (InfiniStoreBackend) BuildService(kv *orchestrationv1alpha1.KVCache) *corev1.Service {
	return buildHeadlessServiceForInfiniStore(kv)
}

func buildKVCacheWatcherPodForInfiniStore(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
	params := getKVCacheParams(kvCache.GetAnnotations())
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
			Value: "12345",
		},
		{
			Name:  "AIBRIX_KVCACHE_ADMIN_PORT",
			Value: "8888",
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
					// TODO: add commands to distinguish infinistore and hpkv
					Args: []string{
						"--kv-cache-Backend", KVCacheBackendInfinistore,
					},
					// You can also add volumeMounts, env vars, etc. if needed.
					Env:             envs,
					ImagePullPolicy: corev1.PullAlways,
				},
			},
			ServiceAccountName: "kvcache-watcher-sa",
		},
	}

	return pod
}

func buildCacheStatefulSetForInfiniStore(kvCache *orchestrationv1alpha1.KVCache) *appsv1.StatefulSet {
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
		{Name: "AIBRIX_KVCACHE_RDMA_PORT", Value: "12345"},
		{Name: "AIBRIX_KVCACHE_ADMIN_PORT", Value: "8888"},
	}

	envs := append(metadataEnvVars, fieldRefEnvVars...)
	envs = append(envs, kvCacheServerEnvVars...)

	kvCacheServerArgs := []string{
		"--service-port", "$AIBRIX_KVCACHE_RDMA_IP",
		"--link-type", "Ethernet",
		"--manage-port", "$AIBRIX_KVCACHE_ADMIN_PORT",
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
							//Ports: []corev1.ContainerPort{
							//	{Name: "service", ContainerPort: 9600, Protocol: corev1.ProtocolTCP},
							//},
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
	port := int32(12345)
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
				{Name: "service", Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP},
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
