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
	"strings"
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
// packed into one field, with the charge times moved onto the Redis clock.
func TestDecodeLedgerPublisher_FlushWritesOwnField(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	store.runningRequestsClockOffsetMillis.Store(2000) // Redis runs 2 s ahead of this process.
	states := map[string]DecodeLedgerState{testLedgerPodKey: {Tokens: 1500, Charges: 3, SumChargedAt: 300}}
	p := newTestLedgerPublisher(store, states)

	p.markDirty(testLedgerPodKey)
	p.flush()

	key := decodeLedgerKey(testLedgerPodKey)
	v, ok := ledgerField(t, client, key, runningRequestsGatewayInstanceID)
	require.True(t, ok)
	assert.Equal(t, "1500,3,306", v, "each of the 3 charge times moves 2 s onto the Redis clock")
	ttl, err := client.PTTL(context.Background(), key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
	assert.Contains(t, p.published, testLedgerPodKey)
	assert.Empty(t, p.dirty)

	// A later change overwrites the value rather than adding to it.
	states[testLedgerPodKey] = DecodeLedgerState{Tokens: 500, Charges: 1, SumChargedAt: 100}
	p.markDirty(testLedgerPodKey)
	p.flush()
	v, _ = ledgerField(t, client, key, runningRequestsGatewayInstanceID)
	assert.Equal(t, "500,1,102", v)
}

// Once the pod's ledger is empty, the gateway's field is deleted and the pod
// is no longer republished.
func TestDecodeLedgerPublisher_EmptyLedgerDeletesField(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	states := map[string]DecodeLedgerState{testLedgerPodKey: {Tokens: 100, Charges: 1, SumChargedAt: 10}}
	p := newTestLedgerPublisher(store, states)
	p.markDirty(testLedgerPodKey)
	p.flush()

	states[testLedgerPodKey] = DecodeLedgerState{}
	p.markDirty(testLedgerPodKey)
	p.flush()

	_, ok := ledgerField(t, client, decodeLedgerKey(testLedgerPodKey), runningRequestsGatewayInstanceID)
	assert.False(t, ok)
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

	require.Eventually(t, func() bool {
		v, ok := ledgerField(t, client, decodeLedgerKey(testLedgerPodKey), runningRequestsGatewayInstanceID)
		return ok && v == "42,1,1"
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

func TestLedgerValueFormatAndParse(t *testing.T) {
	tokens, charges, sumAt, ok := parseLedgerValue(formatLedgerValue(1234.5, 3, 5.25e9))
	require.True(t, ok)
	assert.Equal(t, 1234.5, tokens)
	assert.EqualValues(t, 3, charges)
	assert.Equal(t, 5.25e9, sumAt)

	for _, bad := range []string{"", "1,2", "1,2,3,4", "x,1,1", "1,y,1", "1,1,z"} {
		_, _, _, ok := parseLedgerValue(bad)
		assert.Falsef(t, ok, "%q must not parse", bad)
	}
}

func setLedgerFields(t *testing.T, client *redis.Client, values map[string]string) {
	t.Helper()
	args := map[string]any{}
	for k, v := range values {
		args[k] = v
	}
	require.NoError(t, client.HSet(context.Background(), decodeLedgerKey(testLedgerPodKey), args).Err())
}

func markLive(t *testing.T, client *redis.Client, gateways ...string) {
	t.Helper()
	for _, gw := range gateways {
		require.NoError(t, client.ZAdd(context.Background(), runningRequestsGatewaysKey,
			redis.Z{Score: float64(time.Now().UnixMilli()), Member: gw}).Err())
	}
}

// The read sums the ledgers of the other live gateways only: this gateway's
// own field (the caller has it in memory) and a dead gateway's field are left
// out, and the dead one is queued for pruning. The running-request counts come
// back from the same read.
func TestGetPodsRunningRequestsAndDecodeLedger_SumsOtherLiveGateways(t *testing.T) {
	ctx := context.Background()
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(2)
	store.metaPods.Store(testLedgerPodKey, pod)
	markLive(t, client, "gw-a", "gw-b")
	require.NoError(t, client.HSet(ctx, runningRequestsKey(testNamespace, testPodName), "gw-a", "3").Err())

	nowSeconds := float64(time.Now().UnixMilli()) / 1000
	sum := func(ages ...float64) float64 {
		s := 0.0
		for _, a := range ages {
			s += nowSeconds - a
		}
		return s
	}
	// gw-a: 2 charges made 10 s and 20 s ago; gw-b: 1 charge made 5 s ago.
	setLedgerFields(t, client, map[string]string{
		"gw-a":                           formatLedgerValue(1000, 2, sum(10, 20)),
		"gw-b":                           formatLedgerValue(300, 1, sum(5)),
		"gw-dead":                        formatLedgerValue(9999, 5, sum(1, 1, 1, 1, 1)),
		runningRequestsGatewayInstanceID: formatLedgerValue(777, 1, sum(1)),
	})

	counts, ledger, err := store.GetPodsRunningRequestsAndDecodeLedger([]*v1.Pod{pod.Pod})
	require.NoError(t, err)
	assert.EqualValues(t, 5, counts[testLedgerPodKey], "gw-a's 3 plus this gateway's local 2")

	remote, ok := ledger[testLedgerPodKey]
	require.True(t, ok)
	assert.Equal(t, 1300.0, remote.Tokens)
	assert.InDelta(t, 35.0, remote.Elapsed, 1.0, "10 + 20 + 5 seconds of outstanding charges")

	pending, ok := store.runningRequestsPendingPrunes.Load(decodeLedgerKey(testLedgerPodKey))
	require.True(t, ok, "the dead gateway's field must be queued for pruning")
	assert.Equal(t, []string{"gw-dead"}, pending)
}

// A charge that costs 0 tokens is still decoding: the gateway holding it counts,
// with its elapsed time.
func TestGetPodsRunningRequestsAndDecodeLedger_ZeroTokenChargeCounts(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(0)
	store.metaPods.Store(testLedgerPodKey, pod)
	markLive(t, client, "gw-a")
	nowSeconds := float64(time.Now().UnixMilli()) / 1000
	setLedgerFields(t, client, map[string]string{"gw-a": formatLedgerValue(0, 1, nowSeconds-8)})

	_, ledger, err := store.GetPodsRunningRequestsAndDecodeLedger([]*v1.Pod{pod.Pod})
	require.NoError(t, err)
	remote, ok := ledger[testLedgerPodKey]
	require.True(t, ok, "a 0-token charge must not drop the gateway")
	assert.Zero(t, remote.Tokens)
	assert.InDelta(t, 8.0, remote.Elapsed, 1.0)
}

// A pod that only this gateway has charged has no remote entry.
func TestGetPodsRunningRequestsAndDecodeLedger_OnlySelf(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(0)
	store.metaPods.Store(testLedgerPodKey, pod)
	markLive(t, client, runningRequestsGatewayInstanceID)
	setLedgerFields(t, client, map[string]string{runningRequestsGatewayInstanceID: formatLedgerValue(500, 1, 1)})

	_, ledger, err := store.GetPodsRunningRequestsAndDecodeLedger([]*v1.Pod{pod.Pod})
	require.NoError(t, err)
	assert.NotNil(t, ledger)
	assert.NotContains(t, ledger, testLedgerPodKey)
}

// This gateway's own field is never queued for pruning, even when it is
// missing from the live set.
func TestGetPodsRunningRequestsAndDecodeLedger_NeverPrunesSelf(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	pod := newTestPod(0)
	store.metaPods.Store(testLedgerPodKey, pod)
	setLedgerFields(t, client, map[string]string{runningRequestsGatewayInstanceID: formatLedgerValue(1, 1, 1)})

	_, _, err := store.GetPodsRunningRequestsAndDecodeLedger([]*v1.Pod{pod.Pod})
	require.NoError(t, err)
	pending, ok := store.runningRequestsPendingPrunes.Load(decodeLedgerKey(testLedgerPodKey))
	assert.Falsef(t, ok && len(pending) > 0, "own field queued for pruning: %v", pending)
}

// Close stops the writer goroutine and returns only after it has.
func TestStoreClose_StopsDecodeLedgerPublisher(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	store.PublishDecodeLedger(func(string) DecodeLedgerState { return DecodeLedgerState{} })
	p := store.decodeLedger.Load()
	require.NotNil(t, p)

	store.Close()

	select {
	case <-p.stopped:
	default:
		t.Fatal("Close must wait for the decode ledger writer to return")
	}
}

// A second registration replaces the first: the first registration's writer has
// stopped before the second starts, its notify function stops queueing work,
// and the second one publishes.
func TestPublishDecodeLedger_LaterRegistrationReplacesEarlier(t *testing.T) {
	client := newTestRunningRequestsClient(t)
	store := &Store{redisClient: client}
	first := store.PublishDecodeLedger(func(string) DecodeLedgerState { return DecodeLedgerState{Tokens: 1, Charges: 1} })
	firstPublisher := store.decodeLedger.Load()
	second := store.PublishDecodeLedger(func(string) DecodeLedgerState { return DecodeLedgerState{Tokens: 2, Charges: 1} })
	t.Cleanup(func() { store.decodeLedger.Load().close() })

	select {
	case <-firstPublisher.stopped:
	default:
		t.Fatal("the replaced writer must have returned before the replacement starts")
	}
	first(testLedgerPodKey)
	firstPublisher.mu.Lock()
	assert.Empty(t, firstPublisher.dirty, "a replaced registration must not queue work")
	firstPublisher.mu.Unlock()

	second(testLedgerPodKey)
	require.Eventually(t, func() bool {
		v, ok := ledgerField(t, client, decodeLedgerKey(testLedgerPodKey), runningRequestsGatewayInstanceID)
		return ok && strings.HasPrefix(v, "2,1,")
	}, 2*time.Second, 10*time.Millisecond)
}
