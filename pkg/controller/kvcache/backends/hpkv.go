package backends

import (
	"errors"
	"fmt"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"strconv"
	"strings"
)

const (
	KVCacheAnnotationRDMAPort         = "hpkv.kvcache.orchestration.aibrix.ai/rdma-port"
	KVCacheAnnotationAdminPort        = "hpkv.kvcache.orchestration.aibrix.ai/admin-port"
	KVCacheAnnotationBlockSize        = "hpkv.kvcache.orchestration.aibrix.ai/block-size-bytes"
	KVCacheAnnotationBlockCount       = "hpkv.kvcache.orchestration.aibrix.ai/block-count"
	KVCacheAnnotationTotalSlots       = "hpkv.kvcache.orchestration.aibrix.ai/total-slots"
	KVCacheAnnotationVirtualNodeCount = "hpkv.kvcache.orchestration.aibrix.ai/virtual-node-count"
)

const (
	defaultRDMAPort         = 18512
	defaultAdminPort        = 9100
	defaultBlockSizeInBytes = 4096
	defaultBlockCount       = 1048576
	defaultTotalSlots       = 4096
	defaultVirtualNodeCount = 100
)

type HpKVClusterParams struct {
	RdmaPort          int
	AdminPort         int
	BlockSizeInBytes  int
	BlockCount        int
	TotalSlots        int
	VirtualNodeCount  int
	ContainerRegistry string
}

type HpKVBackend struct{}

func (b HpKVBackend) BuildMetadataWorkload(cache *orchestrationv1alpha1.KVCache) *appsv1.Deployment {
	//TODO implement me
	panic("implement me")
}

func (b HpKVBackend) BuildMetadataService(cache *orchestrationv1alpha1.KVCache) *corev1.Service {
	//TODO implement me
	panic("implement me")
}

func (HpKVBackend) Name() string { return "hpkv" }

func (HpKVBackend) ValidateObject(kv *orchestrationv1alpha1.KVCache) error {
	if kv.Spec.Metadata != nil && kv.Spec.Metadata.Etcd == nil && kv.Spec.Metadata.Redis == nil {
		return errors.New("either etcd or redis configuration is required")
	}
	return nil
}

func (HpKVBackend) BuildWatcherPod(kv *orchestrationv1alpha1.KVCache) *corev1.Pod {
	// you can reuse your existing buildKVCacheWatcherPod() logic
	return buildKVCacheWatcherPod(kv)
}

func (HpKVBackend) BuildCacheStatefulSet(kv *orchestrationv1alpha1.KVCache) *appsv1.StatefulSet {
	return buildCacheStatefulSet(kv)
}

func (HpKVBackend) BuildService(kv *orchestrationv1alpha1.KVCache) *corev1.Service {
	return buildHeadlessService(kv)
}

func buildKVCacheWatcherPod(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
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
						"--kv-cache-Backend", KVCacheBackendHPKV,
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
									corev1.ResourceCPU:                             resource.MustParse(kvCache.Spec.Cache.CPU),
									corev1.ResourceMemory:                          resource.MustParse(kvCache.Spec.Cache.Memory),
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

func buildHeadlessService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	// TODO: update
	port := int32(9600)
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

func getKVCacheParams(annotations map[string]string) *HpKVClusterParams {
	return &HpKVClusterParams{
		RdmaPort:          getPortAnnotation(annotations, KVCacheAnnotationRDMAPort, defaultRDMAPort),
		AdminPort:         getPortAnnotation(annotations, KVCacheAnnotationAdminPort, defaultAdminPort),
		BlockSizeInBytes:  getPositiveIntAnnotation(annotations, KVCacheAnnotationBlockSize, defaultBlockSizeInBytes),
		BlockCount:        getPositiveIntAnnotation(annotations, KVCacheAnnotationBlockCount, defaultBlockCount),
		TotalSlots:        getPositiveIntAnnotation(annotations, KVCacheAnnotationTotalSlots, defaultTotalSlots),
		VirtualNodeCount:  getPositiveIntAnnotation(annotations, KVCacheAnnotationVirtualNodeCount, defaultVirtualNodeCount),
		ContainerRegistry: getStringAnnotation(annotations, constants.KVCacheAnnotationContainerRegistry, ""),
	}
}

func getStringAnnotation(annotations map[string]string, key string, defaultValue string) string {
	if val, ok := annotations[key]; ok && val != "" {
		return val
	}
	klog.Infof("Annotation %s not set or empty, using default: %s", key, defaultValue)
	return defaultValue
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
