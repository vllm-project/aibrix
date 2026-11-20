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

package utils

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"k8s.io/klog/v2"
)

// RedisClientSettings overrides the per-client command retry and timeout policy.
type RedisClientSettings struct {
	Name                  string
	MaxRetries            int
	ContextTimeoutEnabled bool
}

// redisClientFactory creates independent pools from a Redis configuration
// snapshot. A nil settings argument preserves the configured defaults.
type redisClientFactory func(settings *RedisClientSettings) *redis.Client

// RedisClients holds the shared pool and its immutable connection configuration.
// Close releases only Shared; each consumer owns and closes the pools it derives.
type RedisClients struct {
	shared  *redis.Client
	factory redisClientFactory
}

func (c *RedisClients) Shared() *redis.Client {
	if c == nil {
		return nil
	}
	return c.shared
}

func (c *RedisClients) Close() error {
	if c.Shared() == nil {
		return nil
	}
	return c.shared.Close()
}

// Derive creates an independent pool. Zero MaxRetries means no command retries;
// nil settings preserves the configured defaults.
func (c *RedisClients) Derive(settings *RedisClientSettings) *redis.Client {
	if c.Shared() == nil {
		return nil
	}
	if settings != nil {
		copySettings := *settings
		if copySettings.MaxRetries == 0 {
			copySettings.MaxRetries = -1
		}
		settings = &copySettings
	}
	return c.factory(settings)
}

// NewDirectRedisClients wraps a direct client without changing it.
// Consumers borrow Shared; the original caller retains responsibility for closing it.
// Use GetRedisClients for Sentinel: copying Options loses pool invalidation.
func NewDirectRedisClients(client *redis.Client) *RedisClients {
	if client == nil {
		return nil
	}
	if client.Options().Addr == "FailoverClient" {
		panic("Sentinel clients require GetRedisClients; a bare FailoverClient cannot derive independent pools")
	}
	options := *client.Options()
	return &RedisClients{shared: client, factory: func(settings *RedisClientSettings) *redis.Client {
		opt := redisOptionsInput(options)
		if settings != nil {
			opt.MaxRetries = settings.MaxRetries
			opt.ContextTimeoutEnabled = settings.ContextTimeoutEnabled
		}
		return redis.NewClient(&opt)
	}}
}

// CheckRedisConnectivity checks the configured master and releases its pool.
// Unlike GetRedisClient, it honors cancellation and reports configuration errors.
func CheckRedisConnectivity(ctx context.Context) error {
	factory, err := newRedisClientFactoryFromEnv()
	if err != nil {
		return err
	}
	client := factory(&RedisClientSettings{MaxRetries: -1, ContextTimeoutEnabled: true})
	defer func() { _ = client.Close() }()
	return client.Ping(ctx).Err()
}

func redisAddress(host, port string) string {
	host = strings.TrimSpace(host)
	if len(host) >= 2 && strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return net.JoinHostPort(host, port)
}

// NewRedisClientWithContextTimeout copies options from Client.Options and sets
// ContextTimeoutEnabled while preserving effective timeouts, retry count and backoffs.
// Timeout settings that disable socket deadline updates continue to do so.
// Redis initialization commands (including HELLO and CLIENT SETINFO) share the caller's
// deadline when socket deadline updates are enabled. A 100 ms budget assumes Redis RTT
// well below about 30 ms; cold connections on slower links can exhaust it and prevent
// the pool from growing.
// The caller owns and must close the returned client. Nil options return nil.
func NewRedisClientWithContextTimeout(clientOptions *redis.Options) *redis.Client {
	if clientOptions == nil {
		return nil
	}
	options := redisOptionsInput(*clientOptions)
	options.ContextTimeoutEnabled = true
	return redis.NewClient(&options)
}

func redisOptionsInput(options redis.Options) redis.Options {
	// NewClient normalizes disabled retries to 0, but reads 0 as the default on input.
	if options.MaxRetries == 0 {
		options.MaxRetries = -1
	}
	// Restore input sentinels so NewClient preserves normalized timeout/backoff values.
	switch options.ReadTimeout {
	case 0:
		options.ReadTimeout = -1
	case -1:
		options.ReadTimeout = -2
	}
	switch options.WriteTimeout {
	case 0:
		options.WriteTimeout = -1
	case -1:
		options.WriteTimeout = -2
	}
	if options.MinRetryBackoff == 0 {
		options.MinRetryBackoff = -1
	}
	if options.MaxRetryBackoff == 0 {
		options.MaxRetryBackoff = -1
	}
	return options
}

func GetRedisClient() *redis.Client {
	return GetRedisClients().Shared()
}

// GetRedisClients returns the shared pool and independent-pool configuration.
func GetRedisClients() *RedisClients {
	factory, err := newRedisClientFactoryFromEnv()
	if err != nil {
		klog.Warningf("Invalid Redis configuration: %v", err)
		return nil
	}
	client := factory(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pong, err := client.Ping(ctx).Result()
	if err != nil {
		klog.Warningf("Redis not available: %v", err)
		if closeErr := client.Close(); closeErr != nil {
			klog.Warningf("Error closing Redis client: %v", closeErr)
		}
		return nil
	}
	klog.Infof("Connected to Redis: %s", pong)
	return &RedisClients{shared: client, factory: factory}
}

func newRedisClientFromEnv() (*redis.Client, error) {
	factory, err := newRedisClientFactoryFromEnv()
	if err != nil {
		return nil, err
	}
	return factory(nil), nil
}

func newRedisClientFactoryFromEnv() (redisClientFactory, error) {
	sentinelEnabled, err := strconv.ParseBool(LoadEnv("REDIS_SENTINEL_ENABLED", "false"))
	if err != nil {
		return nil, fmt.Errorf("REDIS_SENTINEL_ENABLED must be a boolean")
	}
	redisPassword := LoadEnv("REDIS_PASSWORD", "")
	if sentinelEnabled {
		masterName := strings.TrimSpace(LoadEnv("REDIS_SENTINEL_MASTER_NAME", ""))
		if masterName == "" {
			return nil, fmt.Errorf("REDIS_SENTINEL_MASTER_NAME is required")
		}
		rawAddresses := strings.TrimSpace(LoadEnv("REDIS_SENTINEL_ADDRESSES", ""))
		if rawAddresses == "" {
			return nil, fmt.Errorf("REDIS_SENTINEL_ADDRESSES is required")
		}
		addresses := strings.Split(rawAddresses, ",")
		for i, address := range addresses {
			address = strings.TrimSpace(address)
			host, port, err := net.SplitHostPort(address)
			if err != nil || host == "" {
				return nil, fmt.Errorf("REDIS_SENTINEL_ADDRESSES must contain host:port addresses")
			}
			portNumber, err := strconv.Atoi(port)
			if err != nil || portNumber < 1 || portNumber > 65535 {
				return nil, fmt.Errorf("REDIS_SENTINEL_ADDRESSES contains an invalid port")
			}
			addresses[i] = address
		}
		// All operations go to the discovered master; replicas cannot serve quota writes.
		options := redis.FailoverOptions{
			MasterName:            masterName,
			SentinelAddrs:         addresses,
			SentinelUsername:      LoadEnv("REDIS_SENTINEL_USERNAME", ""),
			SentinelPassword:      LoadEnv("REDIS_SENTINEL_PASSWORD", ""),
			Password:              redisPassword,
			DB:                    0,
			ContextTimeoutEnabled: true,
		}
		return func(settings *RedisClientSettings) *redis.Client {
			opt := options
			opt.SentinelAddrs = append([]string(nil), options.SentinelAddrs...)
			if settings != nil {
				opt.MaxRetries = settings.MaxRetries
				opt.ContextTimeoutEnabled = settings.ContextTimeoutEnabled
			}
			// Copying a FailoverClient's Options only shares its Dialer, not
			// its pool invalidation callback. Each pool needs its own failover.
			client := redis.NewFailoverClient(&opt)
			name := "shared"
			if settings != nil && settings.Name != "" {
				name = settings.Name
			}
			client.AddHook(&redisAddressHook{name: name, emit: logRedisAddress})
			return client
		}, nil
	}
	redisHost := LoadEnv("REDIS_HOST", "localhost")
	redisPort := LoadEnv("REDIS_PORT", "6379")
	options := redis.Options{
		Addr:     redisAddress(redisHost, redisPort),
		Password: redisPassword,
		DB:       0,
	}
	return func(settings *RedisClientSettings) *redis.Client {
		opt := options
		if settings != nil {
			opt.MaxRetries = settings.MaxRetries
			opt.ContextTimeoutEnabled = settings.ContextTimeoutEnabled
		}
		return redis.NewClient(&opt)
	}, nil
}

// Observe successful connections per pool, without replacing go-redis's global logger.
// A notification alone does not imply that a pool has connected to the new master.
type redisAddressHook struct {
	mu      sync.Mutex
	name    string
	address string
	emit    func(name, previous, current string)
}

func logRedisAddress(name, previous, current string) {
	if previous == "" {
		klog.V(2).InfoS("Redis pool connected", "pool", name, "address", current)
		return
	}
	klog.InfoS("Redis pool connection address changed", "pool", name, "previousAddress", previous, "address", current)
}

func (h *redisAddressHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := next(ctx, network, address)
		if err == nil {
			current := conn.RemoteAddr().String()
			h.mu.Lock()
			if current != h.address {
				h.emit(h.name, h.address, current)
				h.address = current
			}
			h.mu.Unlock()
		}
		return conn, err
	}
}

func (h *redisAddressHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (h *redisAddressHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
