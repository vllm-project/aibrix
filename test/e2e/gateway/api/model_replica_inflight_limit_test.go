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
	"errors"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"
)

// TestModelReplicaRequestsInflightLimit verifies that the requestsInflight field set on
// qwen3-8b-replica-inflight's model.aibrix.ai/config profile (cap of 1, see
// development/app/config/mock/replica-limits.yaml) is enforced as a real per-replica
// concurrency cap by the gateway: pods already at the cap are filtered out of the candidate
// pool before routing (filterSaturatedReplicaInflight), and the finally-selected pod is
// re-checked after routing (enforceReplicaInflight). Unlike requestsPerSecondPerReplica, this
// limits concurrent requests, not requests-per-second, so it is exercised with overlapping
// in-flight requests rather than a fixed time window.
func TestModelReplicaRequestsInflightLimit(t *testing.T) {
	msg := "replica inflight limit test message"

	// A large MaxTokens keeps the mock backend's simulated decode latency (see
	// MOCK_DECODE_SECONDS_PER_TOKEN in development/app/app.py) long enough that two
	// concurrently-issued requests are guaranteed to genuinely overlap in flight, rather
	// than racing to complete before the second one is even sent.
	sendRequest := func() error {
		client := createOpenAIClient(gatewayURL, apiKey)
		_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages:  []openai.ChatCompletionMessageParamUnion{openai.UserMessage(msg)},
			Model:     modelNameQwen3ReplicaInflight,
			MaxTokens: openai.Int(400),
		})
		return err
	}

	sendConcurrentRequests := func(count int) []error {
		errs := make([]error, count)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(count)
		for i := range count {
			go func(index int) {
				defer wg.Done()
				<-start
				errs[index] = sendRequest()
			}(i)
		}
		close(start)
		wg.Wait()
		return errs
	}

	t.Run("single_request_succeeds", func(t *testing.T) {
		err := sendRequest()
		require.NoError(t, err, "a single request within the inflight cap should succeed")
	})

	t.Run("concurrent_requests_beyond_cap_are_rejected", func(t *testing.T) {
		errs := sendConcurrentRequests(2)
		require.Len(t, errs, 2)

		var allowed, rejected int
		for _, err := range errs {
			if err == nil {
				allowed++
				continue
			}

			var apiErr *openai.Error
			require.True(t, errors.As(err, &apiErr), "error should be an openai API error, got: %v", err)
			require.Equal(t, 429, apiErr.StatusCode, "exceeding the replica inflight cap should return HTTP 429")
			rejected++
		}

		require.Equal(t, 1, allowed, "exactly one concurrent request should be admitted under the inflight cap of 1")
		require.Equal(t, 1, rejected, "exactly one concurrent request should be rejected by the inflight cap")
	})

	t.Run("requests_succeed_again_once_the_prior_one_completes", func(t *testing.T) {
		// Sequential requests never overlap, so each one sees the replica idle again by
		// the time it is admitted.
		for i := 0; i < 2; i++ {
			err := sendRequest()
			require.NoError(t, err, "sequential request %d should succeed once the prior one has completed", i+1)
		}
	})
}
