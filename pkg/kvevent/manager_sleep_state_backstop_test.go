// Copyright 2026 AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build zmq

package kvevent

import (
	"context"
	"testing"
)

// newBackstopTestManager returns a Manager wired to a fresh mock sync
// indexer, sufficient for CheckSleepStateBackstop / PurgePodPrefixCache: no
// subscribers, no podProvider, since the backstop reads state the caller
// already scraped.
func newBackstopTestManager() (*Manager, *mockSyncIndexerWithErrors) {
	indexer := &mockSyncIndexerWithErrors{}
	manager := &Manager{
		syncProvider: &mockSyncProvider{indexer: indexer},
		ctx:          context.Background(),
	}
	return manager, indexer
}

func TestCheckSleepStateBackstopPurgesOnAwakeToAsleepTransition(t *testing.T) {
	manager, indexer := newBackstopTestManager()
	ctx := context.Background()

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 1) // awake
	if len(indexer.removePrefixCalls) != 0 {
		t.Fatalf("purged on an awake observation: %d calls", len(indexer.removePrefixCalls))
	}

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0) // asleep
	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected 1 purge on the awake-to-asleep transition, got %d", len(indexer.removePrefixCalls))
	}
	call := indexer.removePrefixCalls[0]
	if call.modelName != "model-x" || call.loraID != 0 || call.podKey != "default/pod-a" {
		t.Errorf("purge scoped to %+v, want model-x/0/default/pod-a", call)
	}
}

func TestCheckSleepStateBackstopDoesNotRepurgeWhileStillAsleep(t *testing.T) {
	manager, indexer := newBackstopTestManager()
	ctx := context.Background()

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 1)
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)

	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected exactly 1 purge across repeated asleep observations, got %d", len(indexer.removePrefixCalls))
	}
}

func TestCheckSleepStateBackstopDoesNotPurgeOnWaking(t *testing.T) {
	manager, indexer := newBackstopTestManager()
	ctx := context.Background()

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 1)
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0) // purges once
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 1) // wakes back up

	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected no purge on the asleep-to-awake transition, got %d total calls", len(indexer.removePrefixCalls))
	}
}

// A pod-model pair with no prior observation is seeded as having been awake,
// so a gateway restart while a pod is already asleep still self-corrects:
// this is the case a purely edge-triggered design without seeding would
// silently miss forever.
func TestCheckSleepStateBackstopSeedsFirstObservationAsAwake(t *testing.T) {
	manager, indexer := newBackstopTestManager()
	ctx := context.Background()

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)

	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected 1 purge on a first observation that is already asleep, got %d", len(indexer.removePrefixCalls))
	}

	// The seeded transition must not repeat on the next tick.
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected the seeded purge not to repeat, got %d total calls", len(indexer.removePrefixCalls))
	}
}

func TestCheckSleepStateBackstopTracksModelsOnOnePodIndependently(t *testing.T) {
	manager, indexer := newBackstopTestManager()
	ctx := context.Background()

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 1)
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-y", 0, 1)

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected model-x's transition to purge only model-x, got %d calls", len(indexer.removePrefixCalls))
	}
	if indexer.removePrefixCalls[0].modelName != "model-x" {
		t.Errorf("purged %q, want model-x", indexer.removePrefixCalls[0].modelName)
	}

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-y", 0, 0)
	if len(indexer.removePrefixCalls) != 2 {
		t.Fatalf("expected model-y's own transition to also purge, got %d calls total", len(indexer.removePrefixCalls))
	}
}

// unsubscribeFromPod must forget the tracked state, not just the ZMQ
// subscription: otherwise a pod key reused by a genuinely new pod (a
// Deployment replacing it) inherits stale sleep-state history instead of
// being seeded fresh.
func TestUnsubscribeFromPodForgetsSleepState(t *testing.T) {
	manager, indexer := newBackstopTestManager()
	ctx := context.Background()

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 1) // awake
	manager.unsubscribeFromPod("default/pod-a")

	// After forgetting, the next observation is a first observation again:
	// asleep must seed-and-purge, not be silently accepted as "no change".
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected the post-unsubscribe observation to purge as a fresh first observation, got %d calls", len(indexer.removePrefixCalls))
	}
}

func TestPurgePodPrefixCacheSwallowsTemporaryError(t *testing.T) {
	syncProvider := &mockSyncProvider{err: ErrIndexerNotInitialized}
	manager := &Manager{syncProvider: syncProvider, ctx: context.Background()}

	if err := manager.PurgePodPrefixCache(context.Background(), "model-x", 0, "default/pod-a"); err != nil {
		t.Fatalf("PurgePodPrefixCache returned %v, want nil for a temporary error", err)
	}
}
