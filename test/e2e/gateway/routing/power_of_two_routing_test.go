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

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	powerOfTwoStrategy = "power-of-two"

	// power-of-two compares the running-request counts the gateway keeps in Redis for every
	// routed request, shared by all gateway replicas (pkg/cache/cache_running_requests.go): one
	// hash per pod, aibrix:rrq:<namespace>/<pod>, with a count per gateway instance, and a sorted
	// set of the gateway instances that heartbeat within the last few seconds. A gateway's count
	// only contributes to a pod's total while that gateway is in the set. The tests that read or
	// write these keys depend on that layout and must follow it if it changes.
	runningRequestsPrefix      = "aibrix:rrq"
	runningRequestsGatewaysKey = runningRequestsPrefix + ":gateways"
	// fakeGatewayHeartbeat is how often the busy-pod test's stand-in gateway heartbeats. A
	// gateway counts as live for a few seconds after its last heartbeat, so this keeps it live.
	fakeGatewayHeartbeat = 500 * time.Millisecond

	po2DrainTimeout = 15 * time.Second
)

// powerOfTwoChat sends one non-streaming chat completion with routing-strategy power-of-two
// and returns the pod that served it. It does not touch *testing.T so goroutines can use it.
func powerOfTwoChat(message string) (string, error) {
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, powerOfTwoStrategy, option.WithResponseInto(&dst))
	if _, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(message)},
		Model:    modelName,
	}); err != nil {
		return "", fmt.Errorf("chat completion: %w", err)
	}
	if got := dst.Header.Get("routing-strategy"); got != powerOfTwoStrategy {
		return "", fmt.Errorf("routing-strategy response header is %q, want %q", got, powerOfTwoStrategy)
	}
	pod := dst.Header.Get("target-pod")
	if pod == "" {
		return "", fmt.Errorf("response has no target-pod header")
	}
	return pod, nil
}

// powerOfTwoStreamingChat sends one streaming chat completion with routing-strategy
// power-of-two and reads it to the end. Streaming completes through a different gateway path
// than a plain response, so it has to be exercised separately.
func powerOfTwoStreamingChat(message string) error {
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, powerOfTwoStrategy, option.WithResponseInto(&dst))
	stream := client.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(message)},
		Model:    modelName,
	})
	defer func() { _ = stream.Close() }()

	chunks := 0
	for stream.Next() {
		chunks++
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("streaming chat completion: %w", err)
	}
	if chunks == 0 {
		return fmt.Errorf("streaming chat completion returned no chunks")
	}
	return nil
}

func requirePowerOfTwoRedis(t *testing.T) {
	t.Helper()
	if redisClient == nil {
		t.Skip("Skipping: Redis client is not available")
	}
}

func scanKeys(ctx context.Context, pattern string) ([]string, error) {
	var keys []string
	iter := redisClient.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

// podCounterKeys returns the running-request hash of each of pods, named as the gateway names it
// (which includes the pod's namespace). These counters are shared with every test that sends
// traffic through the gateway, so the tests here only ever read or delete the hashes of the pods
// serving their own model, never all of them.
func podCounterKeys(ctx context.Context, pods []string) ([]string, error) {
	var keys []string
	for _, pod := range pods {
		podKeys, err := scanKeys(ctx, fmt.Sprintf("%s:*/%s", runningRequestsPrefix, pod))
		if err != nil {
			return nil, err
		}
		keys = append(keys, podKeys...)
	}
	return keys, nil
}

// runningRequestCounts returns, for the running-request hash of each of pods, each gateway
// instance's count for that pod.
func runningRequestCounts(ctx context.Context, pods []string) (map[string]map[string]int64, error) {
	keys, err := podCounterKeys(ctx, pods)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]map[string]int64, len(keys))
	if len(keys) == 0 {
		return counts, nil
	}
	pipe := redisClient.Pipeline()
	cmds := make(map[string]*redis.MapStringStringCmd, len(keys))
	for _, key := range keys {
		cmds[key] = pipe.HGetAll(ctx, key)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for key, cmd := range cmds {
		fields, err := cmd.Result()
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			continue // expired between the scan and the read
		}
		counts[key] = make(map[string]int64, len(fields))
		for gateway, value := range fields {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s field %s holds %q: %w", key, gateway, value, err)
			}
			counts[key][gateway] = n
		}
	}
	return counts, nil
}

// awaitDrained polls until every running-request count of pods is zero, or timeout passes, and
// returns the last counts it read and whether they had drained. A negative count is as much a bug
// as a positive one (a decrement with no matching increment), so only zero counts as drained.
func awaitDrained(
	ctx context.Context, pods []string, timeout time.Duration,
) (counts map[string]map[string]int64, drained bool, err error) {
	deadline := time.Now().Add(timeout)
	for {
		counts, err := runningRequestCounts(ctx, pods)
		if err != nil {
			return nil, false, err
		}
		if allZero(counts) {
			return counts, true, nil
		}
		if time.Now().After(deadline) {
			return counts, false, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func deleteRunningRequestCounters(t *testing.T, pods []string) {
	t.Helper()
	ctx := context.Background()
	keys, err := podCounterKeys(ctx, pods)
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, redisClient.Del(ctx, keys...).Err())
	}
}

// discoverModelPods returns the sorted names of the pods serving the test model, found with
// random routing so power-of-two's own behavior is not involved. Those requests are still
// counted like any others, which also leaves each pod's running-request hash in place.
func discoverModelPods(t *testing.T) []string {
	t.Helper()
	discovered := make(map[string]struct{})
	for i := 0; i < 30; i++ {
		if pod := getTargetPodFromChatCompletion(t, fmt.Sprintf("Pod discovery request %d", i), "random"); pod != "" {
			discovered[pod] = struct{}{}
		}
	}
	pods := make([]string, 0, len(discovered))
	for pod := range discovered {
		pods = append(pods, pod)
	}
	sort.Strings(pods)
	require.GreaterOrEqual(t, len(pods), 2, "need at least two pods to compare, found %v", pods)
	return pods
}

// impersonateLiveGateway registers instanceID as a live gateway instance, by heartbeating it
// into the gateway set on Redis's own clock (which is the clock the gateways compare against),
// until the test ends. Only a live gateway's counts contribute to a pod's total.
func impersonateLiveGateway(t *testing.T, instanceID string) {
	t.Helper()
	ctx := context.Background()
	beat := func() error {
		now, err := redisClient.Time(ctx).Result()
		if err != nil {
			return err
		}
		return redisClient.ZAdd(ctx, runningRequestsGatewaysKey,
			redis.Z{Score: float64(now.UnixMilli()), Member: instanceID}).Err()
	}

	stop, stopped := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-stopped
		_ = redisClient.ZRem(ctx, runningRequestsGatewaysKey, instanceID).Err()
	})
	require.NoError(t, beat())
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(fakeGatewayHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = beat()
			case <-stop:
				return
			}
		}
	}()
}

// TestPowerOfTwoRouting checks that the gateway serves the power-of-two strategy at all
// (an unregistered strategy is rejected with a 400) and that it spreads requests over pods.
func TestPowerOfTwoRouting(t *testing.T) {
	const requests = 60
	tally := make(map[string]int)
	for i := 0; i < requests; i++ {
		pod, err := powerOfTwoChat(fmt.Sprintf("power-of-two routing request %d", i))
		require.NoError(t, err, "request %d", i)
		tally[pod]++
	}

	// Sequential requests keep every pod near idle, so power-of-two behaves like random
	// choice here; a router that pinned to one pod would show a single entry.
	assert.GreaterOrEqual(t, len(tally), 2, "power-of-two should use more than one pod, distribution: %v", tally)
}

// TestPowerOfTwoRoutingAvoidsBusyPod makes one pod look heavily loaded to the gateway by
// impersonating a second, live gateway that reports 1000 running requests on it, then checks
// that power-of-two never chooses that pod: a pod with a higher count always loses the
// comparison, and the busy pod is always outranked by whichever pod it is paired with. It thus
// also checks that routing sees load from other gateways, not just its own.
func TestPowerOfTwoRoutingAvoidsBusyPod(t *testing.T) {
	requirePowerOfTwoRedis(t)
	ctx := context.Background()

	pods := discoverModelPods(t)
	busy := pods[0]
	t.Logf("pods: %v, marking %s as busy", pods, busy)

	// The hash is named after the pod's namespace too, so look it up rather than assume one.
	var busyKey string
	require.Eventually(t, func() bool {
		keys, err := scanKeys(ctx, fmt.Sprintf("%s:*/%s", runningRequestsPrefix, busy))
		if err != nil || len(keys) != 1 {
			return false
		}
		busyKey = keys[0]
		return true
	}, 5*time.Second, 100*time.Millisecond, "no running-request counter for pod %s", busy)

	fakeGateway := fmt.Sprintf("e2e-busy-gateway-%d", time.Now().UnixNano())
	impersonateLiveGateway(t, fakeGateway)
	t.Cleanup(func() { _ = redisClient.HDel(context.Background(), busyKey, fakeGateway).Err() })
	require.NoError(t, redisClient.HSet(ctx, busyKey, fakeGateway, 1000).Err())
	require.NoError(t, redisClient.PExpire(ctx, busyKey, 10*time.Minute).Err())

	const requests = 45
	tally := make(map[string]int)
	for i := 0; i < requests; i++ {
		pod, err := powerOfTwoChat(fmt.Sprintf("busy pod avoidance request %d", i))
		require.NoError(t, err, "request %d", i)
		tally[pod]++
	}

	assert.Zero(t, tally[busy],
		"pod %s carries 1000 running requests from another gateway and must never win a comparison, "+
			"distribution: %v", busy, tally)
}

// TestPowerOfTwoRoutingDrainsCounters checks that every request the gateway counts is also
// uncounted when it finishes -- plain, streamed, and concurrent -- so no count is left skewing
// later routing decisions.
func TestPowerOfTwoRoutingDrainsCounters(t *testing.T) {
	requirePowerOfTwoRedis(t)
	ctx := context.Background()

	pods := discoverModelPods(t)

	// Let discovery's own decrements land first, so they cannot hit the hashes recreated below.
	_, _, err := awaitDrained(ctx, pods, 5*time.Second)
	require.NoError(t, err)

	// Start from an empty slate so what is left afterwards is this test's own traffic. Only this
	// model's pods are cleared: tests that use other models (replica inflight and RPS limits) run on
	// other pods, so this holds whether or not packages run serially, and nothing else in this
	// package runs alongside. A gateway reads a missing hash as "not counted yet" and falls back to
	// its own count, so clearing is safe.
	deleteRunningRequestCounters(t, pods)

	for i := 0; i < 15; i++ {
		_, err := powerOfTwoChat(fmt.Sprintf("drain sequential request %d", i))
		require.NoError(t, err)
	}
	for i := 0; i < 10; i++ {
		require.NoError(t, powerOfTwoStreamingChat(fmt.Sprintf("drain streaming request %d", i)))
	}

	const concurrent = 12
	var wg sync.WaitGroup
	errs := make(chan error, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = powerOfTwoChat(fmt.Sprintf("drain concurrent request %d", i))
			} else {
				err = powerOfTwoStreamingChat(fmt.Sprintf("drain concurrent request %d", i))
			}
			if err != nil {
				errs <- fmt.Errorf("concurrent request %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The hashes outlive the requests (they only expire after an idle period), so their
	// presence shows the gateway really counted this test's traffic; otherwise "all zero"
	// would prove nothing.
	require.Eventually(t, func() bool {
		counts, err := runningRequestCounts(ctx, pods)
		return err == nil && len(counts) > 0
	}, 5*time.Second, 100*time.Millisecond,
		"no running-request hash exists for pods %v after the traffic: the gateway is not counting requests", pods)

	// Completion is reported after the response, so allow a moment for the last decrements.
	counts, drained, err := awaitDrained(ctx, pods, po2DrainTimeout)
	require.NoError(t, err)
	require.True(t, drained, "counts did not drain to zero within %s, so requests were counted but not uncounted "+
		"(or the reverse): %v", po2DrainTimeout, counts)
}

func allZero(counts map[string]map[string]int64) bool {
	for _, gateways := range counts {
		for _, n := range gateways {
			if n != 0 {
				return false
			}
		}
	}
	return true
}
