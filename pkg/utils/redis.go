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
	"net"
	"strings"

	"github.com/redis/go-redis/v9"

	"k8s.io/klog/v2"
)

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
	redisHost := LoadEnv("REDIS_HOST", "localhost")
	redisPort := LoadEnv("REDIS_PORT", "6379")
	redisPassword := LoadEnv("REDIS_PASSWORD", "")
	// Connect to Redis
	client := redis.NewClient(&redis.Options{
		Addr:     redisAddress(redisHost, redisPort),
		Password: redisPassword,
		DB:       0, // Default DB
	})
	pong, err := client.Ping(context.Background()).Result()
	if err != nil {
		klog.Warningf("Redis not available: %v", err)
		if closeErr := client.Close(); closeErr != nil {
			klog.Warningf("Error closing Redis client: %v", closeErr)
		}
		return nil
	}
	klog.Infof("Connected to Redis: %s", pong)
	return client
}
