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

package modelclaim

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestCapacityReservationCachePreventsOvercommitUntilObserved(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	cache := newCapacityReservationCache(time.Minute, func() time.Time { return now })
	pod := types.NamespacedName{Namespace: "default", Name: "warm-1"}
	podUID := types.UID("pod-uid")
	state := CapacityState{Known: true, FreeFraction: 1}

	require.True(t, cache.Reserve(pod, podUID, "claim-1", 0.45, state))
	require.True(t, cache.Reserve(pod, podUID, "claim-2", 0.45, state))
	assert.False(t, cache.Reserve(pod, podUID, "claim-3", 0.45, state))
	assert.InDelta(t, 0.90, cache.PendingFraction(pod, podUID), 0.0001)

	cache.Observe(pod, podUID, &RuntimeSnapshot{Models: []RuntimeSnapshotModel{{
		ClaimRef: &ModelClaimRef{UID: "claim-1"},
	}}})

	assert.InDelta(t, 0.45, cache.PendingFraction(pod, podUID), 0.0001)
}

func TestCapacityReservationCacheExpiresUnobservedReservation(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	cache := newCapacityReservationCache(time.Minute, func() time.Time { return now })
	pod := types.NamespacedName{Namespace: "default", Name: "warm-1"}
	podUID := types.UID("pod-uid")

	require.True(t, cache.Reserve(pod, podUID, "claim-1", 0.45, CapacityState{Known: true, FreeFraction: 1}))
	now = now.Add(time.Minute + time.Nanosecond)

	assert.Zero(t, cache.PendingFraction(pod, podUID))
}
