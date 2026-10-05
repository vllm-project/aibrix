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
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
)

func TestMetricType(t *testing.T) {
	t.Run("IsRawMetric", func(t *testing.T) {
		m := MetricType{Raw: Gauge}
		assert.True(t, m.IsRawMetric())
		assert.False(t, m.IsQuery())
	})

	t.Run("IsQuery", func(t *testing.T) {
		m := MetricType{Query: PromQL}
		assert.True(t, m.IsQuery())
		assert.False(t, m.IsRawMetric())
	})
}

func TestSimpleMetricValue(t *testing.T) {
	value := 42.0
	simpleMetric := SimpleMetricValue{Value: value}

	t.Run("GetSimpleValue", func(t *testing.T) {
		assert.Equal(t, 42.0, simpleMetric.GetSimpleValue())
	})

	t.Run("GetHistogramValue", func(t *testing.T) {
		assert.Nil(t, simpleMetric.GetHistogramValue())
	})

	t.Run("GetPrometheusResult", func(t *testing.T) {
		assert.Nil(t, simpleMetric.GetPrometheusResult())
	})
}

func TestHistogramMetricValue(t *testing.T) {
	histogram := HistogramMetricValue{
		Sum:   100.0,
		Count: 10,
		Buckets: map[string]float64{
			"+Inf":        3369,
			"0.001000":    0,
			"0.005000":    0,
			"0.010000":    0,
			"0.020000":    0,
			"0.040000":    123,
			"0.060000":    126,
			"0.080000":    126,
			"0.100000":    126,
			"0.250000":    127,
			"0.500000":    439,
			"0.750000":    3352,
			"1.000000":    3362,
			"2.500000":    3369,
			"5.000000":    3369,
			"7.500000":    3369,
			"10.000000":   3369,
			"20.000000":   3369,
			"40.000000":   3369,
			"80.000000":   3369,
			"160.000000":  3369,
			"640.000000":  3369,
			"2560.000000": 3369,
		},
	}

	t.Run("GetSimpleValue", func(t *testing.T) {
		assert.Equal(t, 0.0, histogram.GetSimpleValue())
	})

	t.Run("GetHistogramValue", func(t *testing.T) {
		r := histogram.GetHistogramValue()
		assert.NotNil(t, r)
		assert.Equal(t, &histogram, r)
	})

	t.Run("GetPrometheusResult", func(t *testing.T) {
		r := histogram.GetPrometheusResult()
		assert.Nil(t, r)
	})

	t.Run("GetSum", func(t *testing.T) {
		assert.Equal(t, 100.0, histogram.GetSum())
	})

	t.Run("GetCount", func(t *testing.T) {
		assert.Equal(t, 10.0, histogram.GetCount())
	})

	t.Run("GetBucketValue", func(t *testing.T) {
		v, ok := histogram.GetBucketValue("0.100000")
		assert.True(t, ok)
		assert.Equal(t, 126.0, v)

		_, ok = histogram.GetBucketValue("unknown")
		assert.False(t, ok)
	})

	t.Run("GetMean", func(t *testing.T) {
		assert.Equal(t, 10.0, histogram.GetMean())
	})

	t.Run("GetPercentile", func(t *testing.T) {
		p50, err := histogram.GetPercentile(50)
		assert.NoError(t, err)
		assert.InDelta(t, 0.607, p50, 0.1)

		p90, err := histogram.GetPercentile(90)
		assert.NoError(t, err)
		assert.InDelta(t, 0.723, p90, 0.1)

		p99, err := histogram.GetPercentile(99)
		assert.NoError(t, err)
		assert.InDelta(t, 0.749, p99, 0.1)

		_, err = histogram.GetPercentile(110)
		assert.Error(t, err)
		assert.Equal(t, "percentile must be between 0 and 100, got: 110.000000", err.Error())
	})
}

func TestPrometheusMetricValue(t *testing.T) {
	result := model.Vector{
		&model.Sample{
			Metric: model.Metric{"__name__": "test_metric"},
			Value:  123.45,
		},
	}
	var value model.Value = result
	prometheusMetric := PrometheusMetricValue{Result: &value}

	t.Run("GetSimpleValue", func(t *testing.T) {
		assert.Equal(t, 0.0, prometheusMetric.GetSimpleValue())
	})

	t.Run("GetHistogramValue", func(t *testing.T) {
		assert.Nil(t, prometheusMetric.GetHistogramValue())
	})

	t.Run("GetPrometheusResult", func(t *testing.T) {
		r := prometheusMetric.GetPrometheusResult()
		assert.NotNil(t, r)
		assert.Equal(t, result.Type(), (*r).Type())
	})
}

func TestMetric(t *testing.T) {
	metric := Metric{
		MetricSource: PodRawMetrics,
		MetricType: MetricType{
			Raw: Gauge,
		},
		PromQL:      "",
		Description: "A test metric",
	}

	t.Run("Metric fields", func(t *testing.T) {
		assert.Equal(t, PodRawMetrics, metric.MetricSource)
		assert.Equal(t, Gauge, metric.MetricType.Raw)
		assert.Equal(t, "", metric.PromQL)
		assert.Equal(t, "A test metric", metric.Description)
	})
}
