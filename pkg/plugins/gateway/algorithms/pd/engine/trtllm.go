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
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Snowflake-style disagg request ID constants for TensorRT-LLM PD routing.
// Layout: [timestamp(41b)][machineID(10b)][counter(12b)]
// The modulo rotation guarantees result >= TRTMinGlobalID so TRT-LLM's executor
// treats it as a global (cross-worker) disagg ID rather than a local one.
const (
	// TRTMachineIDBits is the width of the machine-ID field in snowflake disagg request IDs.
	TRTMachineIDBits = 10
	// TRTMinGlobalID is the minimum global disagg ID; values below this are treated as local by TRT-LLM.
	TRTMinGlobalID = int64(1) << 42

	trtCounterBits      = 12
	trtTimestampBits    = 41
	trtSnowflakeEpochMs = int64(1672531200000) // 2023-01-01T00:00:00Z in milliseconds
	trtCounterMask      = (1 << trtCounterBits) - 1
	trtTimestampMax     = (1 << trtTimestampBits) - 1
	trtMaxInt64         = int64(1<<63 - 1)
)

var (
	// trtMachineID is the 10-bit machine ID embedded in every snowflake disagg request ID.
	trtMachineID int64 = int64(utils.LoadEnvInt("AIBRIX_TRT_MACHINE_ID", 0))

	// globalDisaggCounter is a per-process monotonic counter for snowflake ID generation.
	globalDisaggCounter atomic.Int64
)

func init() {
	if err := ValidateTRTMachineID(trtMachineID); err != nil {
		panic("routingalgorithms/engine: " + err.Error())
	}
	Register(&TRTLLMHandler{})
}

// ValidateTRTMachineID returns an error if machineID does not fit in TRTMachineIDBits bits.
func ValidateTRTMachineID(machineID int64) error {
	maxExclusive := int64(1 << TRTMachineIDBits)
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
	globalID := (timestampMs << (TRTMachineIDBits + trtCounterBits)) |
		(machineID << trtCounterBits) |
		counter
	return globalID%(trtMaxInt64-TRTMinGlobalID) + TRTMinGlobalID
}

const (
	TRTContextFirst    = "context_first"
	TRTGenerationFirst = "generation_first"
	// TRT-LLM's DisaggScheduleStyle is an IntEnum on the HTTP wire.
	trtGenerationFirstSchedule = 1
)

// TRTLLMHandler implements EngineHandler for TensorRT-LLM. Configuration is
// immutable and router-scoped; the zero value retains context-first behavior.
type TRTLLMHandler struct {
	generationFirst bool
	serverInfo      TRTServerInfoProvider
}

// NewTRTLLMHandler validates the configured schedule before any request is sent.
// The registry's default handler is never mutated by a router's configuration.
func NewTRTLLMHandler(scheduleStyle string, serverInfo TRTServerInfoProvider) (*TRTLLMHandler, error) {
	switch scheduleStyle {
	case "", TRTContextFirst:
		return &TRTLLMHandler{}, nil
	case TRTGenerationFirst:
		if serverInfo == nil {
			return nil, fmt.Errorf("TRT generation_first requires a server_info provider")
		}
		return &TRTLLMHandler{generationFirst: true, serverInfo: serverInfo}, nil
	default:
		return nil, fmt.Errorf("invalid AIBRIX_TRT_SCHEDULE_STYLE=%q: expected context_first or generation_first", scheduleStyle)
	}
}

func (h *TRTLLMHandler) Name() string  { return pd.EngineTRTLLM }
func (h *TRTLLMHandler) IsAsync() bool { return h.generationFirst }

// trtControlledFields are the top-level keys AugmentPrefillRequest and
// MergePrefillResponse write: disaggregated_params on both bodies, and
// prompt / prompt_token_ids on the decode body when the prefill response
// carries prompt_token_ids.
var trtControlledFields = []string{"disaggregated_params", "prompt", "prompt_token_ids"}

func (h *TRTLLMHandler) ControlledFields() []string { return trtControlledFields }

// AugmentPrefillRequest replaces disaggregated_params with request_type="context_only"
// and a unique disagg_request_id generated from the machine snowflake ID, in a
// single top-level edit. The ID is written as an integer literal, so it never
// goes through float64 and keeps full int64 precision.
func (h *TRTLLMHandler) AugmentPrefillRequest(
	routingCtx *types.RoutingContext,
	pod *v1.Pod,
	body []byte,
) ([]byte, error) {
	if h.generationFirst {
		info, err := h.serverInfo.Get(routingCtx.Context, pod)
		if err != nil {
			return nil, fmt.Errorf("prepare TRT generation_first: %w", err)
		}
		prefillBody, decodeBody, err := prepareTRTGenerationFirst(body, info, GetDisaggRequestID(trtMachineID))
		if err != nil {
			return nil, err
		}
		// Commit only after both bodies are ready, before either leg can run.
		routingCtx.ReqBody = decodeBody
		return prefillBody, nil
	}
	params := fmt.Sprintf(`{"request_type":"context_only","disagg_request_id":%d}`, GetDisaggRequestID(trtMachineID))
	return pd.NewJSONEditor(body).
		SetRaw("disaggregated_params", []byte(params)).
		Result()
}

// prepareTRTGenerationFirst constructs independent bodies with one shared ID.
// Only the small gateway-owned params objects are serialized; prompt/messages/
// tools retain their original bytes on both legs. The worker metadata is
// allowlisted so it cannot overwrite request_type, IDs, or scheduling policy.
func prepareTRTGenerationFirst(body []byte, info TRTServerInfo, id int64) ([]byte, []byte, error) {
	if err := info.validate(); err != nil {
		return nil, nil, err
	}
	if err := pd.ValidateJSONObject(body, "TRT request body"); err != nil {
		return nil, nil, err
	}
	ctxParams, err := pd.NewJSONEditor([]byte(`{}`)).
		Set("request_type", "context_only").
		Set("disagg_request_id", id).
		Set("schedule_style", trtGenerationFirstSchedule).Result()
	if err != nil {
		return nil, nil, err
	}
	gen := pd.NewJSONEditor([]byte(`{}`)).
		Set("request_type", "generation_only").
		Set("disagg_request_id", id).
		Set("ctx_request_id", id).
		Set("schedule_style", trtGenerationFirstSchedule).
		Set("ctx_info_endpoint", info.ContextInfoEndpoint).
		Set("ctx_dp_rank", info.ContextDPRank)
	if info.EncodedOpaqueState != "" {
		gen.Set("encoded_opaque_state", info.EncodedOpaqueState)
	}
	genParams, err := gen.Result()
	if err != nil {
		return nil, nil, err
	}
	prefillBody, err := pd.NewJSONEditor(body).SetRaw("disaggregated_params", ctxParams).Result()
	if err != nil {
		return nil, nil, err
	}
	decodeBody, err := pd.NewJSONEditor(body).SetRaw("disaggregated_params", genParams).Result()
	return prefillBody, decodeBody, err
}

// MergePrefillResponse injects TensorRT-LLM disaggregated_params from the
// prefill response into routingCtx.ReqBody so the decode worker can resume
// generation from the pre-filled KV cache.
//
// disaggregated_params is looked up at the top level first, then under
// choices[0]. Its fragment is copied verbatim from the response with
// request_type overridden to "generation_only", so large integer fields such
// as disagg_request_id / ctx_request_id keep their exact value. When the
// response includes prompt_token_ids they are routed into the decode body by
// request path: "prompt" for /v1/completions, "prompt_token_ids" for
// /v1/chat/completions.
func (h *TRTLLMHandler) MergePrefillResponse(
	routingCtx *types.RoutingContext,
	prefillResponse []byte,
	prefillPod *v1.Pod,
) error {
	if h.generationFirst {
		// Envoy may already be using the decode body. This mode does not
		// consume CTX response metadata or its prompt_token_ids.
		return nil
	}
	if err := pd.ValidateJSONObject(routingCtx.ReqBody, "original request body"); err != nil {
		return err
	}

	disaggParams := gjson.GetBytes(prefillResponse, "disaggregated_params")
	if !disaggParams.Exists() {
		disaggParams = gjson.GetBytes(prefillResponse, "choices.0.disaggregated_params")
	}
	if !disaggParams.Exists() {
		klog.InfoS("no disaggregated_params in TRT prefill response", "request_id", routingCtx.RequestID)
		return nil
	}
	if !disaggParams.IsObject() {
		return fmt.Errorf("disaggregated_params has unexpected type %s, expected object", disaggParams.Type.String())
	}

	// Patch request_type on the small disaggregated_params fragment first,
	// then splice it into the (much larger) request body with one edit.
	params, err := sjson.SetBytes([]byte(disaggParams.Raw), "request_type", "generation_only")
	if err != nil {
		return fmt.Errorf("failed to set disaggregated_params.request_type: %w", err)
	}
	e := pd.NewJSONEditor(routingCtx.ReqBody).SetRaw("disaggregated_params", params)
	if pti := gjson.GetBytes(prefillResponse, "prompt_token_ids"); pti.IsArray() {
		switch utils.PathWithoutQuery(routingCtx.ReqPath) {
		case "/v1/completions":
			e.SetRaw("prompt", []byte(pti.Raw))
		case "/v1/chat/completions":
			e.SetRaw("prompt_token_ids", []byte(pti.Raw))
		}
	}
	updatedReqBody, err := e.Result()
	if err != nil {
		return fmt.Errorf("failed to update request body: %w", err)
	}
	routingCtx.ReqBody = updatedReqBody

	klog.InfoS("updated routing context with disaggregated_params (TensorRT-LLM)",
		"request_id", routingCtx.RequestID,
		"prefill_pod", prefillPod.Name,
		"prefill_host", prefillPod.Status.PodIP)
	return nil
}
