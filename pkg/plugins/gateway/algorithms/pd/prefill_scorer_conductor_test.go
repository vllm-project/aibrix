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

package pd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ cache.MetricCache = (*mockMetricCache)(nil)

// mockMetricCache is a mock implementation of cache.MetricCache for testing.
// It only implements the methods used by conductor policy.
type mockMetricCache struct {
	// key: podName/podNamespace/modelName/metricName -> MetricValue
	values map[string]metrics.MetricValue
}

func newMockMetricCache() *mockMetricCache {
	return &mockMetricCache{
		values: make(map[string]metrics.MetricValue),
	}
}

func (m *mockMetricCache) key(podName, podNamespace, modelName, metricName string) string {
	return fmt.Sprintf("%s/%s/%s/%s", podName, podNamespace, modelName, metricName)
}

func (m *mockMetricCache) SetHistogram(podName, podNamespace, modelName, metricName string, sum, count float64) {
	m.values[m.key(podName, podNamespace, modelName, metricName)] = &metrics.HistogramMetricValue{
		Sum:   sum,
		Count: count,
	}
}

func (m *mockMetricCache) SetSimple(podName, podNamespace, modelName, metricName string, value float64) {
	m.values[m.key(podName, podNamespace, modelName, metricName)] = &metrics.SimpleMetricValue{
		Value: value,
	}
}

func (m *mockMetricCache) Clear(podName, podNamespace, modelName, metricName string) {
	delete(m.values, m.key(podName, podNamespace, modelName, metricName))
}

func (m *mockMetricCache) GetMetricValueByPod(podName, podNamespace, metricName string) (metrics.MetricValue, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

func (m *mockMetricCache) GetMetricValueByPodModel(podName, podNamespace, modelName, metricName string) (metrics.MetricValue, error) {
	key := m.key(podName, podNamespace, modelName, metricName)
	val, ok := m.values[key]
	if !ok {
		return nil, fmt.Errorf("metric not found: %s", key)
	}
	return val, nil
}

func (m *mockMetricCache) AddSubscriber(subscriber metrics.MetricSubscriber) {
	// no-op for testing
}

// Helper to create a test pod
func makeTestPod(name, namespace string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

// Helper to create a routing context
func makeRoutingContext(model, message string) *types.RoutingContext {
	return &types.RoutingContext{
		Model:   model,
		Message: message,
	}
}

// ============================================================
// Test: NewConductorPrefillPolicy
// ============================================================
// Verifies that the policy is created correctly with all dependencies.
func TestNewConductorPrefillPolicy(t *testing.T) {
	tok := tokenizer.NewCharacterTokenizer()
	pt := prefixcacheindexer.NewPrefixHashTable()
	mc := newMockMetricCache()

	policy := NewConductorPrefillPolicy(tok, pt, mc, NewConductorPrefillPolicyConfig())

	assert.NotNil(t, policy)
	assert.Equal(t, PrefillScorePolicyConductor, policy.Name())
}

// ============================================================
// Test: Prepare basic functionality
// ============================================================
// Verifies that Prepare correctly tokenizes and creates a scorer
// with the right model name and metric cache reference.
func TestConductorPrepare_Basic(t *testing.T) {
	tok := tokenizer.NewCharacterTokenizer()
	pt := prefixcacheindexer.NewPrefixHashTable()
	mc := newMockMetricCache()

	policy := NewConductorPrefillPolicy(tok, pt, mc, NewConductorPrefillPolicyConfig())

	ctx := makeRoutingContext("test-model", "hello world")
	pods := []*v1.Pod{makeTestPod("pod-1", "default")}
	readyMap := map[string]struct{}{"pod-1": {}}

	scorer, err := policy.Prepare(ctx, pods, readyMap)

	assert.NoError(t, err)
	assert.NotNil(t, scorer)

	conductorScorer := scorer.(*conductorScorer)
	assert.Equal(t, "test-model", conductorScorer.modelName)
	assert.Equal(t, len("hello world"), conductorScorer.totalTokens)
	assert.Equal(t, mc, conductorScorer.metricCache)
}

// ============================================================
// Test: ScorePod with valid histogram metric
// ============================================================
// Verifies that ScorePod correctly extracts avgPrefillTime from
// a histogram metric using GetMean().
func TestConductorScorePod_WithHistogramMetric(t *testing.T) {
	tok := tokenizer.NewCharacterTokenizer()
	pt := prefixcacheindexer.NewPrefixHashTable()
	mc := newMockMetricCache()

	policy := NewConductorPrefillPolicy(tok, pt, mc, NewConductorPrefillPolicyConfig())

	model := "test-model"
	pod := makeTestPod("pod-1", "default")

	// Set up histogram: sum=0.2ms, count=100 -> mean=0.02ms
	mc.SetHistogram("pod-1", "default", model, metrics.RequestPrefillTimeSeconds, 0.2, 100.0)

	ctx := makeRoutingContext(model, "test")
	readyMap := map[string]struct{}{"pod-1": {}}

	scorer, err := policy.Prepare(ctx, []*v1.Pod{pod}, readyMap)
	assert.NoError(t, err)

	score := scorer.ScorePod(pod, 3.0, 10.0)
	// tokenLen = 4, where 0 matched, 4 unmatched
	// queue 3 * 0.2s / 100 = 6ms
	// prefix = 0 + 1 = 1ms
	// prefill = 0.001 * 4^1.5 + 5 = 5.008ms
	// total = 6 + 1 + 5.008 = 12.008ms
	assert.InDelta(t, 12.008, score, 1e-9)
}

// ============================================================
// Test: ScorePod with zero tokens (empty message)
// ============================================================
// Verifies that an empty message (totalTokens = 0) results in
// both prefix and prefill estimates short-circuit to 0.
// The score reduces to purely the queue component.
func TestConductorScorePod_EmptyMessage(t *testing.T) {
	tok := tokenizer.NewCharacterTokenizer()
	pt := prefixcacheindexer.NewPrefixHashTable()
	mc := newMockMetricCache()

	policy := NewConductorPrefillPolicy(tok, pt, mc, NewConductorPrefillPolicyConfig())

	model := "test-model"
	pod := makeTestPod("pod-1", "default")

	// Set up histogram: sum=0.2s, count=100 -> mean=0.002s = 2ms
	mc.SetHistogram("pod-1", "default", model, metrics.RequestPrefillTimeSeconds, 0.2, 100.0)

	// Empty message -> totalTokens = 0
	ctx := makeRoutingContext(model, "")
	readyMap := map[string]struct{}{"pod-1": {}}

	scorer, err := policy.Prepare(ctx, []*v1.Pod{pod}, readyMap)
	assert.NoError(t, err)

	conductorScorer := scorer.(*conductorScorer)
	assert.Equal(t, 0, conductorScorer.totalTokens)

	score := scorer.ScorePod(pod, 3.0, 10.0)
	assert.InDelta(t, 6.0, score, 1e-9)
}

// ============================================================
// Test: External cache change is reflected in new Prepare() calls
// ============================================================
// KEY TEST: Verifies that when the external cache values change,
// a new Prepare() call picks up the updated values.
// This validates that policy holds a reference to cache (not a snapshot),
// and each scorer gets fresh data from cache at ScorePod time.
func TestConductorScorePod_CacheUpdateReflected(t *testing.T) {
	tok := tokenizer.NewCharacterTokenizer()
	pt := prefixcacheindexer.NewPrefixHashTable()
	mc := newMockMetricCache()

	policy := NewConductorPrefillPolicy(tok, pt, mc, NewConductorPrefillPolicyConfig())

	model := "test-model"
	pod := makeTestPod("pod-1", "default")

	// Phase 1: Initial state - avgPrefillTime = 1.0s
	mc.SetHistogram("pod-1", "default", model, metrics.RequestPrefillTimeSeconds, 0.1, 100.0)

	ctx := makeRoutingContext(model, "hello")
	readyMap := map[string]struct{}{"pod-1": {}}

	scorer1, err := policy.Prepare(ctx, []*v1.Pod{pod}, readyMap)
	assert.NoError(t, err)

	score1 := scorer1.ScorePod(pod, 5.0, 10.0)

	// Phase 2: Update cache - avgPrefillTime becomes 10.0s (10x slower)
	mc.SetHistogram("pod-1", "default", model, metrics.RequestPrefillTimeSeconds, 1.0, 100.0)

	// Create a NEW scorer via Prepare (simulating a new routing request)
	scorer2, err := policy.Prepare(ctx, []*v1.Pod{pod}, readyMap)
	assert.NoError(t, err)

	score2 := scorer2.ScorePod(pod, 5.0, 10.0)

	// Score should be much higher now because avgPrefillTime increased
	// Queue component went from 5*1.0=5.0 to 5*10.0=50.0
	assert.Greater(t, score2, score1)
}
