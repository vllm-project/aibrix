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
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

func TestIsValidMetricValue(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  bool
	}{
		{name: "zero", value: 0, want: true},
		{name: "positive", value: 42.5, want: true},
		{name: "max float", value: math.MaxFloat64, want: true},
		{name: "nan", value: math.NaN(), want: false},
		{name: "positive infinity", value: math.Inf(1), want: false},
		{name: "negative infinity", value: math.Inf(-1), want: false},
		{name: "negative", value: -0.1, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidMetricValue(tt.value))
		})
	}
}

func TestEvaluateSnapshot(t *testing.T) {
	tests := []struct {
		name          string
		values        []float64
		stats         types.MetricCollectionStats
		wantValues    []float64
		wantHealth    types.MetricSourceHealth
		wantInputSame bool
	}{
		{
			name:   "all samples are valid and collection complete",
			values: []float64{0, 7.5, math.MaxFloat64},
			stats: types.MetricCollectionStats{
				ExpectedCount:     3,
				FetchSuccessCount: 3,
			},
			wantValues: []float64{0, 7.5, math.MaxFloat64},
			wantHealth: types.MetricSourceHealth{
				TotalCount:   3,
				ValidCount:   3,
				FailureCount: 0,
				InvalidCount: 0,
				State:        types.MetricHealthStateHealthy,
				Reason:       types.MetricReasonMetricsHealthy,
			},
			wantInputSame: true,
		},
		{
			name:   "partial collection failure with remaining valid samples is degraded",
			values: []float64{10, 20},
			stats: types.MetricCollectionStats{
				ExpectedCount:     3,
				FetchSuccessCount: 2,
				FetchFailureCount: 1,
			},
			wantValues: []float64{10, 20},
			wantHealth: types.MetricSourceHealth{
				TotalCount:   3,
				ValidCount:   2,
				FailureCount: 1,
				InvalidCount: 0,
				State:        types.MetricHealthStateDegraded,
				Reason:       types.MetricReasonPartialMetricCollectionFailure,
			},
			wantInputSame: true,
		},
		{
			name:   "invalid samples are removed and source is degraded when valid samples remain",
			values: []float64{math.NaN(), 5, math.Inf(1), math.Inf(-1), -3},
			stats: types.MetricCollectionStats{
				ExpectedCount:     5,
				FetchSuccessCount: 5,
			},
			wantValues: []float64{5},
			wantHealth: types.MetricSourceHealth{
				TotalCount:   5,
				ValidCount:   1,
				FailureCount: 0,
				InvalidCount: 4,
				State:        types.MetricHealthStateDegraded,
				Reason:       types.MetricReasonInvalidMetricValue,
			},
			wantInputSame: true,
		},
		{
			name: "all fetches fail",
			stats: types.MetricCollectionStats{
				ExpectedCount:     2,
				FetchFailureCount: 2,
			},
			wantHealth: types.MetricSourceHealth{
				TotalCount:   2,
				ValidCount:   0,
				FailureCount: 2,
				InvalidCount: 0,
				State:        types.MetricHealthStateFailed,
				Reason:       types.MetricReasonAllMetricSamplesUnavailable,
			},
			wantInputSame: true,
		},
		{
			name:   "all fetched samples are invalid",
			values: []float64{math.NaN(), math.Inf(1), -1},
			stats: types.MetricCollectionStats{
				ExpectedCount:     3,
				FetchSuccessCount: 3,
			},
			wantHealth: types.MetricSourceHealth{
				TotalCount:   3,
				ValidCount:   0,
				FailureCount: 0,
				InvalidCount: 3,
				State:        types.MetricHealthStateFailed,
				Reason:       types.MetricReasonAllMetricSamplesUnavailable,
			},
			wantInputSame: true,
		},
		{
			name:   "fetch failure and invalid sample without valid samples is failed",
			values: []float64{math.Inf(-1)},
			stats: types.MetricCollectionStats{
				ExpectedCount:     2,
				FetchSuccessCount: 1,
				FetchFailureCount: 1,
			},
			wantHealth: types.MetricSourceHealth{
				TotalCount:   2,
				ValidCount:   0,
				FailureCount: 1,
				InvalidCount: 1,
				State:        types.MetricHealthStateFailed,
				Reason:       types.MetricReasonAllMetricSamplesUnavailable,
			},
			wantInputSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := &types.MetricSnapshot{
				Namespace:       "tenant-a",
				TargetName:      "worker",
				MetricName:      "queue_depth",
				Values:          slices.Clone(tt.values),
				CollectionStats: tt.stats,
			}
			originalValues := slices.Clone(snapshot.Values)

			sanitized, health := EvaluateSnapshot(snapshot)

			require.NotSame(t, snapshot, sanitized)
			if len(tt.wantValues) == 0 {
				assert.Empty(t, sanitized.Values)
			} else {
				assert.Equal(t, tt.wantValues, sanitized.Values)
			}
			assert.Equal(t, tt.wantHealth, health)
			assert.Equal(t, health, sanitized.Health)
			assert.Equal(t, health.TotalCount, health.ValidCount+health.FailureCount+health.InvalidCount)
			if tt.wantInputSame {
				assertRawMetricValues(t, originalValues, snapshot.Values)
			}
		})
	}
}
