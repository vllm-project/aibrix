/*
Copyright 2025 The Aibrix Team.

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
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelRPSLimit verifies that the per-model requestsPerSecond limit defined in the
// model.aibrix.ai/config annotation is enforced by the gateway across all instances.
//
// The "rps-limited" profile on qwen3-8b is configured with requestsPerSecond: 1,
// meaning only 1 request per second is allowed. This is defined in
// development/app/config/mock/config-profile.yaml.
func TestModelRPSLimit(t *testing.T) {
	msg := "rps limit test message"

	// waitForFreshWindow sleeps until the current 1-second Redis window expires,
	// ensuring the counter is at zero at the start of each sub-test.
	waitForFreshWindow := func() {
		nextWindow := time.Now().Truncate(time.Second).Add(time.Second + 50*time.Millisecond)
		time.Sleep(time.Until(nextWindow))
	}

	sendRequest := func(t *testing.T, profile string) error {
		t.Helper()
		client := createOpenAIClientWithConfigProfile(gatewayURL, apiKey, profile, nil)
		_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(msg)},
			Model:    modelNameQwen3,
		})
		return err
	}

	t.Run("first_request_succeeds", func(t *testing.T) {
		waitForFreshWindow()

		err := sendRequest(t, "rps-limited")
		require.NoError(t, err, "first request within the RPS limit should succeed")
	})

	t.Run("second_request_in_same_window_is_rejected", func(t *testing.T) {
		waitForFreshWindow()

		// First request exhausts the 1 RPS budget.
		err := sendRequest(t, "rps-limited")
		require.NoError(t, err, "first request should succeed")

		// Second request in the same 1-second window must be rejected.
		err = sendRequest(t, "rps-limited")
		require.Error(t, err, "second request should be rejected after RPS limit is exhausted")

		var apiErr *openai.Error
		require.True(t, errors.As(err, &apiErr), "error should be an openai API error, got: %v", err)
		assert.Equal(t, 429, apiErr.StatusCode, "exceeded RPS limit should return HTTP 429")
	})

	t.Run("requests_succeed_after_window_resets", func(t *testing.T) {
		waitForFreshWindow()

		// Exhaust the window.
		err := sendRequest(t, "rps-limited")
		require.NoError(t, err, "first request should succeed")

		err = sendRequest(t, "rps-limited")
		require.Error(t, err, "second request should be throttled")

		// Wait for the window to roll over, then verify requests succeed again.
		waitForFreshWindow()

		err = sendRequest(t, "rps-limited")
		require.NoError(t, err, "request in the next window should succeed after the counter resets")
	})

	t.Run("no_rps_limit_without_rps_profile", func(t *testing.T) {
		// The default profile has no requestsPerSecond, so multiple rapid requests
		// to the same model must all succeed regardless of how many are sent.
		for i := 0; i < 3; i++ {
			err := sendRequest(t, "least-request")
			require.NoError(t, err, "request %d without RPS profile should succeed", i+1)
		}
	})
}
