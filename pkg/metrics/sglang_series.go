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
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

// Only these gauges describe the independent request/KV pools of DP-attention.
// Other SGLang metrics can describe replicated work or different memory pools.
func mergeSGLangDPSeries(families map[string]*dto.MetricFamily) {
	capacity := families["sglang:max_total_num_tokens"]
	pools := sglangDPPools(capacity, false)
	for _, name := range []string{
		"sglang:num_running_reqs", "sglang:num_queue_reqs",
		"sglang:token_usage", "sglang:max_total_num_tokens",
	} {
		family := families[name]
		if family == nil || family.GetType() != dto.MetricType_GAUGE {
			continue
		}
		groups := sglangDPPools(family, true)
		keys := make([]string, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var merged []*dto.Metric
		for _, m := range family.Metric {
			if rank, _ := GetLabelValueForKey(m, "dp_rank"); rank == "" || m.Gauge == nil {
				merged = append(merged, m)
			}
		}
		for _, key := range keys {
			group := groups[key]
			ranks := make([]string, 0, len(group))
			for rank := range group {
				ranks = append(ranks, rank)
			}
			sort.Strings(ranks)
			value, weight := 0.0, 0.0
			valid := true
			for _, rank := range ranks {
				m := group[rank]
				if name == "sglang:token_usage" {
					c := pools[sglangScopeKey(m, false)][rank]
					if c == nil || c.Gauge.GetValue() <= 0 {
						valid = false
						break
					}
					value += m.Gauge.GetValue() * c.Gauge.GetValue()
					weight += c.Gauge.GetValue()
				} else {
					value += m.Gauge.GetValue()
				}
			}
			if !valid {
				// Without a capacity for every pool, retain the original series
				// rather than publishing an unjustified unweighted utilization.
				for _, m := range family.Metric {
					if rank, _ := GetLabelValueForKey(m, "dp_rank"); rank != "" && m.Gauge != nil && sglangScopeKey(m, true) == key {
						merged = append(merged, m)
					}
				}
				continue
			}
			if weight > 0 {
				value /= weight
			}
			out := &dto.Metric{Gauge: &dto.Gauge{Value: &value}}
			for _, label := range group[ranks[0]].Label {
				if !sglangRankLabel(label.GetName()) {
					out.Label = append(out.Label, label)
				}
			}
			merged = append(merged, out)
		}
		family.Metric = merged
	}
}

// TP/PP/EP observations of one DP rank are replicas, not additional pools.
// Choose a stable representative when multiple observations are exposed.
// Priority is retained as its own scope: totals and breakdowns are never added.
func sglangDPPools(family *dto.MetricFamily, priority bool) map[string]map[string]*dto.Metric {
	groups := make(map[string]map[string]*dto.Metric)
	if family == nil || family.GetType() != dto.MetricType_GAUGE {
		return groups
	}
	for _, m := range family.Metric {
		rank, _ := GetLabelValueForKey(m, "dp_rank")
		if rank == "" || m.Gauge == nil {
			continue
		}
		key := sglangScopeKey(m, priority)
		if groups[key] == nil {
			groups[key] = make(map[string]*dto.Metric)
		}
		previous := groups[key][rank]
		if previous == nil || labelKeyWithoutEngine(m) < labelKeyWithoutEngine(previous) {
			groups[key][rank] = m
		}
	}
	return groups
}

func sglangScopeKey(m *dto.Metric, priority bool) string {
	pairs := make([]string, 0, len(m.Label))
	for _, label := range m.Label {
		name := label.GetName()
		if !sglangRankLabel(name) && (priority || name != "priority") {
			pairs = append(pairs, name+"\x00"+label.GetValue())
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\x01")
}

func sglangRankLabel(name string) bool {
	return name == "dp_rank" || name == "tp_rank" || name == "pp_rank" || name == "moe_ep_rank"
}
