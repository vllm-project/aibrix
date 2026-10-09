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

package gateway

import (
	"sync/atomic"
	"testing"

	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestNewServerWithOptionsUsesInjectedCache(t *testing.T) {
	// The local manager captures injected for the strategies registered with
	// explicit cache constructors. Other production providers intentionally keep
	// their global-cache dependency and need it initialized before availability
	// (rather than mere registration) is validated.
	cache.InitForTest()
	injected := &MockCache{}

	server := NewServerWithOptions(nil, nil, nil, ServerOptions{Cache: injected})
	if server.cache != injected {
		t.Fatalf("NewServerWithOptions() cache = %p, want injected cache %p", server.cache, injected)
	}
	if server.routerManager == nil {
		t.Fatal("NewServerWithOptions() with Cache-only options must create a local router manager")
	}
	assertProductionStrategiesRegistered(t, server)
}

func TestNewServerInitializesGlobalRouterManager(t *testing.T) {
	cache.InitForTest()
	server := NewServer(nil, nil, nil)
	if server.routerManager == nil {
		t.Fatal("NewServer() must retain the initialized global router manager")
	}
	assertProductionStrategiesRegistered(t, server)
}

// countingRouter is a no-op router for counting constructor calls.
type countingRouter struct{}

func (countingRouter) Route(*types.RoutingContext, types.PodList) (string, error) { return "", nil }

// A server on the global router manager constructs each router once.
// Initializing the manager twice built two PD routers, each with its own
// token-load tracker, request-tracker registration and shared-ledger publisher.
func TestNewServerConstructsGlobalRoutersOnce(t *testing.T) {
	cache.InitForTest()
	var constructed atomic.Int32
	routing.Register(types.RoutingAlgorithm("test-construct-once"), func() (types.Router, error) {
		constructed.Add(1)
		return countingRouter{}, nil
	})

	NewServer(nil, nil, nil)

	if got := constructed.Load(); got != 1 {
		t.Fatalf("router constructed %d times, want 1", got)
	}
}

func assertProductionStrategiesRegistered(t *testing.T, server *Server) {
	t.Helper()
	strategies := []string{
		"pd", "slo-pack-load", "slo-least-load", "vtc-basic", "throughput",
		"session-affinity", "least-busy-time", "least-gpu-cache", "least-utilization",
		"least-request", "least-kv-cache", "least-latency", "load-balance", "prefix-cache",
	}
	for _, strategy := range strategies {
		if _, ok := server.routerManager.Validate(strategy); !ok {
			t.Fatalf("router manager does not validate production strategy %q", strategy)
		}
	}
}
