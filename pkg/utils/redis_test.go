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
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

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
			if got := redisAddress(tt.host, tt.port); got != tt.want {
				t.Fatalf("redisAddress(%q, %q) = %q, want %q", tt.host, tt.port, got, tt.want)
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
