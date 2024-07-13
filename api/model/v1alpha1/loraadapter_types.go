/*
Copyright 2024.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// LoraAdapterSpec defines the desired state of LoraAdapter
type LoraAdapterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Foo is an example field of LoraAdapter. Edit loraadapter_types.go to remove/update
	Foo string `json:"foo,omitempty"`
}

// LoraAdapterStatus defines the observed state of LoraAdapter
type LoraAdapterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// LoraAdapter is the Schema for the loraadapters API
type LoraAdapter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LoraAdapterSpec   `json:"spec,omitempty"`
	Status LoraAdapterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// LoraAdapterList contains a list of LoraAdapter
type LoraAdapterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LoraAdapter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LoraAdapter{}, &LoraAdapterList{})
}
