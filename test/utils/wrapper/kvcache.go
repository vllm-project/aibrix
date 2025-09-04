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

// Annotation adds an annotation to the KVCache.
func (w *KVCacheWrapper) Annotation(key, value string) *KVCacheWrapper {
	if w.cache.Annotations == nil {
		w.cache.Annotations = map[string]string{}
	}
	w.cache.Annotations[key] = value
	return w
}
