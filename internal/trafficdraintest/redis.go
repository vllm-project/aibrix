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

// Package trafficdraintest contains Redis fixtures used only by traffic-drain tests.
package trafficdraintest

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

// SetIdentity supplies the INFO server section not implemented by miniredis.
// Tests change the identity only while dropping every connection to model restart.
func SetIdentity(m *miniredis.Miniredis, id *atomic.Value) {
	m.Server().SetPreHook(func(peer *server.Peer, cmd string, args ...string) bool {
		if strings.EqualFold(cmd, "INFO") && len(args) == 1 && args[0] == "server" {
			peer.WriteBulk("# Server\r\nrun_id:" + id.Load().(string) + "\r\n")
			return true
		}
		return false
	})
}

func NewRedis(t testing.TB) (*miniredis.Miniredis, *redis.Client, *atomic.Value) {
	t.Helper()
	m := miniredis.RunT(t)
	id := &atomic.Value{}
	id.Store(strings.Repeat("a", 40))
	SetIdentity(m, id)
	client := redis.NewClient(&redis.Options{Addr: m.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	return m, client, id
}
