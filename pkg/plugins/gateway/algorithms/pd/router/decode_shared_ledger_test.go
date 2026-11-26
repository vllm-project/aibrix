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

package router

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	v1 "k8s.io/api/core/v1"
)

// fakeSharedLedgerCache is a test cache whose other gateway replicas hold the
// decode ledger in remote.
type fakeSharedLedgerCache struct {
	*cache.Store
	available bool
	remote    map[string]cache.RemoteDecodeLoad
	ledgerErr error

	mu              sync.Mutex
	provider        cache.DecodeLedgerProvider
	notified        []string
	reads           int
	ledgerOnlyReads int
}

func (f *fakeSharedLedgerCache) SharedDecodeLedgerAvailable() bool { return f.available }

func (f *fakeSharedLedgerCache) PublishDecodeLedger(provider cache.DecodeLedgerProvider) func(string) {
	f.provider = provider
	return func(podKey string) {
		f.mu.Lock()
		f.notified = append(f.notified, podKey)
		f.mu.Unlock()
	}
}

func (f *fakeSharedLedgerCache) GetPodsRunningRequestsAndDecodeLedger(pods []*v1.Pod) (map[string]int64, map[string]cache.RemoteDecodeLoad, error) {
	f.mu.Lock()
	f.reads++
	f.mu.Unlock()
	counts, err := f.GetPodsRunningRequests(pods)
	return counts, f.remote, err
}

func (f *fakeSharedLedgerCache) GetPodsDecodeLedger(_ []*v1.Pod) (map[string]cache.RemoteDecodeLoad, error) {
	f.mu.Lock()
	f.ledgerOnlyReads++
	err := f.ledgerErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.remote, nil
}

func withTokenLoadSharedLedger(t *testing.T, on bool) {
	t.Helper()
	prev := aibrixTokenLoadSharedLedger
	aibrixTokenLoadSharedLedger = on
	t.Cleanup(func() { aibrixTokenLoadSharedLedger = prev })
}

// With the ledger shared, token_load adds what the other replicas hold on each
// decode pod to this replica's own charges: here this replica has charged
// decode-0 and another replica has charged more to decode-1, so the request
// goes to decode-0. With the ledger local, it goes to decode-1.
func TestPDRouter_DecodeTokenLoadAddsRemoteLedger(t *testing.T) {
	decode0 := burstPod("decode-0", "decode", "127.0.0.100")
	decode1 := burstPod("decode-1", "decode", "127.0.0.101")
	readyPods := []*v1.Pod{burstPod("prefill-0", "prefill", "127.0.0.1"), decode0, decode1}

	route := func(t *testing.T, shared bool) (string, *fakeSharedLedgerCache) {
		withTokenLoadSharedLedger(t, true)
		r, tokenLoad := newDecodeTokenLoadTestRouter(t)
		fake := &fakeSharedLedgerCache{
			Store:     r.cache.(*cache.Store),
			available: shared,
			remote:    map[string]cache.RemoteDecodeLoad{burstPodKey(decode1.Name): {Tokens: 5000}},
		}
		r.cache = fake
		r.shareDecodeLedger()
		tokenLoad.AcquireDecodeWithTTL("local", burstPodKey(decode0.Name), 1000, 0)

		_, decode, err := r.filterPrefillDecodePods(tokenLoadRequest(t, "next", 400), readyPods)
		require.NoError(t, err)
		return decode.Name, fake
	}

	picked, fake := route(t, true)
	assert.Equal(t, decode0.Name, picked, "decode-1 carries 5000 tokens charged by another replica")
	assert.Equal(t, 1, fake.reads, "without a snapshot, the remote ledger is read with the running-request counts")
	assert.Zero(t, fake.ledgerOnlyReads)

	picked, fake = route(t, false)
	assert.Equal(t, decode1.Name, picked, "with a local ledger only this replica's charge on decode-0 counts")
	assert.Zero(t, fake.reads)
	assert.Zero(t, fake.ledgerOnlyReads)
}

// When the routing decision already holds a populated RunningRequests snapshot
// covering the decode pods (e.g. from narrowToInflightCapacity), token_load
// reuses those cached counts and calls GetPodsDecodeLedger instead of
// re-reading running-request hashes. When the snapshot does not cover all
// decode pods, it falls back to the combined single-RTT read.
func TestPDRouter_DecodeTokenLoadReusesSnapshotWithLedgerOnlyRead(t *testing.T) {
	decode0 := burstPod("decode-0", "decode", "127.0.0.100")
	decode1 := burstPod("decode-1", "decode", "127.0.0.101")
	readyPods := []*v1.Pod{burstPod("prefill-0", "prefill", "127.0.0.1"), decode0, decode1}
	decodePods := []*v1.Pod{decode0, decode1}

	newRouter := func(t *testing.T) (*pdRouter, *pd.TokenLoadTracker, *fakeSharedLedgerCache) {
		t.Helper()
		withTokenLoadSharedLedger(t, true)
		r, tokenLoad := newDecodeTokenLoadTestRouter(t)
		fake := &fakeSharedLedgerCache{
			Store:     r.cache.(*cache.Store),
			available: true,
			remote:    map[string]cache.RemoteDecodeLoad{burstPodKey(decode1.Name): {Tokens: 5000}},
		}
		r.cache = fake
		r.shareDecodeLedger()
		return r, tokenLoad, fake
	}

	t.Run("snapshot hit uses ledger-only read", func(t *testing.T) {
		r, tokenLoad, fake := newRouter(t)
		tokenLoad.AcquireDecodeWithTTL("local", burstPodKey(decode0.Name), 1000, 0)

		ctx := tokenLoadRequest(t, "snap-hit", 400)
		ctx.StartRunningRequestsSnapshot()
		defer ctx.StopRunningRequestsSnapshot()
		_, err := ctx.RunningRequests(readyPods, func([]*v1.Pod) (map[string]int64, error) {
			return map[string]int64{
				burstPodKey("prefill-0"):  0,
				burstPodKey(decode0.Name): 7,
				burstPodKey(decode1.Name): 11,
			}, nil
		})
		require.NoError(t, err)

		_, picked, err := r.filterPrefillDecodePods(ctx, readyPods)
		require.NoError(t, err)
		assert.Equal(t, decode0.Name, picked.Name, "remote decode ledger on decode-1 must still steer traffic to decode-0")
		assert.Zero(t, fake.reads, "snapshot hit must not call combined GetPodsRunningRequestsAndDecodeLedger")
		assert.Equal(t, 1, fake.ledgerOnlyReads, "snapshot hit must call ledger-only GetPodsDecodeLedger")
	})

	t.Run("snapshot hit preserves cached counts even if ledger-only read errors", func(t *testing.T) {
		r, _, fake := newRouter(t)
		fake.ledgerErr = errors.New("redis blip")

		ctx := tokenLoadRequest(t, "snap-hit-err", 400)
		ctx.StartRunningRequestsSnapshot()
		defer ctx.StopRunningRequestsSnapshot()
		_, err := ctx.RunningRequests(decodePods, func([]*v1.Pod) (map[string]int64, error) {
			return map[string]int64{
				burstPodKey(decode0.Name): 7,
				burstPodKey(decode1.Name): 11,
			}, nil
		})
		require.NoError(t, err)

		var remote map[string]cache.RemoteDecodeLoad
		counts, err := r.decodeRunningRequests(ctx, decodePods, &remote)
		require.NoError(t, err, "a failed ledger read must not discard valid cached running counts")
		assert.Equal(t, int64(7), counts[burstPodKey(decode0.Name)])
		assert.Equal(t, int64(11), counts[burstPodKey(decode1.Name)])
		assert.Nil(t, remote)
		assert.Zero(t, fake.reads)
		assert.Equal(t, 1, fake.ledgerOnlyReads)
	})

	t.Run("snapshot miss falls back to combined single-RTT read", func(t *testing.T) {
		r, tokenLoad, fake := newRouter(t)
		tokenLoad.AcquireDecodeWithTTL("local", burstPodKey(decode0.Name), 1000, 0)

		ctx := tokenLoadRequest(t, "snap-miss", 400)
		ctx.StartRunningRequestsSnapshot()
		defer ctx.StopRunningRequestsSnapshot()
		// Populate snapshot for decode-0 only, leaving decode-1 uncovered.
		_, err := ctx.RunningRequests([]*v1.Pod{decode0}, func([]*v1.Pod) (map[string]int64, error) {
			return map[string]int64{burstPodKey(decode0.Name): 3}, nil
		})
		require.NoError(t, err)

		_, picked, err := r.filterPrefillDecodePods(ctx, readyPods)
		require.NoError(t, err)
		assert.Equal(t, decode0.Name, picked.Name)
		assert.Equal(t, 1, fake.reads, "partial snapshot miss must fall back to combined single-RTT read")
		assert.Zero(t, fake.ledgerOnlyReads)
	})
}

// Policies other than token_load don't read the remote ledger.
func TestPDRouter_SharedLedgerReadOnlyForTokenLoad(t *testing.T) {
	withTokenLoadSharedLedger(t, true)
	readyPods := []*v1.Pod{
		burstPod("prefill-0", "prefill", "127.0.0.1"),
		burstPod("decode-0", "decode", "127.0.0.100"),
		burstPod("decode-1", "decode", "127.0.0.101"),
	}
	r, _ := newDecodeTokenLoadTestRouter(t)
	r.decodePolicy = pd.LeastRequestDecodePolicy{}
	fake := &fakeSharedLedgerCache{Store: r.cache.(*cache.Store), available: true}
	r.cache = fake
	r.shareDecodeLedger()

	_, _, err := r.filterPrefillDecodePods(tokenLoadRequest(t, "next", 400), readyPods)
	require.NoError(t, err)
	assert.Zero(t, fake.reads)
}

// decodeTokenLoad adds the remote charges, and their estimated output at the
// pod's per-request rate, to the local ledger.
func TestPDRouter_DecodeTokenLoadRemoteGrowth(t *testing.T) {
	r, tokenLoad := newDecodeTokenLoadTestRouter(t)
	pod := burstPodKey("decode-0")
	tokenLoad.AcquireDecodeWithTTL("local", pod, 1000, 0)
	remote := map[string]cache.RemoteDecodeLoad{pod: {Tokens: 300, Elapsed: 10}}

	assert.Equal(t, 1300.0, r.decodeTokenLoad(pod, 0, remote), "no rate: no growth")
	assert.InDelta(t, 1300.0+50*10, r.decodeTokenLoad(pod, 50, remote), 50*0.5,
		"50 tokens/s for 10 s of remote charges, plus a negligible local growth")
	assert.InDelta(t, 1000.0, r.decodeTokenLoad(pod, 50, nil), 50*0.5, "no remote ledger: local only")
	assert.InDelta(t, 1000.0, r.decodeTokenLoad(burstPodKey("decode-0"), 50, map[string]cache.RemoteDecodeLoad{}), 50*0.5)
}

// shareDecodeLedger publishes the ledger and reads the others' only when the
// flag is on and the cache can share it.
func TestPDRouter_ShareDecodeLedger(t *testing.T) {
	cases := []struct {
		name      string
		flag      bool
		available bool
		shared    bool
	}{
		{"on and available", true, true, true},
		{"flag off", false, true, false},
		{"no redis", true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTokenLoadSharedLedger(t, tc.flag)
			r, tokenLoad := newDecodeTokenLoadTestRouter(t)
			fake := &fakeSharedLedgerCache{Store: r.cache.(*cache.Store), available: tc.available}
			r.cache = fake
			r.shareDecodeLedger()

			tokenLoad.AcquireDecodeWithTTL("req", "ns/decode-0", 100, 0)
			if !tc.shared {
				assert.Nil(t, r.sharedDecodeLedger)
				assert.Empty(t, fake.notified)
				return
			}
			assert.NotNil(t, r.sharedDecodeLedger)
			assert.Equal(t, []string{"ns/decode-0"}, fake.notified)
			require.NotNil(t, fake.provider)
			state := fake.provider("ns/decode-0")
			assert.Equal(t, 100.0, state.Tokens)
			assert.EqualValues(t, 1, state.Charges)
			assert.Greater(t, state.SumChargedAt, 0.0)
		})
	}

	t.Run("cache without sharing", func(t *testing.T) {
		withTokenLoadSharedLedger(t, true)
		r, _ := newDecodeTokenLoadTestRouter(t)
		r.shareDecodeLedger()
		assert.Nil(t, r.sharedDecodeLedger, "the plain test store has no Redis")
	})
}
