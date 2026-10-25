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

package trafficdrain

import (
	"net"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

// NewFromEnvironment uses the same standalone Redis configuration as the Gateway.
// It does not connect until an opt-in Pod needs coordination.
func NewFromEnvironment() *Store {
	host, port := strings.TrimSpace(os.Getenv("REDIS_HOST")), os.Getenv("REDIS_PORT")
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "6379"
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return newWithOptions(redis.Options{Addr: net.JoinHostPort(host, port), Password: os.Getenv("REDIS_PASSWORD"), DB: 0})
}
