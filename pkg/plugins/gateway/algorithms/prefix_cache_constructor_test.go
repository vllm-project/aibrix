/*
Copyright 2026 The Aibrix Team.

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

package routingalgorithms

import (
	"testing"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
)

func TestNewPrefixCacheRouterWithCache(t *testing.T) {
	router, err := NewPrefixCacheRouterWithCache(cache.NewForTest())
	if err != nil || router == nil {
		t.Fatalf("NewPrefixCacheRouterWithCache() = (%v, %v)", router, err)
	}
}

func TestNewPrefixCacheRouterWithOptionsUsesInjectedIndexer(t *testing.T) {
	indexer := prefixcacheindexer.NewPrefixHashTable()
	router, err := NewPrefixCacheRouterWithOptions(cache.NewForTest(), indexer)
	if err != nil {
		t.Fatalf("NewPrefixCacheRouterWithOptions() error = %v", err)
	}
	prefixRouter, ok := router.(prefixCacheRouter)
	if !ok {
		t.Fatalf("NewPrefixCacheRouterWithOptions() returned %T, want prefixCacheRouter", router)
	}
	if prefixRouter.prefixCacheIndexer != indexer {
		t.Fatal("NewPrefixCacheRouterWithOptions() did not retain injected indexer")
	}
}
