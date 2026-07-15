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

package metrics

import (
	"math"

	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// IsValidMetricValue returns whether a metric sample can be used for scaling.
func IsValidMetricValue(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// EvaluateSnapshot returns a sanitized snapshot and source health without mutating the input.
func EvaluateSnapshot(snapshot *types.MetricSnapshot) (*types.MetricSnapshot, types.MetricSourceHealth) {
	if snapshot == nil {
		health := types.MetricSourceHealth{
			State:  types.MetricHealthStateFailed,
			Reason: types.MetricReasonAllMetricSamplesUnavailable,
		}
		return &types.MetricSnapshot{Health: health}, health
	}

	sanitized := *snapshot
	sanitized.Values = make([]float64, 0, len(snapshot.Values))

	for _, value := range snapshot.Values {
		if !IsValidMetricValue(value) {
			continue
		}
		sanitized.Values = append(sanitized.Values, value)
	}

	validCount := len(sanitized.Values)
	failureCount := snapshot.CollectionStats.FetchFailureCount
	invalidCount := len(snapshot.Values) - validCount
	totalCount := validCount + failureCount + invalidCount

	if snapshot.CollectionStats.ExpectedCount > totalCount {
		totalCount = snapshot.CollectionStats.ExpectedCount
	}
	if missingCount := totalCount - validCount - failureCount - invalidCount; missingCount > 0 {
		failureCount += missingCount
	}

	health := types.MetricSourceHealth{
		TotalCount:   totalCount,
		ValidCount:   validCount,
		FailureCount: failureCount,
		InvalidCount: invalidCount,
	}

	switch {
	case validCount == 0:
		health.State = types.MetricHealthStateFailed
		health.Reason = types.MetricReasonAllMetricSamplesUnavailable
	case failureCount > 0:
		health.State = types.MetricHealthStateDegraded
		health.Reason = types.MetricReasonPartialMetricCollectionFailure
	case invalidCount > 0:
		health.State = types.MetricHealthStateDegraded
		health.Reason = types.MetricReasonInvalidMetricValue
	default:
		health.State = types.MetricHealthStateHealthy
		health.Reason = types.MetricReasonMetricsHealthy
	}

	sanitized.Health = health
	return &sanitized, health
}
