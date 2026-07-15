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

package podautoscaler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestComputeDesiredReplicas(t *testing.T) {
	table := []struct {
		name             string
		metricsSources   []autoscalingv1alpha1.MetricSource
		expectedReplicas int32
	}{
		{
			name: "with one metrics source",
			metricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					TargetMetric:     "gpu_cache_usage_perc",
					TargetValue:      "50",
				},
			},
			expectedReplicas: 2,
		},
		{
			name: "with multiple metrics source",
			metricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					TargetMetric:     "gpu_cache_usage_perc",
					TargetValue:      "50",
				},
				{
					MetricSourceType: autoscalingv1alpha1.RESOURCE,
					TargetMetric:     "cpu",
					TargetValue:      "30",
				},
			},
			expectedReplicas: 3,
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = autoscalingv1alpha1.AddToScheme(scheme)

			pa := autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test-llm-apa",
				},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					MetricsSources: tt.metricsSources,
					ScaleTargetRef: corev1.ObjectReference{
						Kind: "Deployment",
						Name: "test-llm",
					},
					ScalingStrategy: "APA",
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(&pa).
				Build()

			mockFactory := &mockMetricFetcherFactory{
				mockMetricFetcher: mockMetricFetcher{
					metricsValue: 70.0,
				},
			}

			autoScaler := NewDefaultAutoScaler(mockFactory, fakeClient)

			// set context fields for APA strategy
			ctx := scalingctx.NewBaseScalingContext()
			ctx.MaxReplicas = 6
			ctx.MaxScaleUpRate = 4
			err := ctx.UpdateByPaTypes(&pa)
			require.NoError(t, err)

			request := ReplicaComputeRequest{
				PodAutoscaler:   pa,
				ScalingContext:  ctx,
				CurrentReplicas: 1,
				Pods: []corev1.Pod{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "pod-1",
						},
					},
				},
				Timestamp: time.Now(),
			}

			result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), request)

			assert.NoError(t, err)
			assert.NotNil(t, result)
			assert.Equal(t, tt.expectedReplicas, result.DesiredReplicas)
		})
	}
}

type mockMetricFetcherFactory struct {
	mockMetricFetcher
}

func (f *mockMetricFetcherFactory) For(source autoscalingv1alpha1.MetricSource) metrics.MetricFetcher {
	return &f.mockMetricFetcher
}

type mockMetricFetcher struct {
	metricsValue float64
}

func (f *mockMetricFetcher) FetchPodMetrics(ctx context.Context, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	return f.metricsValue, nil
}

func TestComputeDesiredReplicasHealth(t *testing.T) {
	sourceA := autoscalingv1alpha1.MetricSource{MetricSourceType: autoscalingv1alpha1.POD, TargetMetric: "queue_a", TargetValue: "50"}
	sourceB := autoscalingv1alpha1.MetricSource{MetricSourceType: autoscalingv1alpha1.POD, TargetMetric: "queue_b", TargetValue: "50"}
	fetchErr := errors.New("fetch failed")

	tests := []struct {
		name          string
		sources       []autoscalingv1alpha1.MetricSource
		fetch         map[string]map[string]metricFetchOutcome
		aggregated    map[string]*types.AggregatedMetrics
		wantDesired   int32
		wantHasResult bool
		wantHealthy   int
		wantDegraded  int
		wantFailed    int
		wantReason    string
	}{
		{
			name:    "two healthy sources use maximum recommendation",
			sources: []autoscalingv1alpha1.MetricSource{sourceA, sourceB},
			fetch: map[string]map[string]metricFetchOutcome{
				"queue_a": {"pod-1": {value: 70}},
				"queue_b": {"pod-1": {value: 120}},
			},
			aggregated: map[string]*types.AggregatedMetrics{
				"queue_a": {StableValue: 70, PanicValue: 70, Confidence: 1},
				"queue_b": {StableValue: 120, PanicValue: 120, Confidence: 1},
			},
			wantDesired:   3,
			wantHasResult: true,
			wantHealthy:   2,
			wantReason:    types.MetricReasonMetricsHealthy,
		},
		{
			name:    "healthy and degraded sources keep valid maximum recommendation",
			sources: []autoscalingv1alpha1.MetricSource{sourceA, sourceB},
			fetch: map[string]map[string]metricFetchOutcome{
				"queue_a": {"pod-1": {value: 120}, "pod-2": {value: 120}},
				"queue_b": {"pod-1": {value: 70}, "pod-2": {err: fetchErr}},
			},
			aggregated: map[string]*types.AggregatedMetrics{
				"queue_a": {StableValue: 120, PanicValue: 120, Confidence: 1},
				"queue_b": {StableValue: 70, PanicValue: 70, Confidence: 1},
			},
			wantDesired:   3,
			wantHasResult: true,
			wantHealthy:   1,
			wantDegraded:  1,
			wantReason:    types.MetricReasonPartialMetricCollectionFailure,
		},
		{
			name:    "healthy and failed sources keep healthy recommendation",
			sources: []autoscalingv1alpha1.MetricSource{sourceA, sourceB},
			fetch: map[string]map[string]metricFetchOutcome{
				"queue_a": {"pod-1": {value: 70}},
				"queue_b": {"pod-1": {err: fetchErr}},
			},
			aggregated: map[string]*types.AggregatedMetrics{
				"queue_a": {StableValue: 70, PanicValue: 70, Confidence: 1},
			},
			wantDesired:   2,
			wantHasResult: true,
			wantHealthy:   1,
			wantFailed:    1,
			wantReason:    types.MetricReasonAllMetricSamplesUnavailable,
		},
		{
			name:    "all failed sources return round without recommendation",
			sources: []autoscalingv1alpha1.MetricSource{sourceA, sourceB},
			fetch: map[string]map[string]metricFetchOutcome{
				"queue_a": {"pod-1": {err: fetchErr}},
				"queue_b": {"pod-1": {err: fetchErr}},
			},
			wantHasResult: false,
			wantFailed:    2,
			wantReason:    types.MetricReasonAllMetricSourcesFailed,
		},
		{
			name:    "invalid aggregated value fails source without calling algorithm",
			sources: []autoscalingv1alpha1.MetricSource{sourceA},
			fetch: map[string]map[string]metricFetchOutcome{
				"queue_a": {"pod-1": {value: 70}},
			},
			aggregated: map[string]*types.AggregatedMetrics{
				"queue_a": {StableValue: math.NaN(), PanicValue: math.NaN(), Confidence: 1},
			},
			wantHasResult: false,
			wantFailed:    1,
			wantReason:    types.MetricReasonAggregatedMetricInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			autoScaler, request := newHealthTestAutoScaler(t, tt.sources, tt.fetch, tt.aggregated, true)

			result, err := autoScaler.ComputeDesiredReplicas(context.Background(), request)

			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, len(tt.sources), result.TotalSourceCount)
			assert.Equal(t, tt.wantHealthy, result.HealthySourceCount)
			assert.Equal(t, tt.wantDegraded, result.DegradedSourceCount)
			assert.Equal(t, tt.wantFailed, result.FailedSourceCount)
			assert.Equal(t, tt.wantHasResult, result.HasRecommendation)
			assert.Equal(t, tt.wantReason, result.HealthReason)
			assert.Len(t, result.SourceResults, len(tt.sources))
			if tt.wantHasResult {
				assert.True(t, result.Valid)
				assert.Equal(t, tt.wantDesired, result.DesiredReplicas)
			} else {
				assert.False(t, result.Valid)
			}
		})
	}
}

func TestComputeDesiredReplicasCompatibility(t *testing.T) {
	sourceA := autoscalingv1alpha1.MetricSource{MetricSourceType: autoscalingv1alpha1.POD, TargetMetric: "queue_a", TargetValue: "50"}
	sourceB := autoscalingv1alpha1.MetricSource{MetricSourceType: autoscalingv1alpha1.POD, TargetMetric: "queue_b", TargetValue: "50"}
	fetchErr := errors.New("fetch failed")

	t.Run("disabled circuit breaker keeps all sources failed error", func(t *testing.T) {
		autoScaler, request := newHealthTestAutoScaler(t, []autoscalingv1alpha1.MetricSource{sourceA, sourceB}, map[string]map[string]metricFetchOutcome{
			"queue_a": {"pod-1": {err: fetchErr}},
			"queue_b": {"pod-1": {err: fetchErr}},
		}, nil, false)

		result, err := autoScaler.ComputeDesiredReplicas(context.Background(), request)

		require.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "all 2 metric sources failed for PodAutoscaler default/test-llm-apa")
	})

	t.Run("disabled circuit breaker keeps partial failure recommendation", func(t *testing.T) {
		autoScaler, request := newHealthTestAutoScaler(t, []autoscalingv1alpha1.MetricSource{sourceA, sourceB}, map[string]map[string]metricFetchOutcome{
			"queue_a": {"pod-1": {value: 70}},
			"queue_b": {"pod-1": {err: fetchErr}},
		}, map[string]*types.AggregatedMetrics{
			"queue_a": {StableValue: 70, PanicValue: 70, Confidence: 1},
		}, false)

		result, err := autoScaler.ComputeDesiredReplicas(context.Background(), request)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.Valid)
		assert.Equal(t, int32(2), result.DesiredReplicas)
	})

	t.Run("internal aggregator error is returned", func(t *testing.T) {
		autoScaler, request := newHealthTestAutoScaler(t, []autoscalingv1alpha1.MetricSource{sourceA}, map[string]map[string]metricFetchOutcome{
			"queue_a": {"pod-1": {value: 70}},
		}, nil, false)
		autoScaler.aggregator = roundHealthAggregator{getErr: errors.New("window unavailable")}

		result, err := autoScaler.ComputeDesiredReplicas(context.Background(), request)

		require.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "all 1 metric sources failed for PodAutoscaler default/test-llm-apa")
	})

	t.Run("enabled circuit breaker converts source computation error to failed round", func(t *testing.T) {
		autoScaler, request := newHealthTestAutoScaler(t, []autoscalingv1alpha1.MetricSource{sourceA}, map[string]map[string]metricFetchOutcome{
			"queue_a": {"pod-1": {value: 70}},
		}, nil, true)
		autoScaler.aggregator = roundHealthAggregator{getErr: errors.New("window unavailable")}

		result, err := autoScaler.ComputeDesiredReplicas(context.Background(), request)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.False(t, result.Valid)
		assert.False(t, result.HasRecommendation)
		assert.Equal(t, 1, result.TotalSourceCount)
		assert.Equal(t, 1, result.FailedSourceCount)
		assert.Equal(t, types.MetricReasonAllMetricSourcesFailed, result.HealthReason)
		assert.Len(t, result.SourceResults, 1)
		assert.Equal(t, types.MetricHealthStateFailed, result.SourceResults[0].Health.State)
	})
}

type metricFetchOutcome struct {
	value float64
	err   error
}

type metricNameFetcherFactory struct {
	results map[string]map[string]metricFetchOutcome
}

func (f metricNameFetcherFactory) For(source autoscalingv1alpha1.MetricSource) metrics.MetricFetcher {
	return metricNameFetcher{source: source.TargetMetric, results: f.results[source.TargetMetric]}
}

type metricNameFetcher struct {
	source  string
	results map[string]metricFetchOutcome
}

func (f metricNameFetcher) FetchPodMetrics(_ context.Context, pod corev1.Pod, _ autoscalingv1alpha1.MetricSource) (float64, error) {
	result, ok := f.results[pod.Name]
	if !ok {
		return 0, fmt.Errorf("missing metric result for %s/%s", f.source, pod.Name)
	}
	return result.value, result.err
}

type roundHealthAggregator struct {
	values map[string]*types.AggregatedMetrics
	getErr error
}

func (a roundHealthAggregator) ProcessSnapshot(metricKey types.MetricKey, snapshot *types.MetricSnapshot) error {
	if snapshot.Error != nil {
		return snapshot.Error
	}
	return nil
}

func (a roundHealthAggregator) GetAggregatedMetrics(key types.MetricKey, now time.Time) (*types.AggregatedMetrics, error) {
	if a.getErr != nil {
		return nil, a.getErr
	}
	value := a.values[key.MetricName]
	if value == nil {
		return nil, fmt.Errorf("missing aggregated metric for %s", key.MetricName)
	}
	out := *value
	out.MetricKey = key
	out.LastUpdated = now
	return &out, nil
}

func newHealthTestAutoScaler(
	t *testing.T,
	sources []autoscalingv1alpha1.MetricSource,
	fetch map[string]map[string]metricFetchOutcome,
	aggregated map[string]*types.AggregatedMetrics,
	circuitBreakerEnabled bool,
) (*DefaultAutoScaler, ReplicaComputeRequest) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, autoscalingv1alpha1.AddToScheme(scheme))

	pa := autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-llm-apa"},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources:  sources,
			ScalingStrategy: autoscalingv1alpha1.APA,
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "test-llm",
			},
			CircuitBreaker: &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: circuitBreakerEnabled},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&pa).Build()
	autoScaler := NewDefaultAutoScaler(metricNameFetcherFactory{results: fetch}, fakeClient)
	autoScaler.aggregator = roundHealthAggregator{values: aggregated}

	ctx := scalingctx.NewBaseScalingContext()
	ctx.MaxReplicas = 6
	ctx.MaxScaleUpRate = 4
	require.NoError(t, ctx.UpdateByPaTypes(&pa))

	podNames := map[string]struct{}{}
	for _, byPod := range fetch {
		for podName := range byPod {
			podNames[podName] = struct{}{}
		}
	}
	pods := make([]corev1.Pod, 0, len(podNames))
	for podName := range podNames {
		if strings.TrimSpace(podName) == "" {
			continue
		}
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "default"}})
	}

	return autoScaler, ReplicaComputeRequest{
		PodAutoscaler:   pa,
		ScalingContext:  ctx,
		CurrentReplicas: 1,
		Pods:            pods,
		Timestamp:       time.Now(),
	}
}
