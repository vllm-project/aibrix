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

// Package discovery provides service discovery backends for the gateway.
//
// Available providers:
//   - StaticProvider: loads endpoints from a YAML config file (for standalone/Docker mode)
//   - KubernetesProvider: watches Pods and ModelAdapters via K8s informers
//
// TODO: Add ConsulProvider, EtcdProvider.
package discovery

// EventType represents the type of a watch event.
type EventType int

const (
	EventAdd EventType = iota
	EventUpdate
	EventDelete
)

// WatchEvent represents a change detected by a discovery provider.
type WatchEvent struct {
	// Type is the kind of change: add, update, or delete.
	Type EventType
	// Object is the current state of the resource (for add/update) or
	// the last known state (for delete). Currently *v1.Pod.
	Object any
	// OldObject is the previous state, only set for EventUpdate. Nil otherwise.
	OldObject any
}

// EventHandler is a callback invoked by a provider when a resource changes.
type EventHandler func(event WatchEvent)

// ModelConfig carries the config profiles discovery supplies for a model as a
// whole, as the JSON the model.aibrix.ai/config pod annotation holds. The
// static provider delivers one as EventAdd for each model whose entry sets
// config. The Kubernetes provider does not deliver it: there the pod
// annotation stays the source.
type ModelConfig struct {
	// ModelName is the model the config applies to.
	ModelName string
	// Config is the model config JSON.
	Config string
}

// Provider defines the interface for service discovery backends.
//
// All initial state and ongoing changes are delivered through Watch() via
// the EventHandler callback.
type Provider interface {
	// Watch registers a handler for resource change events and starts watching.
	// The provider calls handler for each change (add/update/delete).
	//
	// Watch should return once the provider has reached a consistent ready state
	// (e.g., initial sync complete, config loaded). After return, dynamic providers
	// continue delivering ongoing changes via the handler asynchronously.
	//
	// Static providers deliver initial state and return (no ongoing changes).
	// K8s provider lets informers deliver events directly via the handler from
	// the start, then does a post-sync reconcile before returning.
	// Consul/etcd providers may deliver initial state, then start a background
	// watch/poll loop.
	Watch(handler EventHandler, stopCh <-chan struct{}) error

	// Type returns a string identifier for the provider type (e.g., "static", "consul").
	Type() string
}
