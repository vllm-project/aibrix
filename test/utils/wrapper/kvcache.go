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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// KVCacheWrapper wraps core KVCache types to provide a fluent API for test construction.
type KVCacheWrapper struct {
	cache orchestrationapi.KVCache
}

// MakeKVCache creates a new KVCacheWrapper with the given name.
func MakeKVCache(name string) *KVCacheWrapper {
	return &KVCacheWrapper{
		cache: orchestrationapi.KVCache{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		},
	}
}

// Obj returns the pointer to the underlying KVCache object.
func (w *KVCacheWrapper) Obj() *orchestrationapi.KVCache {
	return &w.cache
}

// Name sets the name of the KVCache.
func (w *KVCacheWrapper) Name(name string) *KVCacheWrapper {
	w.cache.Name = name
	return w
}

// Namespace sets the namespace of the KVCache.
func (w *KVCacheWrapper) Namespace(ns string) *KVCacheWrapper {
	w.cache.Namespace = ns
	return w
}

// Mode sets the mode of the KVCache.
func (w *KVCacheWrapper) Mode(mode string) *KVCacheWrapper {
	w.cache.Spec.Mode = mode
	return w
}

// WithWatcher sets the watcher of the KVCache.
func (w *KVCacheWrapper) WithWatcher(watcher orchestrationapi.RuntimeSpec) *KVCacheWrapper {
	w.cache.Spec.Watcher = &watcher
	return w
}

// WithCache sets the cache of the KVCache.
func (w *KVCacheWrapper) WithCache(cache orchestrationapi.RuntimeSpec) *KVCacheWrapper {
	w.cache.Spec.Cache = cache
	return w
}

// Annotation adds an annotation to the KVCache.
func (w *KVCacheWrapper) Annotation(key, value string) *KVCacheWrapper {
	if w.cache.Annotations == nil {
		w.cache.Annotations = map[string]string{}
	}
	w.cache.Annotations[key] = value
	return w
}

// WithDefaultConfiguration sets up the KVCache with default configuration.
func (w *KVCacheWrapper) WithDefaultConfiguration() *KVCacheWrapper {
	w.cache.Spec = orchestrationapi.KVCacheSpec{
		Metadata: &orchestrationapi.MetadataSpec{},
		Service: orchestrationapi.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "service",
					Port:       12345,
					TargetPort: intstr.FromInt(12345),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "admin",
					Port:       8088,
					TargetPort: intstr.FromInt(8088),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
		Watcher: &orchestrationapi.RuntimeSpec{},
		Cache:   orchestrationapi.RuntimeSpec{},
	}
	return w
}
