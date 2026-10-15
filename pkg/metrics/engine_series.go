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
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

// engineLabel is the label vLLM puts on every metric to tell apart the engines of a
// data-parallel server (--data-parallel-size N exposes one series per engine).
const engineLabel = "engine"

// engineMeanGauges hold a fraction or a 0/1 state per engine, so the pod's value is the
// mean across engines. Every other gauge (request counts, throughput) is summed.
var engineMeanGauges = map[string]bool{
	"vllm:kv_cache_usage_perc":  true,
	"vllm:gpu_cache_usage_perc": true,
	"vllm:cpu_cache_usage_perc": true,
	"vllm:engine_sleep_state":   true,
}

// mergeEngineSeries folds the per-engine series of each family into one series per
// remaining label set, so the parsers that read one series per pod or per model see the
// whole pod instead of a single engine. Families without an engine label are left as is.
func mergeEngineSeries(families map[string]*dto.MetricFamily) {
	for name, family := range families {
		family.Metric = mergeFamilyEngines(name, family)
	}
}

func mergeFamilyEngines(name string, family *dto.MetricFamily) []*dto.Metric {
	switch family.GetType() {
	case dto.MetricType_GAUGE, dto.MetricType_COUNTER, dto.MetricType_UNTYPED, dto.MetricType_HISTOGRAM:
	default:
		return family.Metric
	}
	hasEngine := false
	for _, m := range family.Metric {
		if engineIndex(m) >= 0 {
			hasEngine = true
			break
		}
	}
	if !hasEngine {
		return family.Metric
	}

	merged := make([]*dto.Metric, 0, len(family.Metric))
	groups := make(map[string]int, len(family.Metric))
	engines := make([]int, 0, len(family.Metric))
	for _, m := range family.Metric {
		key := labelKeyWithoutEngine(m)
		i, ok := groups[key]
		if !ok {
			groups[key] = len(merged)
			merged = append(merged, copyWithoutEngine(m))
			engines = append(engines, 1)
			continue
		}
		addSeries(merged[i], m)
		engines[i]++
	}
	if engineMeanGauges[name] {
		for i, m := range merged {
			if m.Gauge != nil && engines[i] > 1 {
				v := m.Gauge.GetValue() / float64(engines[i])
				m.Gauge.Value = &v
			}
		}
	}
	return merged
}

func engineIndex(m *dto.Metric) int {
	for i, l := range m.Label {
		if l.GetName() == engineLabel {
			return i
		}
	}
	return -1
}

func labelKeyWithoutEngine(m *dto.Metric) string {
	pairs := make([]string, 0, len(m.Label))
	for _, l := range m.Label {
		if l.GetName() != engineLabel {
			pairs = append(pairs, l.GetName()+"\x00"+l.GetValue())
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\x01")
}

// copyWithoutEngine copies m minus its engine label, so merging never writes into the
// parsed series it started from.
func copyWithoutEngine(m *dto.Metric) *dto.Metric {
	out := &dto.Metric{}
	for _, l := range m.Label {
		if l.GetName() != engineLabel {
			out.Label = append(out.Label, l)
		}
	}
	if m.Gauge != nil {
		v := m.Gauge.GetValue()
		out.Gauge = &dto.Gauge{Value: &v}
	}
	if m.Counter != nil {
		v := m.Counter.GetValue()
		out.Counter = &dto.Counter{Value: &v}
	}
	if m.Untyped != nil {
		v := m.Untyped.GetValue()
		out.Untyped = &dto.Untyped{Value: &v}
	}
	if m.Histogram != nil {
		count, sum := m.Histogram.GetSampleCount(), m.Histogram.GetSampleSum()
		h := &dto.Histogram{SampleCount: &count, SampleSum: &sum}
		for _, b := range m.Histogram.Bucket {
			c, ub := b.GetCumulativeCount(), b.GetUpperBound()
			h.Bucket = append(h.Bucket, &dto.Bucket{CumulativeCount: &c, UpperBound: &ub})
		}
		out.Histogram = h
	}
	return out
}

func addSeries(dst, src *dto.Metric) {
	add := func(p **float64, v float64) {
		sum := **p + v
		*p = &sum
	}
	switch {
	case dst.Gauge != nil && src.Gauge != nil:
		add(&dst.Gauge.Value, src.Gauge.GetValue())
	case dst.Counter != nil && src.Counter != nil:
		add(&dst.Counter.Value, src.Counter.GetValue())
	case dst.Untyped != nil && src.Untyped != nil:
		add(&dst.Untyped.Value, src.Untyped.GetValue())
	case dst.Histogram != nil && src.Histogram != nil:
		count := dst.Histogram.GetSampleCount() + src.Histogram.GetSampleCount()
		dst.Histogram.SampleCount = &count
		add(&dst.Histogram.SampleSum, src.Histogram.GetSampleSum())
		srcCounts := make(map[float64]uint64, len(src.Histogram.Bucket))
		for _, b := range src.Histogram.Bucket {
			srcCounts[b.GetUpperBound()] = b.GetCumulativeCount()
		}
		for _, b := range dst.Histogram.Bucket {
			c := b.GetCumulativeCount() + srcCounts[b.GetUpperBound()]
			b.CumulativeCount = &c
		}
	}
}
