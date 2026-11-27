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

package types

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTrackerKeyIsUniquePerRequest: two concurrent requests that share one client trace id (so
// RequestID is identical) must still get distinct tracker keys, while keeping RequestID as the
// human-readable prefix.
func TestTrackerKeyIsUniquePerRequest(t *testing.T) {
	const sharedID = "4bf92f3577b34da6a3ce929d0e0e4736" // one client trace id, reused by both

	a := NewRoutingContext(context.Background(), "pd", "model", "msg", sharedID, "user")
	defer a.Delete()
	b := NewRoutingContext(context.Background(), "pd", "model", "msg", sharedID, "user")
	defer b.Delete()

	assert.Equal(t, sharedID, a.RequestID)
	assert.Equal(t, sharedID, b.RequestID)
	assert.NotEqual(t, a.TrackerKey(), b.TrackerKey(), "requests sharing a trace id must get distinct tracker keys")
	assert.True(t, strings.HasPrefix(a.TrackerKey(), sharedID+"-"), "the key keeps RequestID as a greppable prefix")
}

// TestTrackerKeyFreshOnPoolReuse: a RoutingContext returned to the pool and reused for the same
// request id gets a fresh key, so a prior request's charge can never be released under a reused key.
func TestTrackerKeyFreshOnPoolReuse(t *testing.T) {
	const id = "req-reuse"
	first := NewRoutingContext(context.Background(), "pd", "model", "msg", id, "user")
	key1 := first.TrackerKey()
	first.Delete() // return to the pool

	second := NewRoutingContext(context.Background(), "pd", "model", "msg", id, "user")
	defer second.Delete()
	assert.NotEqual(t, key1, second.TrackerKey(), "a reused pooled context must mint a fresh tracker key")
}

// TestTrackerKeyEmptyForUnresetContext documents that a bare &RoutingContext{} (never built via
// NewRoutingContext, e.g. the metrics helpers) has no key and must not be used as a tracker key.
func TestTrackerKeyEmptyForUnresetContext(t *testing.T) {
	assert.Equal(t, "", (&RoutingContext{}).TrackerKey())
}
