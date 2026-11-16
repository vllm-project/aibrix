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

package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// snapshotFreshness feeds aibrix_gateway_snapshot_remote_keys and
// aibrix_gateway_snapshot_oldest_read_age_seconds: the other gateways' keys of a published tick,
// by whether the tick read them, carried them within remoteRunningMaxAge, or carried them past
// it, and the age of the least recently read one. This gateway's own keys and keys without pod
// identity are not counted, as remoteRunningByPod does not count them.
func TestSnapshotFreshnessCountsOtherGatewaysKeysByState(t *testing.T) {
	start := time.Now()
	snapshot := func(gw string) map[string]string {
		return map[string]string{"gateway_instance_id": gw, "namespace": "ns", "pod_name": "a", "requests_running": "1"}
	}
	raw := map[string]map[string]string{
		"fresh-1":    snapshot("gw-1"),
		"fresh-2":    snapshot("gw-2"),
		"carried":    snapshot("gw-3"),
		"at-limit":   snapshot("gw-4"),
		"stale":      snapshot("gw-5"),
		"self-stale": snapshot("self"),
		"no-pod":     {"gateway_instance_id": "gw-6", "requests_running": "1"},
	}
	readAt := map[string]time.Time{
		"fresh-1":    start,
		"fresh-2":    start,
		"carried":    start.Add(-300 * time.Millisecond),
		"at-limit":   start.Add(-remoteRunningMaxAge),
		"stale":      start.Add(-3 * time.Second),
		"self-stale": start.Add(-time.Hour),
		"no-pod":     start.Add(-time.Hour),
	}
	fresh, carried, stale, oldest := snapshotFreshness(raw, readAt, "self", start)
	assert.Equal(t, 2, fresh)
	assert.Equal(t, 2, carried, "a key exactly remoteRunningMaxAge old still counts, as in freshCount")
	assert.Equal(t, 1, stale)
	assert.Equal(t, 3*time.Second, oldest, "this gateway's own and malformed keys do not set the oldest age")

	fresh, carried, stale, oldest = snapshotFreshness(nil, nil, "self", start)
	assert.Equal(t, [4]any{0, 0, 0, time.Duration(0)}, [4]any{fresh, carried, stale, oldest}, "no other gateways' keys")
}
