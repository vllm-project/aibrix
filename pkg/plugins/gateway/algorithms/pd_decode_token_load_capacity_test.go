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

package routingalgorithms

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	v1 "k8s.io/api/core/v1"
)

func withDecodeTokenLoadCapacityAware(t *testing.T, on bool) {
	t.Helper()
	prev := aibrixDecodeTokenLoadCapacityAware
	aibrixDecodeTokenLoadCapacityAware = on
	t.Cleanup(func() { aibrixDecodeTokenLoadCapacityAware = prev })
}

// vllmCapacity is the cache_config_info labels of a vLLM pod with blocks × 16 tokens of KV.
func vllmCapacity(blocks string) map[string]metrics.MetricValue {
	return map[string]metrics.MetricValue{
		metrics.KVCacheNumGPUBlocks: &metrics.LabelValueMetricValue{Value: blocks},
		metrics.KVCacheBlockSize:    &metrics.LabelValueMetricValue{Value: "16"},
	}
}

func TestDecodeKVCapacities(t *testing.T) {
	small := burstPod("decode-small", "decode", "127.0.0.100")
	large := burstPod("decode-large", "decode", "127.0.0.101")
	sglang := burstPod("decode-sglang", "decode", "127.0.0.102")
	unknown := burstPod("decode-unknown", "decode", "127.0.0.103")
	broken := burstPod("decode-broken", "decode", "127.0.0.104")
	infinite := burstPod("decode-infinite", "decode", "127.0.0.105")
	pods := []*v1.Pod{small, large, sglang, unknown, broken, infinite}

	r, _ := newDecodeTokenLoadTestRouter(t)
	r.cache = cache.NewWithPodsMetricsForTest(pods, "token-load-model", map[string]map[string]metrics.MetricValue{
		small.Name:  vllmCapacity("6250"),  // 100k tokens
		large.Name:  vllmCapacity("12500"), // 200k tokens
		sglang.Name: {metrics.KVCacheCapacityTokens: &metrics.SimpleMetricValue{Value: 300_000}},
		// vLLM reports "None" before the engine has allocated its blocks.
		broken.Name: vllmCapacity("None"),
		// The Prometheus text format accepts +Inf as a gauge value.
		infinite.Name: {metrics.KVCacheCapacityTokens: &metrics.SimpleMetricValue{Value: math.Inf(1)}},
	})

	t.Run("factors scale to the mean capacity", func(t *testing.T) {
		withDecodeTokenLoadCapacityAware(t, true)
		factors := r.decodeKVCapacities(true, pods)
		require.Len(t, factors, len(pods))
		// Mean of the known capacities: (100k + 200k + 300k) / 3 = 200k.
		assert.InDelta(t, 2.0, factors[burstPodKey(small.Name)], 1e-9)
		assert.InDelta(t, 1.0, factors[burstPodKey(large.Name)], 1e-9)
		assert.InDelta(t, 2.0/3, factors[burstPodKey(sglang.Name)], 1e-9)
		assert.Equal(t, 1.0, factors[burstPodKey(unknown.Name)], "an unknown capacity counts as the mean")
		assert.Equal(t, 1.0, factors[burstPodKey(broken.Name)], "an unparsable capacity counts as the mean")
		assert.Equal(t, 1.0, factors[burstPodKey(infinite.Name)], "an infinite capacity counts as the mean")
	})

	t.Run("no scaling", func(t *testing.T) {
		withDecodeTokenLoadCapacityAware(t, true)
		assert.Nil(t, r.decodeKVCapacities(false, pods), "only token_load is scaled")
		assert.Nil(t, r.decodeKVCapacities(true, []*v1.Pod{unknown, broken, infinite}), "no candidate reports a capacity")
		withDecodeTokenLoadCapacityAware(t, false)
		assert.Nil(t, r.decodeKVCapacities(true, pods), "switch off")
	})
}

// Two decode pods hold close token loads, but one has half the KV of the
// other. Scaled by capacity, the request goes to the larger pod, which is
// less full; with the switch off, the smaller token count wins.
func TestPDRouter_DecodeTokenLoadScalesByCapacity(t *testing.T) {
	small := burstPod("decode-small", "decode", "127.0.0.100")
	large := burstPod("decode-large", "decode", "127.0.0.101")
	readyPods := []*v1.Pod{burstPod("prefill-0", "prefill", "127.0.0.1"), small, large}
	for _, pod := range readyPods {
		pod.Labels[constants.ModelLabelName] = "token-load-model"
	}

	route := func(t *testing.T, capacityAware bool) string {
		withDecodeTokenLoadCapacityAware(t, capacityAware)
		r, tokenLoad := newDecodeTokenLoadTestRouter(t)
		r.cache = cache.NewWithPodsMetricsForTest(readyPods, "token-load-model", map[string]map[string]metrics.MetricValue{
			small.Name: vllmCapacity("6250"),  // 100k tokens, 60% full
			large.Name: vllmCapacity("12500"), // 200k tokens, 40% full
		})
		tokenLoad.AcquireDecodeWithTTL("on-small", burstPodKey(small.Name), 60_000, 0)
		tokenLoad.AcquireDecodeWithTTL("on-large", burstPodKey(large.Name), 80_000, 0)

		_, decode, err := r.filterPrefillDecodePods(tokenLoadRequest(t, "next", 400), readyPods)
		require.NoError(t, err)
		return decode.Name
	}

	assert.Equal(t, large.Name, route(t, true), "the larger pod is less full")
	assert.Equal(t, small.Name, route(t, false), "without capacities, the smaller token count wins")
}

// With equal capacities the scaled scores are the token scores themselves.
func TestPDRouter_DecodeTokenLoadEqualCapacitiesUnchanged(t *testing.T) {
	a := burstPod("decode-a", "decode", "127.0.0.100")
	b := burstPod("decode-b", "decode", "127.0.0.101")
	pods := []*v1.Pod{a, b}

	scores := func(t *testing.T, capacityAware bool) map[string]float64 {
		withDecodeTokenLoadCapacityAware(t, capacityAware)
		r, tokenLoad := newDecodeTokenLoadTestRouter(t)
		r.cache = cache.NewWithPodsMetricsForTest(pods, "token-load-model", map[string]map[string]metrics.MetricValue{
			a.Name: vllmCapacity("10000"),
			b.Name: vllmCapacity("10000"),
		})
		tokenLoad.AcquireDecodeWithTTL("on-a", burstPodKey(a.Name), 1234, 0)
		run := r.scoreDecodePods(tokenLoadRequest(t, "probe", 400), append([]*v1.Pod{}, pods...),
			1, 1, 1, map[string]float64{}, map[string]float64{}, map[string]float64{}, nil)
		require.NoError(t, run.Err)
		return map[string]float64{"max": run.MaxScore, "pick": run.PerRoleset["burst-rs"].Score}
	}

	assert.Equal(t, scores(t, false), scores(t, true))
}
