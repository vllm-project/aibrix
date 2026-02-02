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

// Package discovery provides service discovery for standalone/Docker mode.
//
// Currently only FileProvider is implemented. Kubernetes mode still uses
// the informer logic in pkg/cache/informers.go directly.
//
// TODO: Add KubernetesProvider to unify both modes under the Provider interface.
// The main blocker is that informers.go also watches ModelAdapters (for LoRA).
package discovery

import (
	"k8s.io/client-go/tools/cache"
)

// Provider defines the interface for service discovery backends.
type Provider interface {
	// Load returns all k8s like Resources, can be *v1.Pod, *modelv1alpha1.ModelAdapter
	Load() ([]any, error)

	// Process Resource add/update/delete events. kind is the resource type, "Pod", "ModelAdapter", etc
	AddEventHandler(kind string,
		handler cache.ResourceEventHandlerFuncs,
		stopCh <-chan struct{}) error

	// Type returns a string identifier for the provider type.
	Type() string
}
