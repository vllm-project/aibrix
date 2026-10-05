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

package wrapper

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// ModelAdapterWrapper wraps core ModelAdapter types to provide a fluent API for test construction.
type ModelAdapterWrapper struct {
	adapter modelapi.ModelAdapter
}

// MakeModelAdapter creates a new ModelAdapterWrapper with the given name.
func MakeModelAdapter(name string) *ModelAdapterWrapper {
	return &ModelAdapterWrapper{
		adapter: modelapi.ModelAdapter{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		},
	}
}

// Obj returns the pointer to the underlying ModelAdapter object.
func (w *ModelAdapterWrapper) Obj() *modelapi.ModelAdapter {
	return &w.adapter
}

// Name sets the name of the ModelAdapter.
func (w *ModelAdapterWrapper) Name(name string) *ModelAdapterWrapper {
	w.adapter.Name = name
	return w
}

// Namespace sets the namespace of the ModelAdapter.
func (w *ModelAdapterWrapper) Namespace(ns string) *ModelAdapterWrapper {
	w.adapter.Namespace = ns
	return w
}

// ArtifactURL sets the ModelAdapter ArtifactURL.
func (w *ModelAdapterWrapper) ArtifactURL(url string) *ModelAdapterWrapper {
	w.adapter.Spec.ArtifactURL = url
	return w
}

// PodSelector sets the ModelAdapter PodSelector.
func (w *ModelAdapterWrapper) PodSelector(selector *metav1.LabelSelector) *ModelAdapterWrapper {
	w.adapter.Spec.PodSelector = selector
	return w
}

// Replicas sets the ModelAdapter Replicas.
func (w *ModelAdapterWrapper) Replicas(replicas *int32) *ModelAdapterWrapper {
	w.adapter.Spec.Replicas = replicas
	return w
}
