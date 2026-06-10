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

package engine

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Snowflake-style disagg request ID constants for TensorRT-LLM PD routing.
// Layout: [timestamp(41b)][machineID(10b)][counter(12b)]
// The modulo rotation guarantees result >= trtMinGlobalID so TRT-LLM's executor
// treats it as a global (cross-worker) disagg ID rather than a local one.
const (
	trtCounterBits      = 12
	trtMachineIDBits    = 10
	trtTimestampBits    = 41
	trtSnowflakeEpochMs = int64(1672531200000) // 2023-01-01T00:00:00Z in milliseconds
	trtCounterMask      = (1 << trtCounterBits) - 1
	trtTimestampMax     = (1 << trtTimestampBits) - 1
	trtMinGlobalID      = int64(1) << 42
	trtMaxInt64         = int64(1<<63 - 1)
)

var (
	// trtMachineID is the 10-bit machine ID embedded in every snowflake disagg request ID.
	trtMachineID int64 = int64(utils.LoadEnvInt("AIBRIX_TRT_MACHINE_ID", 0))

	// globalDisaggCounter is a per-process monotonic counter for snowflake ID generation.
	globalDisaggCounter atomic.Int64

	// sonicJSONInt64 unmarshals JSON numbers as int64 to avoid float64 precision loss
	// on large fields such as disagg_request_id.
	sonicJSONInt64 = sonic.Config{UseInt64: true}.Froze()
)

func init() {
	if err := ValidateTRTMachineID(trtMachineID); err != nil {
		panic("routingalgorithms/engine: " + err.Error())
	}
	Register(&TRTLLMHandler{})
}

// ValidateTRTMachineID returns an error if machineID does not fit in trtMachineIDBits bits.
func ValidateTRTMachineID(machineID int64) error {
	maxExclusive := int64(1 << trtMachineIDBits)
	if machineID < 0 || machineID >= maxExclusive {
		return fmt.Errorf("invalid AIBRIX_TRT_MACHINE_ID=%d: must satisfy 0 <= id < %d (10-bit field)", machineID, maxExclusive)
	}
	return nil
}

// GetDisaggRequestID generates a snowflake-style ID shared between a prefill
// and its decode request so TRT-LLM can correlate the KV-cache entry.
func GetDisaggRequestID(machineID int64) int64 {
	timestampMs := time.Now().UnixMilli() - trtSnowflakeEpochMs
	if timestampMs < 0 {
		timestampMs = 0
	}
	if timestampMs > trtTimestampMax {
		timestampMs = trtTimestampMax
	}
	counter := (globalDisaggCounter.Add(1) - 1) & trtCounterMask
	globalID := (timestampMs << (trtMachineIDBits + trtCounterBits)) |
		(machineID << trtCounterBits) |
		counter
	return globalID%(trtMaxInt64-trtMinGlobalID) + trtMinGlobalID
}

// TRTLLMHandler implements EngineHandler for TensorRT-LLM.
type TRTLLMHandler struct{}

func (h *TRTLLMHandler) Name() string  { return "trtllm" }
func (h *TRTLLMHandler) IsAsync() bool { return false }

// AugmentPrefillRequest adds disaggregated_params with request_type="context_only"
// and a unique disagg_request_id generated from the machine snowflake ID.
func (h *TRTLLMHandler) AugmentPrefillRequest(
	_ *types.RoutingContext,
	_ *v1.Pod,
	completionRequest map[string]any,
) error {
	completionRequest["disaggregated_params"] = map[string]any{
		"request_type":      "context_only",
		"disagg_request_id": GetDisaggRequestID(trtMachineID),
	}
	return nil
}

// MergePrefillResponse injects TensorRT-LLM disaggregated_params from the
// prefill response into routingCtx.ReqBody so the decode worker can resume
// generation from the pre-filled KV cache.
func (h *TRTLLMHandler) MergePrefillResponse(
	routingCtx *types.RoutingContext,
	responseData map[string]any,
	prefillPod *v1.Pod,
) error {
	var originalRequest map[string]any
	if err := sonicJSONInt64.Unmarshal(routingCtx.ReqBody, &originalRequest); err != nil {
		return fmt.Errorf("failed to unmarshal original request body: %w", err)
	}
	if originalRequest == nil {
		return fmt.Errorf("original request body is empty or null")
	}

	// Locate disaggregated_params: top-level first, then choices[0] fallback.
	var disaggParams any
	var exists bool

	disaggParams, exists = responseData["disaggregated_params"]
	if !exists {
		if choices, ok := responseData["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				disaggParams, exists = choice["disaggregated_params"]
			}
		}
	}

	if !exists {
		klog.InfoS("no disaggregated_params in TRT prefill response", "request_id", routingCtx.RequestID)
		return nil
	}

	disaggParamsMap, ok := disaggParams.(map[string]any)
	if !ok {
		return fmt.Errorf("disaggregated_params has unexpected type %T, expected map[string]any", disaggParams)
	}

	disaggParamsMap["request_type"] = "generation_only"
	originalRequest["disaggregated_params"] = disaggParamsMap

	if pti, ok := responseData["prompt_token_ids"]; ok && pti != nil {
		if ids, ok := anySliceForJSON(pti); ok {
			switch routingCtx.ReqPath {
			case "/v1/completions":
				originalRequest["prompt"] = ids
			case "/v1/chat/completions":
				originalRequest["prompt_token_ids"] = ids
			}
		}
	}

	updatedReqBody, err := sonic.Marshal(originalRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal updated request body: %w", err)
	}
	routingCtx.ReqBody = updatedReqBody

	klog.InfoS("updated routing context with disaggregated_params (TensorRT-LLM)",
		"request_id", routingCtx.RequestID,
		"prefill_pod", prefillPod.Name,
		"prefill_host", prefillPod.Status.PodIP)
	return nil
}

// anySliceForJSON converts a JSON-decoded array into []any suitable for map[string]any marshaling.
func anySliceForJSON(v any) ([]any, bool) {
	if s, ok := v.([]any); ok {
		out := make([]any, len(s))
		copy(out, s)
		return out, true
	}
	val := reflect.ValueOf(v)
	if val.Kind() != reflect.Slice {
		return nil, false
	}
	out := make([]any, val.Len())
	for i := 0; i < val.Len(); i++ {
		out[i] = val.Index(i).Interface()
	}
	return out, true
}
