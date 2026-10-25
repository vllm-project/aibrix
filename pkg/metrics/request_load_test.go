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

package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFetchSingleModelRequestLoad(t *testing.T) {
	zero := "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running{model_name=\"m\"} 0\n# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"m\"} 0\n"
	cases := []struct {
		name, engine, body string
		code               int
		want               float64
		bad                bool
	}{
		{"idle", "vllm", zero, 200, 0, false},
		{"waiting", "vllm", strings.Replace(zero, "waiting{model_name=\"m\"} 0", "waiting{model_name=\"m\"} 4", 1), 200, 4, false},
		{"missing", "vllm", "", 200, 0, true},
		{"http-error", "vllm", zero, 500, 0, true},
		{"duplicate-worker", "vllm", zero + "vllm:num_requests_running{model_name=\"m\",worker=\"second\"} 5\n", 200, 0, true},
		{"nan", "vllm", strings.Replace(zero, "} 0", "} NaN", 1), 200, 0, true},
		{"infinite", "vllm", strings.Replace(zero, "} 0", "} +Inf", 1), 200, 0, true},
		{"negative", "vllm", strings.Replace(zero, "} 0", "} -1", 1), 200, 0, true},
		{"mismatch", "vllm", strings.Replace(zero, "waiting{model_name=\"m\"}", "waiting{model_name=\"another\"}", 1), 200, 0, true},
		{"no-model", "vllm", strings.ReplaceAll(zero, "{model_name=\"m\"}", ""), 200, 0, true},
		{"unsupported-engine", "unknown", zero, 200, 0, true},
		{"wrong-type", "vllm", strings.Replace(zero, "running gauge", "running counter", 1), 200, 0, true},
		{"sglang-total", "sglang", "# TYPE sglang:num_running_reqs gauge\nsglang:num_running_reqs{model_name=\"m\",priority=\"\"} 3\nsglang:num_running_reqs{model_name=\"m\",priority=\"high\"} 3\n# TYPE sglang:num_queue_reqs gauge\nsglang:num_queue_reqs{model_name=\"m\",priority=\"\"} 2\n", 200, 5, false},
		{"priority-without-total", "sglang", "# TYPE sglang:num_running_reqs gauge\nsglang:num_running_reqs{model_name=\"m\",priority=\"high\"} 0\n# TYPE sglang:num_queue_reqs gauge\nsglang:num_queue_reqs{model_name=\"m\",priority=\"high\"} 0\n", 200, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.code)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			fetcher := NewEngineMetricsFetcherWithConfig(EngineMetricsFetcherConfig{Timeout: time.Second})
			load, err := fetcher.FetchSingleModelRequestLoad(context.Background(), strings.TrimPrefix(srv.URL, "http://"), tc.engine)
			if tc.bad {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, load)
			}
			require.EqualValues(t, 1, calls.Load(), "both gauges come from one response, without retries")
		})
	}
}
