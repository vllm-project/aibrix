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
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
)

const testLedgerPodKey = testNamespace + "/" + testPodName

func ledgerField(t *testing.T, client *redis.Client, key, field string) (string, bool) {
	t.Helper()
	v, err := client.HGet(context.Background(), key, field).Result()
	if err == redis.Nil {
		return "", false
	}
	require.NoError(t, err)
	return v, true
}

func newTestLedgerPublisher(store *Store, states map[string]DecodeLedgerState) *decodeLedgerPublisher {
	return newDecodeLedgerPublisher(store, func(podKey string) DecodeLedgerState { return states[podKey] })
}

// A flush writes this gateway's absolute ledger values under its instance ID,
// with the charge times moved onto the Redis clock.
func TestDecodeLedgerPublisher_FlushWritesOwnFields(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	store.runningRequestsClockOffsetMillis.Store(2000) // Redis runs 2 s ahead of this process.
	states := map[string]DecodeLedgerState{testLedgerPodKey: {Tokens: 1500, Charges: 3, SumChargedAt: 300}}
	p := newTestLedgerPublisher(store, states)

	p.markDirty(testLedgerPodKey)
	p.flush()

	tokensKey, chargesKey, sumAtKey := decodeLedgerKeys(testLedgerPodKey)
	v, ok := ledgerField(t, client, tokensKey, runningRequestsGatewayInstanceID)
	require.True(t, ok)
	assert.Equal(t, "1500", v)
	v, ok = ledgerField(t, client, chargesKey, runningRequestsGatewayInstanceID)
	require.True(t, ok)
	assert.Equal(t, "3", v)
	v, ok = ledgerField(t, client, sumAtKey, runningRequestsGatewayInstanceID)
	require.True(t, ok)
	assert.Equal(t, "306", v, "each of the 3 charge times moves 2 s onto the Redis clock")
	ttl, err := client.PTTL(context.Background(), tokensKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
	assert.Contains(t, p.published, testLedgerPodKey)
	assert.Empty(t, p.dirty)

	// A later change overwrites the values rather than adding to them.
	states[testLedgerPodKey] = DecodeLedgerState{Tokens: 500, Charges: 1, SumChargedAt: 100}
	p.markDirty(testLedgerPodKey)
	p.flush()
	v, _ = ledgerField(t, client, tokensKey, runningRequestsGatewayInstanceID)
	assert.Equal(t, "500", v)
}

// Once the pod's ledger is empty, the gateway's fields are deleted and the pod
// is no longer republished.
func TestDecodeLedgerPublisher_EmptyLedgerDeletesFields(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	states := map[string]DecodeLedgerState{testLedgerPodKey: {Tokens: 100, Charges: 1, SumChargedAt: 10}}
	p := newTestLedgerPublisher(store, states)
	p.markDirty(testLedgerPodKey)
	p.flush()

	states[testLedgerPodKey] = DecodeLedgerState{}
	p.markDirty(testLedgerPodKey)
	p.flush()

	tokensKey, chargesKey, sumAtKey := decodeLedgerKeys(testLedgerPodKey)
	for _, key := range []string{tokensKey, chargesKey, sumAtKey} {
		_, ok := ledgerField(t, client, key, runningRequestsGatewayInstanceID)
		assert.Falsef(t, ok, "%s must not keep this gateway's field", key)
	}
	assert.NotContains(t, p.published, testLedgerPodKey)
}

// A failed write leaves the pod dirty, so the next flush writes it.
func TestDecodeLedgerPublisher_FailedWriteStaysDirty(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	store := &Store{redisClient: client}
	p := newTestLedgerPublisher(store, map[string]DecodeLedgerState{testLedgerPodKey: {Tokens: 100, Charges: 1, SumChargedAt: 10}})

	p.markDirty(testLedgerPodKey)
	p.flush()

	assert.Contains(t, p.dirty, testLedgerPodKey)
	assert.NotContains(t, p.published, testLedgerPodKey)
}

// PublishDecodeLedger's writer goroutine publishes a change without a flush
// call, and stops on close.
func TestPublishDecodeLedger_WritesInBackground(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	notify := store.PublishDecodeLedger(func(string) DecodeLedgerState {
		return DecodeLedgerState{Tokens: 42, Charges: 1, SumChargedAt: 1}
	})
	t.Cleanup(func() { store.decodeLedger.Load().close() })

	notify(testLedgerPodKey)

	tokensKey, _, _ := decodeLedgerKeys(testLedgerPodKey)
	require.Eventually(t, func() bool {
		v, ok := ledgerField(t, client, tokensKey, runningRequestsGatewayInstanceID)
		return ok && v == "42"
	}, 2*time.Second, 10*time.Millisecond)
}

// Without Redis the ledger cannot be shared: registration is a no-op and reads
// return the local running counts and no remote ledger.
func TestDecodeLedger_WithoutRedis(t *testing.T) {
	store := &Store{}
	pod := newTestPod(4)
	store.metaPods.Store(testLedgerPodKey, pod)

	assert.False(t, store.SharedDecodeLedgerAvailable())
	store.PublishDecodeLedger(func(string) DecodeLedgerState { return DecodeLedgerState{} })("ignored")
	assert.Nil(t, store.decodeLedger.Load())

	counts, ledger, err := store.GetPodsRunningRequestsAndDecodeLedger([]*v1.Pod{pod.Pod})
	require.NoError(t, err)
	assert.Nil(t, ledger)
	assert.EqualValues(t, 4, counts[testLedgerPodKey])
}

// The read sums the ledgers of the other live gateways only: this gateway's
// own fields (the caller has them in memory) and a dead gateway's fields are
// left out, and the dead ones are queued for pruning. The running-request
// counts come back from the same read.
func TestGetPodsRunningRequestsAndDecodeLedger_SumsOtherLiveGateways(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(2)
	store.metaPods.Store(testLedgerPodKey, pod)

	nowMillis := time.Now().UnixMilli()
	nowSeconds := float64(nowMillis) / 1000
	for _, gw := range []string{"gw-a", "gw-b"} {
		require.NoError(t, client.ZAdd(ctx, runningRequestsGatewaysKey, redis.Z{Score: float64(nowMillis), Member: gw}).Err())
	}
	require.NoError(t, client.HSet(ctx, runningRequestsKey(testNamespace, testPodName), "gw-a", "3").Err())

	tokensKey, chargesKey, sumAtKey := decodeLedgerKeys(testLedgerPodKey)
	set := func(key string, values map[string]string) {
		args := map[string]any{}
		for k, v := range values {
			args[k] = v
		}
		require.NoError(t, client.HSet(ctx, key, args).Err())
	}
	// gw-a: 2 charges made 10 s and 20 s ago; gw-b: 1 charge made 5 s ago.
	// gw-dead and this gateway's own fields must not count.
	sum := func(ages ...float64) string {
		s := 0.0
		for _, a := range ages {
			s += nowSeconds - a
		}
		return strconv.FormatFloat(s, 'g', -1, 64)
	}
	set(tokensKey, map[string]string{"gw-a": "1000", "gw-b": "300", "gw-dead": "9999", runningRequestsGatewayInstanceID: "777"})
	set(chargesKey, map[string]string{"gw-a": "2", "gw-b": "1", "gw-dead": "5", runningRequestsGatewayInstanceID: "1"})
	set(sumAtKey, map[string]string{"gw-a": sum(10, 20), "gw-b": sum(5), "gw-dead": sum(1, 1, 1, 1, 1), runningRequestsGatewayInstanceID: sum(1)})

	counts, ledger, err := store.GetPodsRunningRequestsAndDecodeLedger([]*v1.Pod{pod.Pod})
	require.NoError(t, err)
	assert.EqualValues(t, 5, counts[testLedgerPodKey], "gw-a's 3 plus this gateway's local 2")

	remote, ok := ledger[testLedgerPodKey]
	require.True(t, ok)
	assert.Equal(t, 1300.0, remote.Tokens)
	assert.InDelta(t, 35.0, remote.Elapsed, 1.0, "10 + 20 + 5 seconds of outstanding charges")

	for _, key := range []string{tokensKey, chargesKey, sumAtKey} {
		pending, ok := store.runningRequestsPendingPrunes.Load(key)
		require.Truef(t, ok, "%s: the dead gateway's field must be queued for pruning", key)
		assert.Equal(t, []string{"gw-dead"}, pending)
	}
}

// A pod that only this gateway has charged has no remote entry.
func TestGetPodsRunningRequestsAndDecodeLedger_OnlySelf(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(0)
	store.metaPods.Store(testLedgerPodKey, pod)

	tokensKey, chargesKey, sumAtKey := decodeLedgerKeys(testLedgerPodKey)
	require.NoError(t, client.HSet(ctx, tokensKey, runningRequestsGatewayInstanceID, "500").Err())
	require.NoError(t, client.HSet(ctx, chargesKey, runningRequestsGatewayInstanceID, "1").Err())
	require.NoError(t, client.HSet(ctx, sumAtKey, runningRequestsGatewayInstanceID, "1").Err())
	require.NoError(t, client.ZAdd(ctx, runningRequestsGatewaysKey,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: runningRequestsGatewayInstanceID}).Err())

	_, ledger, err := store.GetPodsRunningRequestsAndDecodeLedger([]*v1.Pod{pod.Pod})
	require.NoError(t, err)
	assert.NotNil(t, ledger)
	assert.NotContains(t, ledger, testLedgerPodKey)
}

// This gateway's own ledger fields are never queued for pruning, even when it
// is missing from the live set.
func TestGetPodsRunningRequestsAndDecodeLedger_NeverPrunesSelf(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(0)
	store.metaPods.Store(testLedgerPodKey, pod)

	tokensKey, chargesKey, sumAtKey := decodeLedgerKeys(testLedgerPodKey)
	for _, key := range []string{tokensKey, chargesKey, sumAtKey} {
		require.NoError(t, client.HSet(ctx, key, runningRequestsGatewayInstanceID, "1").Err())
	}

	_, _, err := store.GetPodsRunningRequestsAndDecodeLedger([]*v1.Pod{pod.Pod})
	require.NoError(t, err)
	for _, key := range []string{tokensKey, chargesKey, sumAtKey} {
		pending, ok := store.runningRequestsPendingPrunes.Load(key)
		assert.Falsef(t, ok && len(pending) > 0, "%s: own field queued for pruning: %v", key, pending)
	}
}

// Close stops the writer goroutine.
func TestStoreClose_StopsDecodeLedgerPublisher(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	store.PublishDecodeLedger(func(string) DecodeLedgerState { return DecodeLedgerState{} })
	p := store.decodeLedger.Load()
	require.NotNil(t, p)

	store.Close()

	select {
	case <-p.done:
	default:
		t.Fatal("Close must stop the decode ledger publisher")
	}
}

// A second registration replaces the first: the first registration's notify
// function stops queueing work, and the second one publishes.
func TestPublishDecodeLedger_LaterRegistrationReplacesEarlier(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	first := store.PublishDecodeLedger(func(string) DecodeLedgerState { return DecodeLedgerState{Tokens: 1, Charges: 1} })
	firstPublisher := store.decodeLedger.Load()
	second := store.PublishDecodeLedger(func(string) DecodeLedgerState { return DecodeLedgerState{Tokens: 2, Charges: 1} })
	t.Cleanup(func() { store.decodeLedger.Load().close() })

	first(testLedgerPodKey)
	firstPublisher.mu.Lock()
	assert.Empty(t, firstPublisher.dirty, "a replaced registration must not queue work")
	firstPublisher.mu.Unlock()

	second(testLedgerPodKey)
	tokensKey, _, _ := decodeLedgerKeys(testLedgerPodKey)
	require.Eventually(t, func() bool {
		v, ok := ledgerField(t, client, tokensKey, runningRequestsGatewayInstanceID)
		return ok && v == "2"
	}, 2*time.Second, 10*time.Millisecond)
}
