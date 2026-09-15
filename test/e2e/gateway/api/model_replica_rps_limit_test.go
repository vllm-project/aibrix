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
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"
)

// TestModelReplicaRPSLimit verifies that the requestsPerSecondPerReplica field set on
// qwen3-8b-replica-rps's model.aibrix.ai/config profile (see
// development/app/config/mock/replica-limits.yaml) is enforced by the gateway. Unlike
// TestModelRPSLimit (which exercises the flat, aggregate requestsPerSecond field), this
// exercises the per-replica path: applyConfigProfile multiplies requestsPerSecondPerReplica
// (set to 1) by the model's routable replica count (1) to derive the same effective limit of
// 1 request/second, and forces routing to least-request so the cap is actually enforced end
// to end (see selectTargetPod).
func TestModelReplicaRPSLimit(t *testing.T) {
	msg := "replica rps limit test message"

	// waitForFreshWindow sleeps until the current 1-second Redis window expires,
	// ensuring the counter is at zero at the start of each sub-test.
	waitForFreshWindow := func() {
		nextWindow := time.Now().Truncate(time.Second).Add(time.Second + 50*time.Millisecond)
		time.Sleep(time.Until(nextWindow))
	}

	sendRequest := func() error {
		client := createOpenAIClient(gatewayURL, apiKey)
		_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages:  []openai.ChatCompletionMessageParamUnion{openai.UserMessage(msg)},
			Model:     modelNameQwen3ReplicaRPS,
			MaxTokens: openai.Int(1),
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

	requireOneAllowedOneRejected := func(t *testing.T, errs []error) {
		t.Helper()
		require.Len(t, errs, 2)

		var allowed, rejected int
		for _, err := range errs {
			if err == nil {
				allowed++
				continue
			}

			var apiErr *openai.Error
			require.True(t, errors.As(err, &apiErr), "error should be an openai API error, got: %v", err)
			require.Equal(t, 429, apiErr.StatusCode, "exceeded replica RPS limit should return HTTP 429")
			rejected++
		}

		require.Equal(t, 1, allowed, "exactly one request should be allowed within the 1 RPS window")
		require.Equal(t, 1, rejected, "exactly one request should be rejected within the 1 RPS window")
	}

	t.Run("first_request_succeeds", func(t *testing.T) {
		waitForFreshWindow()

		err := sendRequest()
		require.NoError(t, err, "first request within the replica RPS limit should succeed")
	})

	t.Run("second_request_in_same_window_is_rejected", func(t *testing.T) {
		waitForFreshWindow()

		// Start both requests together so both reach the gateway in the same
		// fixed window regardless of backend completion latency.
		errs := sendConcurrentRequests(2)
		requireOneAllowedOneRejected(t, errs)
	})

	t.Run("requests_succeed_after_window_resets", func(t *testing.T) {
		waitForFreshWindow()

		// Exhaust the window with a concurrent burst.
		errs := sendConcurrentRequests(2)
		requireOneAllowedOneRejected(t, errs)

		// Wait for the window to roll over, then verify requests succeed again.
		waitForFreshWindow()

		err := sendRequest()
		require.NoError(t, err, "request in the next window should succeed after the counter resets")
	})
}
