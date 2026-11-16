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

package utils

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func resetRedisEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"REDIS_HOST", "REDIS_PORT", "REDIS_PASSWORD", "REDIS_SENTINEL_ENABLED",
		"REDIS_SENTINEL_MASTER_NAME", "REDIS_SENTINEL_ADDRESSES",
		"REDIS_SENTINEL_USERNAME", "REDIS_SENTINEL_PASSWORD",
	} {
		t.Setenv(key, "")
	}
}

func TestRedisAddress(t *testing.T) {
	tests := []struct {
		name string
		host string
		port string
		want string
	}{
		{"hostname", "localhost", "6379", "localhost:6379"},
		{"IPv4", "127.0.0.1", "6379", "127.0.0.1:6379"},
		{"IPv6", "::1", "6379", "[::1]:6379"},
		{"bracketed IPv6", "[::1]", "6379", "[::1]:6379"},
		{"IPv6 documentation", "2001:db8::1", "6379", "[2001:db8::1]:6379"},
		{"bracketed IPv6 documentation", "[2001:db8::1]", "6379", "[2001:db8::1]:6379"},
		{"padded hostname", " localhost ", "6379", "localhost:6379"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, redisAddress(tt.host, tt.port))
			resetRedisEnv(t)
			t.Setenv("REDIS_HOST", tt.host)
			t.Setenv("REDIS_PORT", tt.port)
			client, err := newRedisClientFromEnv()
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			require.Equal(t, tt.want, client.Options().Addr)
		})
	}
}

func TestNewRedisClientWithContextTimeout(t *testing.T) {
	require.Nil(t, NewRedisClientWithContextTimeout(nil))
	for _, retries := range []int{0, -1, 5} {
		shared := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0", MaxRetries: retries})
		t.Cleanup(func() { _ = shared.Close() })
		before := *shared.Options()
		client := NewRedisClientWithContextTimeout(shared.Options())
		t.Cleanup(func() { _ = client.Close() })

		require.NotSame(t, shared, client)
		require.True(t, client.Options().ContextTimeoutEnabled)
		require.Equal(t, before.MaxRetries, client.Options().MaxRetries, "preserve the effective retry count")
		require.Equal(t, before.Addr, client.Options().Addr)
		require.Equal(t, before.ReadTimeout, client.Options().ReadTimeout)
		require.Equal(t, before.WriteTimeout, client.Options().WriteTimeout)
		require.Equal(t, before.MaxRetries, shared.Options().MaxRetries, "the source retry count is unchanged")
		require.Equal(t, before.ContextTimeoutEnabled, shared.Options().ContextTimeoutEnabled, "the source deadline behavior is unchanged")
	}
}

func TestNewRedisClientWithContextTimeout_PreservesTimeoutsAndBackoff(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options redis.Options
	}{
		{"defaults", redis.Options{}},
		{"custom", redis.Options{ReadTimeout: 2 * time.Second, WriteTimeout: 4 * time.Second, MinRetryBackoff: 12 * time.Millisecond, MaxRetryBackoff: 700 * time.Millisecond}},
		{"read disabled", redis.Options{ReadTimeout: -1}},
		{"read no deadline", redis.Options{ReadTimeout: -2}},
		{"write disabled", redis.Options{WriteTimeout: -1}},
		{"write no deadline", redis.Options{WriteTimeout: -2}},
		{"min backoff disabled", redis.Options{MinRetryBackoff: -1}},
		{"max backoff disabled", redis.Options{MaxRetryBackoff: -1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shared := redis.NewClient(&tt.options)
			t.Cleanup(func() { _ = shared.Close() })
			before := *shared.Options()
			client := NewRedisClientWithContextTimeout(shared.Options())
			t.Cleanup(func() { _ = client.Close() })

			require.True(t, client.Options().ContextTimeoutEnabled)
			require.Equal(t, before.ReadTimeout, client.Options().ReadTimeout)
			require.Equal(t, before.WriteTimeout, client.Options().WriteTimeout)
			require.Equal(t, before.MinRetryBackoff, client.Options().MinRetryBackoff)
			require.Equal(t, before.MaxRetryBackoff, client.Options().MaxRetryBackoff)
			require.Equal(t, before.ReadTimeout, shared.Options().ReadTimeout)
			require.Equal(t, before.WriteTimeout, shared.Options().WriteTimeout)
			require.Equal(t, before.MinRetryBackoff, shared.Options().MinRetryBackoff)
			require.Equal(t, before.MaxRetryBackoff, shared.Options().MaxRetryBackoff)
		})
	}
}

func TestRedisDirectClient(t *testing.T) {
	resetRedisEnv(t)
	s := miniredis.RunT(t)
	s.RequireAuth("data-password")
	t.Setenv("REDIS_HOST", s.Host())
	t.Setenv("REDIS_PORT", s.Port())
	t.Setenv("REDIS_PASSWORD", "data-password")
	// Sentinel fields must not affect the default direct connection.
	t.Setenv("REDIS_SENTINEL_ADDRESSES", "invalid")
	client := GetRedisClient()
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })
	require.Equal(t, s.Addr(), client.Options().Addr)
	require.NoError(t, client.Set(context.Background(), "direct", "ok", 0).Err())
	value, err := s.Get("direct")
	require.NoError(t, err)
	require.Equal(t, "ok", value)
}

func TestRedisDirectClientFactorySnapshot(t *testing.T) {
	resetRedisEnv(t)
	s := miniredis.RunT(t)
	s.RequireAuth("data-password")
	t.Setenv("REDIS_HOST", s.Host())
	t.Setenv("REDIS_PORT", s.Port())
	t.Setenv("REDIS_PASSWORD", "data-password")
	client, factory := GetRedisClientWithFactory()
	require.NotNil(t, client)
	require.NotNil(t, factory)
	t.Cleanup(func() { _ = client.Close() })
	t.Setenv("REDIS_HOST", "invalid")
	t.Setenv("REDIS_PASSWORD", "wrong-password")
	derived := factory(&RedisClientSettings{MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = derived.Close() })
	require.Equal(t, s.Addr(), derived.Options().Addr)
	require.Equal(t, "data-password", derived.Options().Password)
	require.Equal(t, 0, derived.Options().MaxRetries)
	require.True(t, derived.Options().ContextTimeoutEnabled)
	require.Equal(t, 3, client.Options().MaxRetries)
	require.False(t, client.Options().ContextTimeoutEnabled)
	require.NoError(t, client.Close())
	require.NoError(t, derived.Ping(context.Background()).Err())
}

func TestRedisSentinelConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		enabled   string
		master    string
		addresses string
	}{
		{"invalid enabled", "invalid", "mymaster", "localhost:26379"},
		{"missing master", "true", "", "localhost:26379"},
		{"missing addresses", "true", "mymaster", ""},
		{"missing port", "true", "mymaster", "localhost"},
		{"missing host", "true", "mymaster", ":26379"},
		{"invalid port", "true", "mymaster", "localhost:abc"},
		{"zero port", "true", "mymaster", "localhost:0"},
		{"large port", "true", "mymaster", "localhost:65536"},
		{"empty list entry", "true", "mymaster", "localhost:26379,"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetRedisEnv(t)
			t.Setenv("REDIS_SENTINEL_ENABLED", tt.enabled)
			t.Setenv("REDIS_SENTINEL_MASTER_NAME", tt.master)
			t.Setenv("REDIS_SENTINEL_ADDRESSES", tt.addresses)
			client, err := newRedisClientFromEnv()
			require.Error(t, err)
			require.Nil(t, client)
		})
	}
}

func TestRedisSentinelDiscoveryAndReconnect(t *testing.T) {
	resetRedisEnv(t)
	oldMaster := miniredis.RunT(t)
	newMaster := miniredis.RunT(t)
	oldMaster.RequireAuth("data-password")
	newMaster.RequireAuth("data-password")
	sentinel := miniredis.RunT(t)
	sentinel.RequireAuth("sentinel-password")

	var masterAddress atomic.Value
	masterAddress.Store(oldMaster.Addr())
	require.NoError(t, sentinel.Server().Register("SENTINEL", func(c *server.Peer, _ string, args []string) {
		if len(args) != 2 || args[1] != "mymaster" {
			c.WriteError("ERR unknown master")
			return
		}
		switch strings.ToLower(args[0]) {
		case "get-master-addr-by-name":
			host, port, _ := net.SplitHostPort(masterAddress.Load().(string))
			c.WriteStrings([]string{host, port})
		case "sentinels":
			c.WriteLen(0)
		default:
			c.WriteError("ERR unsupported Sentinel command")
		}
	}))
	t.Setenv("REDIS_SENTINEL_ENABLED", "true")
	t.Setenv("REDIS_SENTINEL_MASTER_NAME", " mymaster ")
	t.Setenv("REDIS_SENTINEL_ADDRESSES", " 127.0.0.1:1, "+sentinel.Addr()+" ")
	t.Setenv("REDIS_SENTINEL_PASSWORD", "sentinel-password")
	t.Setenv("REDIS_PASSWORD", "data-password")
	// A stale direct address must be ignored in Sentinel mode.
	t.Setenv("REDIS_HOST", "invalid")
	client := GetRedisClient()
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Set(context.Background(), "before", "ok", 0).Err())
	value, err := oldMaster.Get("before")
	require.NoError(t, err)
	require.Equal(t, "ok", value)

	// Simulate Sentinel announcing a new master after the previous master fails.
	masterAddress.Store(newMaster.Addr())
	oldMaster.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.Eventually(t, func() bool {
		return client.Set(ctx, "after", "ok", 0).Err() == nil
	}, 3*time.Second, 10*time.Millisecond)
	value, err = newMaster.Get("after")
	require.NoError(t, err)
	require.Equal(t, "ok", value)
}
