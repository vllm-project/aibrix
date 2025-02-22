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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceConfig holds all service configuration about KvCache public facing service
type ServiceConfig struct {
	// Type defines the type of service (e.g., ClusterIP, NodePort, LoadBalancer).
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:="ClusterIP"
	Type corev1.ServiceType `json:"type,omitempty"`

	// service port
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:=9600
	Port int32 `json:"port,omitempty"`

	// NodePort specifies the port on each node on which this service is exposed when using NodePort type.
	// +kubebuilder:validation:Optional
	NodePort *int32 `json:"nodePort,omitempty"`
}

// MetadataConfig holds the configuration about the kv cache metadata service
type MetadataConfig struct {
	Redis RedisConfig `json:"redis,omitempty"`
	Etcd  EtcdConfig  `json:"etcd,omitempty"`
}

// RedisConfig provides the configuration fields for deploying Redis.
type RedisConfig struct {
	Image     string                      `json:"image"`
	Replicas  int32                       `json:"replicas"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	Storage   MetadataStorage             `json:"storage"`
}

// EtcdConfig provides the configuration fields for deploying etcd.
type EtcdConfig struct {
	Image string `json:"image"`
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:=1
	Replicas  int32                       `json:"replicas"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	Storage   MetadataStorage             `json:"storage"`
}

// MetadataStorage configures the persistent storage used by the metadata service.
type MetadataStorage struct {
	Size string `json:"size"`
}

type CacheSpec struct {
	// Replicas is the number of kvcache pods to deploy
	// +kubebuilder:default:=3
	// +optional
	Replicas *int `json:"replicas,omitempty"`

	// represent the kvcache's image
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:="aibrix/kvcache:20241120"
	Image string `json:"image,omitempty"`

	// the policy about pulling image
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:="IfNotPresent"
	ImagePullPolicy string `json:"imagePullPolicy,omitempty"`

	// shared memory size for kvcach
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:=""
	SharedMemorySize string `json:"sharedMemorySize,omitempty"`

	// kvcache environment configuration
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:={}
	Env []corev1.EnvVar `json:"env,omitempty"`

	// the memory resources of kvcache container
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:="2Gi"
	Memory string `json:"memory,omitempty"`

	// the cpu resources of kvcache container
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:="1"
	CPU string `json:"cpu,omitempty"`
}

// KVCacheSpec defines the desired state of KVCache
type KVCacheSpec struct {
	// Replicas is the number of kv cache pods to deploy
	// +kubebuilder:validation:Required
	// +kubebuilder:default:=1
	Replicas int32 `json:"replicas,omitempty"`

	// EtcdReplicas describe the etcd replicas
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:=1
	EtcdReplicas int32 `json:"etcdReplicas,omitempty"`

	// Metadata configuration for kv cache service
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:={etcd: {image: "", replicas: 1, storage: {size: "10Gi"}}}
	Metadata MetadataConfig `json:"metadata,omitempty"`

	// kvcache dataplane container configuration
	// +kubebuilder:validation:Optional
	//nolint: lll
	// +kubebuilder:default:={image: "aibrix/kvcache:20241120", imagePullPolicy: "IfNotPresent"}
	Cache CacheSpec `json:"cacheSpec,omitempty"`

	// cache's service
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:={type: "ClusterIP", port: 9600}
	Service ServiceConfig `json:"service,omitempty"`
}

// KVCacheStatus defines the observed state of KVCache
type KVCacheStatus struct {
	// Total replicas of current running kv cache instances.
	ReadyReplicas int32 `json:"current,omitempty"`
	// Represents the kv cache deployment's current state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// KVCache is the Schema for the kvcaches API
type KVCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KVCacheSpec   `json:"spec,omitempty"`
	Status KVCacheStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KVCacheList contains a list of KVCache
type KVCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KVCache `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KVCache{}, &KVCacheList{})
}
