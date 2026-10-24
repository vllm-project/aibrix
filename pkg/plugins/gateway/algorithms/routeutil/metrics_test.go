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

package routeutil

import (
	"math"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

func promValue(result model.Value) *metrics.PrometheusMetricValue {
	return &metrics.PrometheusMetricValue{Result: &result}
}

func TestNumericValue(t *testing.T) {
	tests := []struct {
		name   string
		value  metrics.MetricValue
		want   float64
		wantOK bool
	}{
		{"simple value", &metrics.SimpleMetricValue{Value: 3}, 3, true},
		{"promql vector", promValue(model.Vector{&model.Sample{Value: 42}}), 42, true},
		{"promql scalar", promValue(&model.Scalar{Value: 7}), 7, true},
		{"promql NaN from a pod with no requests", promValue(model.Vector{&model.Sample{Value: model.SampleValue(math.NaN())}}), 0, false},
		{"promql empty vector", promValue(model.Vector{}), 0, false},
		{"promql nil result", &metrics.PrometheusMetricValue{}, 0, false},
		{"nil value", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NumericValue(tt.value)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
