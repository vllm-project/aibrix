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
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/asyncjob"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestAsyncJobRegistryShutdownClosesOnlyOwnedPool(t *testing.T) {
	mr := miniredis.RunT(t)
	shared := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = shared.Close() })
	s := &Server{redisClient: shared, cache: &MockCache{}}
	registry := s.asyncJobRegistry()
	owned := s.asyncRedisClient
	require.NotNil(t, owned)
	require.NoError(t, owned.Ping(context.Background()).Err())
	s.Shutdown()
	s.Shutdown()
	require.ErrorIs(t, owned.Ping(context.Background()).Err(), redis.ErrClosed)
	require.NoError(t, shared.Ping(context.Background()).Err())
	require.Same(t, registry, s.asyncJobRegistry())

	uninitialized := &Server{redisClient: shared, cache: &MockCache{}}
	uninitialized.Shutdown()
	memory, _ := asyncjob.NewAsyncJobRegistryForClients(nil, nil)
	require.IsType(t, memory, uninitialized.asyncJobRegistry())
	require.Nil(t, uninitialized.asyncRedisClient, "no pool should open after shutdown")

	injected, injectedPool := asyncjob.NewAsyncJobRegistryForClients(utils.NewDirectRedisClients(shared), &MockCache{})
	t.Cleanup(func() { _ = injectedPool.Close() })
	s = &Server{asyncJobs: injected, cache: &MockCache{}}
	s.Shutdown()
	require.Same(t, injected, s.asyncJobRegistry())
	require.NoError(t, injectedPool.Ping(context.Background()).Err(), "injected registry is borrowed")
	require.NoError(t, shared.Ping(context.Background()).Err())

	concurrent := &Server{redisClient: shared, cache: &MockCache{}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		concurrent.asyncJobRegistry()
	}()
	go func() {
		defer wg.Done()
		concurrent.Shutdown()
	}()
	wg.Wait()
	if concurrent.asyncRedisClient != nil {
		require.ErrorIs(t, concurrent.asyncRedisClient.Ping(context.Background()).Err(), redis.ErrClosed)
	}
	require.NoError(t, shared.Ping(context.Background()).Err())
}

func TestGatewayRedisClientsSentinelSwitch(t *testing.T) {
	ctx := context.Background()
	oldMaster, newMaster, sentinel := miniredis.RunT(t), miniredis.RunT(t), miniredis.RunT(t)
	oldMaster.RequireAuth("data-password")
	newMaster.RequireAuth("data-password")
	sentinel.RequireAuth("sentinel-password")
	var address atomic.Value
	address.Store(oldMaster.Addr())
	require.NoError(t, sentinel.Server().Register("SENTINEL", func(c *server.Peer, _ string, args []string) {
		if len(args) != 2 || args[1] != "mymaster" {
			c.WriteError("ERR unknown master")
			return
		}
		switch strings.ToLower(args[0]) {
		case "get-master-addr-by-name":
			host, port, _ := net.SplitHostPort(address.Load().(string))
			c.WriteStrings([]string{host, port})
		case "sentinels":
			c.WriteLen(0)
		default:
			c.WriteError("ERR unsupported Sentinel command")
		}
	}))
	for key, value := range map[string]string{
		"REDIS_SENTINEL_ENABLED":     "true",
		"REDIS_SENTINEL_MASTER_NAME": "mymaster",
		"REDIS_SENTINEL_ADDRESSES":   sentinel.Addr(),
		"REDIS_PASSWORD":             "data-password",
		"REDIS_SENTINEL_USERNAME":    "",
		"REDIS_SENTINEL_PASSWORD":    "sentinel-password",
	} {
		t.Setenv(key, value)
	}
	clients := utils.GetRedisClients()
	shared := clients.Shared()
	require.NotNil(t, shared)
	t.Cleanup(func() { _ = shared.Close() })
	t.Setenv("REDIS_SENTINEL_ENABLED", "false")
	t.Setenv("REDIS_PASSWORD", "wrong-password")
	cache.InitForTest()
	gateway := NewServerWithRedisClients(clients, nil, nil, ServerOptions{
		Cache:               &MockCache{},
		DisableRateLimiting: true,
	})
	t.Cleanup(gateway.Shutdown)
	sessionRouter, err := gateway.routerManager.Lookup(routing.RouterSessionAffinity)
	require.NoError(t, err)
	probe := types.NewRoutingContext(ctx, "sentinel-wiring", "model", "", "", "")
	probe.ReqHeaders = map[string]string{constants.HeaderSessionKey: "probe"}
	_, _, err = sessionRouter.(types.PodScorer).ScoreAll(probe, &utils.PodArray{})
	require.NoError(t, err)
	var newMasterSessionReads atomic.Int32
	newMaster.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if strings.EqualFold(cmd, "GET") && len(args) == 1 && strings.HasPrefix(args[0], "aibrix:gateway:session_affinity:") {
			newMasterSessionReads.Add(1)
		}
		return false
	})
	require.NoError(t, oldMaster.Set("marker", "old"))
	require.NoError(t, newMaster.Set("marker", "new"))
	require.Equal(t, "old", shared.Get(ctx, "marker").Val())
	require.Equal(t, "old", gateway.asyncRedisClient.Get(ctx, "marker").Val())
	sentinelClient := redis.NewClient(&redis.Options{Addr: sentinel.Addr(), Password: "sentinel-password"})
	t.Cleanup(func() { _ = sentinelClient.Close() })
	require.Eventually(t, func() bool {
		counts, err := sentinelClient.PubSubNumSub(ctx, "+switch-master").Result()
		return err == nil && counts["+switch-master"] == 3
	}, time.Second, 10*time.Millisecond)
	oldHost, oldPort, _ := net.SplitHostPort(oldMaster.Addr())
	newHost, newPort, _ := net.SplitHostPort(newMaster.Addr())
	address.Store(newMaster.Addr())
	require.Positive(t, sentinel.Publish("+switch-master",
		fmt.Sprintf("mymaster %s %s %s %s", oldHost, oldPort, newHost, newPort)))
	require.Eventually(t, func() bool {
		_, _, probeErr := sessionRouter.(types.PodScorer).ScoreAll(probe, &utils.PodArray{})
		return probeErr == nil && newMasterSessionReads.Load() > 0 &&
			shared.Get(ctx, "marker").Val() == "new" &&
			gateway.asyncRedisClient.Get(ctx, "marker").Val() == "new"
	}, 2*time.Second, 10*time.Millisecond)
}
