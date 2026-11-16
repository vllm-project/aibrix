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
	"time"

	"github.com/redis/go-redis/v9"

	"k8s.io/klog/v2"
)

// RedisClientSettings overrides the per-client command retry and timeout policy.
type RedisClientSettings struct {
	MaxRetries            int
	ContextTimeoutEnabled bool
}

// RedisClientFactory creates independent pools from a Redis configuration
// snapshot. A nil settings argument preserves the configured defaults.
type RedisClientFactory func(settings *RedisClientSettings) *redis.Client

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
	options := *clientOptions
	options.ContextTimeoutEnabled = true
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
	return redis.NewClient(&options)
}

func GetRedisClient() *redis.Client {
	client, _ := GetRedisClientWithFactory()
	return client
}

// GetRedisClientWithFactory returns a connected client and a factory for clients
// with the same connection settings, including independent Sentinel failover.
func GetRedisClientWithFactory() (*redis.Client, RedisClientFactory) {
	factory, err := newRedisClientFactoryFromEnv()
	if err != nil {
		klog.Warningf("Invalid Redis configuration: %v", err)
		return nil, nil
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
		return nil, nil
	}
	klog.Infof("Connected to Redis: %s", pong)
	return client, factory
}

func newRedisClientFromEnv() (*redis.Client, error) {
	factory, err := newRedisClientFactoryFromEnv()
	if err != nil {
		return nil, err
	}
	return factory(nil), nil
}

func newRedisClientFactoryFromEnv() (RedisClientFactory, error) {
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
			return redis.NewFailoverClient(&opt)
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
