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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The shared Gateway route uses Streamed response processing even for a
// non-streaming completion. A backend 400 must survive that filter chain as
// a complete JSON body, rather than a late local reply resetting the response.
// Deterministic chunk boundaries are covered by the gateway unit tests.
func TestUpstreamErrorBodySurvivesStreamedProcessing(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			body := fmt.Sprintf(`{
 "model":%q,
 "messages":[{"role":"user","content":"reject at the backend"}],
 "max_tokens":8,"stream":%t
}`, modelName, stream)
			req, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/chat/completions", bytes.NewBufferString(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("routing-strategy", "random")
			req.Header.Set("x-aibrix-mock-delay-ms", "not-a-number")

			client := &http.Client{Timeout: 30 * time.Second}
			defer client.CloseIdleConnections()
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			payload, err := io.ReadAll(resp.Body)
			require.NoError(t, err, "the gateway must not reset or truncate the upstream error")
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, "response: %s", payload)
			require.Contains(t, resp.Header.Get("Content-Type"), "application/json")

			var result struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(payload, &result), "response: %s", payload)
			require.Equal(t, "invalid x-aibrix-mock-delay-ms", result.Error.Message)
			require.Equal(t, "invalid_request_error", result.Error.Type)
		})
	}
}
