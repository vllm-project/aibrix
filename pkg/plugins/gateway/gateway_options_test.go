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
	"reflect"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/ratelimiter"
)

func TestNewServerWithOptionsUsesInjectedCache(t *testing.T) {
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

func TestNewServerWithOptionsConfiguresRateLimitingPolicy(t *testing.T) {
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = redisClient.Close() })

	for _, tt := range []struct {
		name        string
		client      *redis.Client
		disable     bool
		wantNoop    bool
		wantEnabled bool
	}{
		{name: "redis present and enabled by default", client: redisClient, wantEnabled: true},
		{name: "redis present and disabled", client: redisClient, disable: true, wantNoop: true},
		{name: "redis absent and enabled by default", wantNoop: true, wantEnabled: true},
		{name: "redis absent and disabled", disable: true, wantNoop: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServerWithOptions(tt.client, nil, nil, ServerOptions{
				Cache:               &MockCache{},
				DisableRateLimiting: tt.disable,
			})
			t.Cleanup(server.Shutdown)

			if got := server.rateLimitingEnabled; got != tt.wantEnabled {
				t.Fatalf("rateLimitingEnabled = %t, want %t", got, tt.wantEnabled)
			}
			if server.redisClient != tt.client {
				t.Fatalf("redisClient = %p, want supplied client %p", server.redisClient, tt.client)
			}
			noopType := reflect.TypeOf(ratelimiter.NewNoopRateLimiter())
			userNoop := reflect.TypeOf(server.ratelimiter) == noopType
			modelNoop := reflect.TypeOf(server.modelRateLimiter) == noopType
			if userNoop != tt.wantNoop || modelNoop != tt.wantNoop {
				t.Fatalf("no-op limiter selection = (%t, %t), want (%t, %t)", userNoop, modelNoop, tt.wantNoop, tt.wantNoop)
			}
		})
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
