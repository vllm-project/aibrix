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

package client

import (
	"encoding/json"
	"testing"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
)

// TestAIBrixExtraBodyClientSerialization pins the cross-language contract: the
// JSON keys Go emits under aibrix.client must match the metadata-service's
// strict ClientConfig pydantic model, which rejects unknown fields.
func TestAIBrixExtraBodyClientSerialization(t *testing.T) {
	maxConc := int32(256)
	adaptive := true
	factor := 8.0
	retries := int32(5)
	baseDelay := 2.0

	eb := AIBrixExtraBody{
		Client: &plannerapi.ClientConfig{
			MaxConcurrency:      &maxConc,
			AdaptiveConcurrency: &adaptive,
			AdaptiveMaxFactor:   &factor,
			RetryPolicy: &plannerapi.ClientRetryPolicy{
				MaxRetries:       &retries,
				BaseDelaySeconds: &baseDelay,
			},
		},
	}

	raw, err := json.Marshal(eb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Client map[string]json.RawMessage `json:"client"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"max_concurrency", "adaptive_concurrency", "adaptive_max_factor", "retry_policy"} {
		if _, ok := decoded.Client[key]; !ok {
			t.Errorf("expected aibrix.client.%s in %s", key, raw)
		}
	}

	var retry map[string]json.RawMessage
	if err := json.Unmarshal(decoded.Client["retry_policy"], &retry); err != nil {
		t.Fatalf("unmarshal retry_policy: %v", err)
	}
	for _, key := range []string{"max_retries", "base_delay_seconds"} {
		if _, ok := retry[key]; !ok {
			t.Errorf("expected retry_policy.%s in %s", key, raw)
		}
	}
	// Unset fields must be omitted so MDS falls back to env defaults.
	if _, ok := retry["max_delay_seconds"]; ok {
		t.Errorf("expected unset retry_policy.max_delay_seconds to be omitted: %s", raw)
	}
}

// TestAIBrixExtraBodyClientOmittedWhenNil keeps the client block out of the
// payload entirely when no client settings are configured.
func TestAIBrixExtraBodyClientOmittedWhenNil(t *testing.T) {
	raw, err := json.Marshal(AIBrixExtraBody{JobID: "j1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["client"]; ok {
		t.Errorf("expected client to be omitted when nil: %s", raw)
	}
}
