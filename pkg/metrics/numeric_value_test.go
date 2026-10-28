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
	"math"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
)

func numericPromValue(result model.Value) *PrometheusMetricValue {
	return &PrometheusMetricValue{Result: &result}
}

func TestNumericValue(t *testing.T) {
	tests := []struct {
		name   string
		value  MetricValue
		want   float64
		wantOK bool
	}{
		{"simple value", &SimpleMetricValue{Value: 3}, 3, true},
		{"promql vector", numericPromValue(model.Vector{&model.Sample{Value: 42}}), 42, true},
		{"promql scalar", numericPromValue(&model.Scalar{Value: 7}), 7, true},
		{"promql empty vector", numericPromValue(model.Vector{}), 0, false},
		{"promql matrix", numericPromValue(model.Matrix{&model.SampleStream{Values: []model.SamplePair{{Value: 5}}}}), 5, true},
		{"promql NaN from a pod with no requests", numericPromValue(model.Vector{&model.Sample{Value: model.SampleValue(math.NaN())}}), 0, false},
		{"promql +Inf", numericPromValue(model.Vector{&model.Sample{Value: model.SampleValue(math.Inf(1))}}), 0, false},
		{"simple +Inf", &SimpleMetricValue{Value: math.Inf(1)}, 0, false},
		{"promql nil result", &PrometheusMetricValue{}, 0, false},
		{"promql result holding a nil value", &PrometheusMetricValue{Result: new(model.Value)}, 0, false},
		{"typed nil promql value", (*PrometheusMetricValue)(nil), 0, false},
		{"typed nil simple value", (*SimpleMetricValue)(nil), 0, false},
		{"histogram holds no single number", &HistogramMetricValue{Sum: 4, Count: 2}, 0, false},
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
