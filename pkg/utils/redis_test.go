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
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
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
		{name: "hostname", host: "localhost", port: "6379", want: "localhost:6379"},
		{name: "IPv4", host: "127.0.0.1", port: "6379", want: "127.0.0.1:6379"},
		{name: "IPv6", host: "::1", port: "6379", want: "[::1]:6379"},
		{name: "bracketed IPv6", host: "[::1]", port: "6379", want: "[::1]:6379"},
		{name: "IPv6 documentation", host: "2001:db8::1", port: "6379", want: "[2001:db8::1]:6379"},
		{name: "bracketed IPv6 documentation", host: "[2001:db8::1]", port: "6379", want: "[2001:db8::1]:6379"},
		{name: "padded hostname", host: " localhost ", port: "6379", want: "localhost:6379"},
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

func TestCheckRedisConnectivityReleasesPool(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(fmt.Sprintf("stalled=%t", stalled), func(t *testing.T) {
			resetRedisEnv(t)
			mr := miniredis.RunT(t)
			t.Setenv("REDIS_HOST", mr.Host())
			t.Setenv("REDIS_PORT", mr.Port())
			disconnected := make(chan struct{})
			mr.Server().SetPreHook(func(peer *server.Peer, cmd string, _ ...string) bool {
				if strings.EqualFold(cmd, "PING") {
					peer.OnDisconnect(func() { close(disconnected) })
					return stalled
				}
				return false
			})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := CheckRedisConnectivity(ctx)
			if stalled {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			select {
			case <-disconnected:
			case <-time.After(time.Second):
				t.Fatal("connectivity check leaked its Redis connection")
			}
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

func TestRedisDirectClientsSnapshot(t *testing.T) {
	resetRedisEnv(t)
	s := miniredis.RunT(t)
	s.RequireAuth("data-password")
	t.Setenv("REDIS_HOST", s.Host())
	t.Setenv("REDIS_PORT", s.Port())
	t.Setenv("REDIS_PASSWORD", "data-password")
	clients := GetRedisClients()
	client := clients.Shared()
	require.NotNil(t, client)
	require.NotNil(t, clients)
	t.Cleanup(func() { _ = client.Close() })
	t.Setenv("REDIS_HOST", "invalid")
	t.Setenv("REDIS_PASSWORD", "wrong-password")
	derived := clients.Derive(&RedisClientSettings{ContextTimeoutEnabled: true})
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
		wantError string
	}{
		{"invalid enabled", "invalid", "mymaster", "localhost:26379", "REDIS_SENTINEL_ENABLED must be a boolean"},
		{"missing master", "true", "", "localhost:26379", "REDIS_SENTINEL_MASTER_NAME is required"},
		{"missing addresses", "true", "mymaster", "", "REDIS_SENTINEL_ADDRESSES is required"},
		{"whitespace addresses", "true", "mymaster", " \t ", "REDIS_SENTINEL_ADDRESSES is required"},
		{"missing port", "true", "mymaster", "localhost", "REDIS_SENTINEL_ADDRESSES must contain host:port addresses"},
		{"missing host", "true", "mymaster", ":26379", "REDIS_SENTINEL_ADDRESSES must contain host:port addresses"},
		{"invalid port", "true", "mymaster", "localhost:abc", "REDIS_SENTINEL_ADDRESSES contains an invalid port"},
		{"zero port", "true", "mymaster", "localhost:0", "REDIS_SENTINEL_ADDRESSES contains an invalid port"},
		{"large port", "true", "mymaster", "localhost:65536", "REDIS_SENTINEL_ADDRESSES contains an invalid port"},
		{"empty list entry", "true", "mymaster", "localhost:26379,", "REDIS_SENTINEL_ADDRESSES must contain host:port addresses"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetRedisEnv(t)
			t.Setenv("REDIS_SENTINEL_ENABLED", tt.enabled)
			t.Setenv("REDIS_SENTINEL_MASTER_NAME", tt.master)
			t.Setenv("REDIS_SENTINEL_ADDRESSES", tt.addresses)
			client, err := newRedisClientFromEnv()
			require.EqualError(t, err, tt.wantError)
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

func TestRedisClientsOwnership(t *testing.T) {
	var empty *RedisClients
	require.Nil(t, empty.Shared())
	require.Nil(t, empty.Derive(nil))
	require.NoError(t, empty.Close())

	mr := miniredis.RunT(t)
	shared := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, ReadTimeout: -2, MinRetryBackoff: -1})
	clients := NewDirectRedisClients(shared)
	t.Cleanup(func() { _ = clients.Close() })
	original := *shared.Options()
	first := clients.Derive(nil)
	second := clients.Derive(&RedisClientSettings{ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })
	require.Equal(t, original.MaxRetries, first.Options().MaxRetries)
	require.Equal(t, original.ReadTimeout, first.Options().ReadTimeout)
	require.Equal(t, original.MinRetryBackoff, first.Options().MinRetryBackoff)
	require.Equal(t, original.ContextTimeoutEnabled, first.Options().ContextTimeoutEnabled)
	require.NotSame(t, shared, first)
	require.NotSame(t, first, second)
	require.NoError(t, first.Close())
	require.NoError(t, shared.Ping(context.Background()).Err())
	require.NoError(t, second.Ping(context.Background()).Err())
	require.NoError(t, clients.Close())
	require.ErrorIs(t, shared.Ping(context.Background()).Err(), redis.ErrClosed)
	require.NoError(t, second.Ping(context.Background()).Err())
	require.Equal(t, original.ContextTimeoutEnabled, shared.Options().ContextTimeoutEnabled)
}

func TestRedisClientsRejectBareSentinel(t *testing.T) {
	client := redis.NewFailoverClient(&redis.FailoverOptions{MasterName: "mymaster", SentinelAddrs: []string{"127.0.0.1:0"}})
	t.Cleanup(func() { _ = client.Close() })
	require.PanicsWithValue(t, "Sentinel clients require GetRedisClients; a bare FailoverClient cannot derive independent pools", func() {
		NewDirectRedisClients(client)
	})
}

type redisTestConn struct {
	net.Conn
	address net.Addr
}

func (c redisTestConn) RemoteAddr() net.Addr { return c.address }

func TestRedisAddressHook(t *testing.T) {
	var events [][3]string
	hook := &redisAddressHook{name: "admission", emit: func(name, previous, current string) {
		events = append(events, [3]string{name, previous, current})
	}}
	dial := func(port int, err error) redis.DialHook {
		return hook.DialHook(func(context.Context, string, string) (net.Conn, error) {
			if err != nil {
				return nil, err
			}
			return redisTestConn{address: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}}, nil
		})
	}
	ctx := context.Background()
	_, err := dial(6379, nil)(ctx, "tcp", "master")
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = dial(6379, nil)(ctx, "tcp", "master")
		}()
	}
	wg.Wait()
	failure := errors.New("dial failed")
	_, err = dial(6380, failure)(ctx, "tcp", "master")
	require.ErrorIs(t, err, failure)
	_, err = dial(6380, nil)(ctx, "tcp", "master")
	require.NoError(t, err)
	require.Equal(t, [][3]string{
		{"admission", "", "127.0.0.1:6379"},
		{"admission", "127.0.0.1:6379", "127.0.0.1:6380"},
	}, events, "failed dials and unchanged addresses must not emit switch logs")
}
