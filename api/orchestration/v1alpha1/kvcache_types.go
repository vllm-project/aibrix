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

// ServiceSpec holds all service configuration about KvCache public facing service
type ServiceSpec struct {
	// Type defines the type of service (e.g., ClusterIP, NodePort, LoadBalancer).
	// +kubebuilder:default:="ClusterIP"
	Type corev1.ServiceType `json:"type,omitempty"`

	// Ports defines the list of exposed ports
	// +kubebuilder:validation:MinItems=1
	Ports []corev1.ServicePort `json:"ports"`
}

// ExternalConnectionConfig holds config for connecting to external metadata service
type ExternalConnectionConfig struct {
	// Address to connect to (host:port)
	Address string `json:"address,omitempty"`

	// Optional secret reference for password or credential
	PasswordSecretRef string `json:"passwordSecretRef,omitempty"`
}

// MetadataConfig provides the configuration fields for deploying Redis.
type MetadataConfig struct {
	ExternalConnection *ExternalConnectionConfig `json:"externalConnection,omitempty"`
	Runtime            *RuntimeSpec              `json:"runtime,omitempty"`
}

// MetadataSpec holds deployment or external connection config for metadata services
type MetadataSpec struct {
	Redis *MetadataConfig `json:"redis,omitempty"`
	Etcd  *MetadataConfig `json:"etcd,omitempty"`
}

type RuntimeSpec struct {
	// Replicas is the number of kvcache pods to deploy
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:=1
	Replicas int32 `json:"replicas,omitempty"`

	// represent the kvcache's image
	// +kubebuilder:validation:Required
	Image string `json:"image,omitempty"`

	// the policy about pulling image
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:="IfNotPresent"
	ImagePullPolicy string `json:"imagePullPolicy,omitempty"`

	// kvcache environment configuration
	// +kubebuilder:validation:Optional
	// +kubebuilder:default:={}
	Env []corev1.EnvVar `json:"env,omitempty"`

	// the resources of kvcache container
	// +kubebuilder:validation:Optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// KVCacheSpec defines the desired state of KVCache
type KVCacheSpec struct {
	// +kubebuilder:default:=distributed
	Mode string `json:"mode,omitempty"` // centralized | distributed

	// Metadata configuration for kv cache service
	// +kubebuilder:validation:Optional
	Metadata *MetadataSpec `json:"metadata,omitempty"`

	// kvcache dataplane container configuration
	// +kubebuilder:validation:Optional
	//nolint: lll
	// +kubebuilder:default:={image: "aibrix/kvcache:20241120", imagePullPolicy: "IfNotPresent"}
	Cache RuntimeSpec `json:"cache,omitempty"`

	// kvcache watcher pod for member registration
	// +kubebuilder:validation:Optional
	Watcher *RuntimeSpec `json:"watcher,omitempty"`

	// cache's service
	// +kubebuilder:validation:Optional
	Service ServiceSpec `json:"service,omitempty"`
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
