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
	"errors"
	"testing"

	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
)

// newBackstopTestManager returns a Manager wired to a fresh mock sync
// indexer, sufficient for CheckSleepStateBackstop / PurgePodPrefixCache: no
// subscribers, no podProvider, since the backstop reads state the caller
// already scraped.
func newBackstopTestManager() (*Manager, *mockSyncIndexerWithErrors) {
	return newBackstopTestManagerWithIndexer(&mockSyncIndexerWithErrors{})
}

// newBackstopTestManagerWithIndexer is newBackstopTestManager for a caller
// that needs to preconfigure the mock indexer (e.g. removePrefixErr) before
// any call happens.
func newBackstopTestManagerWithIndexer(indexer *mockSyncIndexerWithErrors) (*Manager, *mockSyncIndexerWithErrors) {
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
//
// unsubscribeFromPod only calls forgetSleepState when a subscriber actually
// existed for the pod key (its early return otherwise skips it entirely), so
// this seeds one. It also seeds the tracked state as asleep, not awake,
// before unsubscribing: an awake seed would still purge on the next asleep
// observation even if forgetting were a complete no-op, since a first
// observation is itself seeded as awake. Only starting from an asleep latch
// tells the two cases apart.
func TestUnsubscribeFromPodForgetsSleepState(t *testing.T) {
	manager, indexer := newBackstopTestManager()
	ctx := context.Background()

	manager.subscribers.Store("default/pod-a", kvcache.NewZMQClient(&kvcache.ZMQClientConfig{PodKey: "default/pod-a"}, nil))

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 1) // awake
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0) // asleep, purge #1
	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected one purge before unsubscribing, got %d", len(indexer.removePrefixCalls))
	}

	manager.unsubscribeFromPod("default/pod-a")

	// If forgetting worked, this is a first observation again (seeded awake,
	// so an asleep reading is a transition): purge #2. If forgetSleepState
	// never ran or is a no-op, the map still holds the asleep latch from
	// above and this is silently dropped.
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 2 {
		t.Fatalf("expected the post-unsubscribe observation to purge as a fresh first observation, got %d calls total", len(indexer.removePrefixCalls))
	}
}

func TestPurgePodPrefixCacheSwallowsTemporaryError(t *testing.T) {
	syncProvider := &mockSyncProvider{err: ErrIndexerNotInitialized}
	manager := &Manager{syncProvider: syncProvider, ctx: context.Background()}

	if err := manager.PurgePodPrefixCache(context.Background(), "model-x", 0, "default/pod-a"); err != nil {
		t.Fatalf("PurgePodPrefixCache returned %v, want nil for a temporary error", err)
	}
}

// flakySyncProvider fails GetSyncIndexer for the first failCount calls, then
// delegates to a real indexer. Used to simulate the indexer coming up after
// the gateway restarts while a pod is already asleep.
type flakySyncProvider struct {
	indexer   SyncIndexer
	failCount int
	calls     int
}

func (p *flakySyncProvider) GetSyncIndexer(ctx context.Context) (SyncIndexer, error) {
	p.calls++
	if p.calls <= p.failCount {
		return nil, ErrIndexerNotInitialized
	}
	return p.indexer, nil
}

// A temporary error swallowed by PurgePodPrefixCache must not be treated as a
// successful purge: latching the tracked state to asleep on that swallowed
// error would mean no later asleep observation ever retries it, since the
// only interesting event this method acts on is a transition (review on
// #2735).
func TestCheckSleepStateBackstopRetriesAfterTemporaryIndexerError(t *testing.T) {
	indexer := &mockSyncIndexerWithErrors{}
	provider := &flakySyncProvider{indexer: indexer, failCount: 1}
	manager := &Manager{syncProvider: provider, ctx: context.Background()}
	ctx := context.Background()

	// First observation is already asleep (gateway restarted while the pod
	// was sleeping) and the indexer is not ready yet: the purge attempt is
	// swallowed as a temporary error, and must not count as done.
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 0 {
		t.Fatalf("expected no purge while the indexer is not ready, got %d", len(indexer.removePrefixCalls))
	}

	// The indexer is up by the next tick. The engine is still asleep, so this
	// must retry rather than treat the earlier swallowed error as the
	// transition already having been handled.
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected the retry to purge once the indexer is ready, got %d calls", len(indexer.removePrefixCalls))
	}

	// A further asleep observation must not re-purge now that it succeeded.
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected no further purge once the transition is handled, got %d calls", len(indexer.removePrefixCalls))
	}
}

// A hard RemovePrefix error (not a temporary indexer-not-ready error) must
// also not latch the tracked state to asleep, or the same permanent-stall
// bug applies.
func TestCheckSleepStateBackstopRetriesAfterHardRemovePrefixError(t *testing.T) {
	indexer := &mockSyncIndexerWithErrors{removePrefixErr: errors.New("boom")}
	manager, _ := newBackstopTestManagerWithIndexer(indexer)
	ctx := context.Background()

	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 1 {
		t.Fatalf("expected one attempted purge, got %d", len(indexer.removePrefixCalls))
	}

	indexer.removePrefixErr = nil
	manager.CheckSleepStateBackstop(ctx, "default/pod-a", "model-x", 0, 0)
	if len(indexer.removePrefixCalls) != 2 {
		t.Fatalf("expected the retry to purge once RemovePrefix stops failing, got %d calls", len(indexer.removePrefixCalls))
	}
}
