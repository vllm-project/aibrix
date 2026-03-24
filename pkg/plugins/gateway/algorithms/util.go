/*
Copyright 2024 The Aibrix Team.

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

package routingalgorithms

import (
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Snowflake-style disagg request ID constants for TensorRT-LLM PD routing.
// Layout: [timestamp(41b)][machineID(10b)][counter(12b)]
// The modulo rotation guarantees result >= minGlobalID so TRT-LLM's executor treats
// it as a global (cross-worker) disagg ID rather than a local one.
//
// Timestamp uses milliseconds since trtSnowflakeEpochMs (not Unix epoch) so the 41-bit
// field does not overflow around year ~2039 when using raw Unix millis from 1970.
const (
	trtCounterBits      = 12
	trtMachineIDBits    = 10
	trtTimestampBits    = 41
	trtSnowflakeEpochMs = int64(1672531200000) // 2023-01-01T00:00:00Z in milliseconds
	trtCounterMask      = (1 << trtCounterBits) - 1
	trtTimestampMax     = (1 << trtTimestampBits) - 1 // max ms offset representable in 41 bits (~69.7y span)
	trtMinGlobalID      = int64(1) << 42
	trtMaxInt64         = int64(1<<63 - 1)
)

var (
	// Machine ID for TRT-LLM snowflake disagg request IDs (trtMachineIDBits-wide field).
	// Valid range: 0 <= id < 2^trtMachineIDBits (i.e. [0, 1024) for 10 bits). Out-of-range
	// values would corrupt the snowflake layout in getDisaggRequestID.
	trtMachineID int64 = int64(utils.LoadEnvInt("AIBRIX_TRT_MACHINE_ID", 0))

	// Per-process monotonic counter for snowflake ID generation.
	globalDisaggCounter atomic.Int64
)

func init() {
	if err := validateTRTMachineIDValue(trtMachineID); err != nil {
		panic("routingalgorithms: " + err.Error())
	}
}

// validateTRTMachineIDValue ensures machineID fits in trtMachineIDBits bits (used in getDisaggRequestID).
func validateTRTMachineIDValue(machineID int64) error {
	maxExclusive := int64(1 << trtMachineIDBits)
	if machineID < 0 || machineID >= maxExclusive {
		return fmt.Errorf("invalid AIBRIX_TRT_MACHINE_ID=%d: must satisfy 0 <= id < %d (10-bit field)", machineID, maxExclusive)
	}
	return nil
}

// getDisaggRequestID generates a snowflake-style ID that is shared between a prefill
// and its corresponding decode request so TRT-LLM can correlate the KV-cache entry.
// The modulo rotation ensures the result is always >= trtMinGlobalID (1<<42), which
// is the threshold TRT-LLM uses to distinguish global disagg IDs from local ones.
func getDisaggRequestID(machineID int64) int64 {
	timestampMs := time.Now().UnixMilli() - trtSnowflakeEpochMs
	if timestampMs < 0 {
		timestampMs = 0 // clock skew before custom epoch
	}
	if timestampMs > trtTimestampMax {
		timestampMs = trtTimestampMax // stay within 41-bit timestamp field (~2092+ from epoch ms)
	}
	counter := (globalDisaggCounter.Add(1) - 1) & trtCounterMask
	globalID := (timestampMs << (trtMachineIDBits + trtCounterBits)) |
		(machineID << trtCounterBits) |
		counter
	return globalID%(trtMaxInt64-trtMinGlobalID) + trtMinGlobalID
}

// mean calculates the mean of a slice of float64 numbers.
func mean(numbers []float64) float64 {
	if len(numbers) == 0 {
		return 0
	}
	sum := 0.0
	for _, number := range numbers {
		sum += number
	}
	return sum / float64(len(numbers))
}

// standardDeviation calculates the standard deviation of a slice of float64 numbers.
func standardDeviation(numbers []float64) float64 {
	if len(numbers) <= 1 {
		return 0
	}
	avg := mean(numbers)
	sumOfSquares := 0.0
	for _, number := range numbers {
		sumOfSquares += math.Pow(number-avg, 2)
	}
	variance := sumOfSquares / float64(len(numbers)-1)
	return math.Sqrt(variance)
}

// SelectRandomPodAsFallback selects a pod randomly as a fallback.
// This method should only be used when all other selection mechanisms have failed.
// For example, if no pods meet the required criteria (e.g., valid metrics or specific conditions),
// this method can be called to randomly select a pod from the provided list.
func SelectRandomPodAsFallback(ctx *types.RoutingContext, pods []*v1.Pod, randomFunc func(int) int) (*v1.Pod, error) {
	klog.Warningf("No suitable pods found; selecting a pod randomly as fallback, requestID: %s", ctx.RequestID)
	targetPod, err := utils.SelectRandomPod(pods, randomFunc)
	if err != nil {
		klog.ErrorS(err, "Random fallback selection failed", "requestID", ctx.RequestID)
		return nil, fmt.Errorf("random fallback selection failed: %w", err)
	}
	return targetPod, nil
}

// anySliceForJSON converts a JSON-decoded array (e.g. []any from sonic) into []any suitable for map[string]any marshaling.
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
