/*
Copyright 2025 The Aibrix Team.

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

package cache

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestMergeLabelPairs_DedupAndPreferSecondaryValue(t *testing.T) {
	primaryNames := []string{"engine_type", "model_name"}
	primaryValues := []string{"from_engine", "m1"}
	secondaryNames := []string{"namespace", "engine_type", "pod"}
	secondaryValues := []string{"ns1", "vllm", "p1"}

	mergedNames, mergedValues := mergeLabelPairs(primaryNames, primaryValues, secondaryNames, secondaryValues)
	require.Equal(t, []string{"engine_type", "model_name", "namespace", "pod"}, mergedNames)
	require.Equal(t, []string{"vllm", "m1", "ns1", "p1"}, mergedValues)

	seen := make(map[string]struct{}, len(mergedNames))
	for _, n := range mergedNames {
		_, ok := seen[n]
		require.False(t, ok)
		seen[n] = struct{}{}
	}

	descDup := prometheus.NewDesc("num_requests_running", "help", []string{"engine_type", "engine_type"}, nil)
	require.Panics(t, func() {
		_ = prometheus.MustNewConstMetric(descDup, prometheus.GaugeValue, 1, "a", "b")
	})

	descMerged := prometheus.NewDesc("num_requests_running", "help", mergedNames, nil)
	require.NotPanics(t, func() {
		_ = prometheus.MustNewConstMetric(descMerged, prometheus.GaugeValue, 1, mergedValues...)
	})
}
