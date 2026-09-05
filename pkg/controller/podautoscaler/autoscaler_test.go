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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/algorithm"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
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

func TestComputeDesiredReplicasConfiguresMetricWindowsFromPodAutoscalerSpec(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(scheme)

	pa := autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-llm-kpa",
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "test-llm",
			},
			MaxReplicas:          10,
			ScalingStrategy:      autoscalingv1alpha1.KPA,
			ObserveWindowSeconds: ptr.To[int64](120),
			PanicWindowSeconds:   ptr.To[int64](30),
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					TargetMetric:     "gpu_cache_usage_perc",
					TargetValue:      "50",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&pa).
		Build()
	mockFactory := &mockMetricFetcherFactory{
		mockMetricFetcher: mockMetricFetcher{
			metricsValue: 10.0,
		},
	}
	autoScaler := NewDefaultAutoScaler(mockFactory, fakeClient)

	scalingContext := scalingctx.NewBaseScalingContext()
	scalingContext.MaxReplicas = 10
	require.NoError(t, scalingContext.UpdateByPaTypes(&pa))

	request := ReplicaComputeRequest{
		PodAutoscaler:   pa,
		ScalingContext:  scalingContext,
		CurrentReplicas: 1,
		Pods: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
			},
		},
		Timestamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	_, err := autoScaler.ComputeDesiredReplicas(context.TODO(), request)
	require.NoError(t, err)

	mockFactory.metricsValue = 70.0
	request.Timestamp = request.Timestamp.Add(60 * time.Second)
	_, err = autoScaler.ComputeDesiredReplicas(context.TODO(), request)
	require.NoError(t, err)

	metricKey := types.MetricKey{
		Namespace:   "default",
		Name:        "test-llm",
		MetricName:  "gpu_cache_usage_perc",
		PaNamespace: "default",
		PaName:      "test-llm-kpa",
	}
	stableValue, panicValue, err := autoScaler.metricsClient.GetMetricValue(metricKey, request.Timestamp)
	require.NoError(t, err)
	assert.Equal(t, 40.0, stableValue)
	assert.Equal(t, 70.0, panicValue)
}

func TestComputeDesiredReplicasAdjustsKPAAPAConservativelyWhenReplicasPending(t *testing.T) {
	for _, tt := range []struct {
		name         string
		strategy     autoscalingv1alpha1.ScalingStrategyType
		metricValue  float64
		readyPods    int32
		pendingPods  int32
		wantReason   string
		wantReplicas int32
	}{
		{
			name:         "kpa scale-up adjusted",
			strategy:     autoscalingv1alpha1.KPA,
			metricValue:  250,
			readyPods:    2,
			pendingPods:  2,
			wantReason:   "scale-up adjusted: pending replicas treated as missing metrics",
			wantReplicas: 4,
		},
		{
			name:         "kpa scale-up dampened",
			strategy:     autoscalingv1alpha1.KPA,
			metricValue:  350,
			readyPods:    3,
			pendingPods:  1,
			wantReason:   "scale-up adjusted: pending replicas treated as missing metrics",
			wantReplicas: 6,
		},
		{
			name:         "apa scale-up adjusted",
			strategy:     autoscalingv1alpha1.APA,
			metricValue:  90,
			readyPods:    2,
			pendingPods:  2,
			wantReason:   "scale-up adjusted: pending replicas treated as missing metrics",
			wantReplicas: 4,
		},
		{
			name:         "apa scale-down adjusted",
			strategy:     autoscalingv1alpha1.APA,
			metricValue:  10,
			readyPods:    2,
			pendingPods:  2,
			wantReason:   "scale-down adjusted: pending replicas treated at target utilization",
			wantReplicas: 3,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pa := testPodAutoscaler(tt.strategy)
			autoScaler := NewDefaultAutoScaler(&mockMetricFetcherFactory{
				mockMetricFetcher: mockMetricFetcher{metricsValue: tt.metricValue},
			}, fake.NewClientBuilder().Build())
			scalingContext := testScalingContext(t, &pa)

			result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
				PodAutoscaler:   pa,
				ScalingContext:  scalingContext,
				CurrentReplicas: 4,
				ReplicaState: ReplicaState{
					ReadyReplicas:   tt.readyPods,
					PendingReplicas: tt.pendingPods,
				},
				Pods: []corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "ready-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "ready-2"}},
				},
				Timestamp: time.Now(),
			})

			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, tt.wantReplicas, result.DesiredReplicas)
			assert.Equal(t, tt.wantReason, result.Reason)
			assert.True(t, result.PendingReplicaGuardActive)
		})
	}
}

func TestComputeDesiredReplicasDoesNotHoldWhenNoReplicasPending(t *testing.T) {
	pa := testPodAutoscaler(autoscalingv1alpha1.APA)
	autoScaler := NewDefaultAutoScaler(&mockMetricFetcherFactory{
		mockMetricFetcher: mockMetricFetcher{metricsValue: 90},
	}, fake.NewClientBuilder().Build())
	scalingContext := testScalingContext(t, &pa)

	result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
		PodAutoscaler:   pa,
		ScalingContext:  scalingContext,
		CurrentReplicas: 4,
		ReplicaState: ReplicaState{
			ReadyReplicas:   4,
			PendingReplicas: 0,
		},
		Pods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "ready-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ready-2"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ready-3"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ready-4"}},
		},
		Timestamp: time.Now(),
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, int32(8), result.DesiredReplicas)
	assert.Equal(t, "apa scaling based on current metrics", result.Reason)
}

func TestComputeDesiredReplicasDoesNotHoldHPAWhenReplicasPending(t *testing.T) {
	pa := testPodAutoscaler(autoscalingv1alpha1.HPA)
	autoScaler := NewDefaultAutoScaler(&mockMetricFetcherFactory{
		mockMetricFetcher: mockMetricFetcher{metricsValue: 90},
	}, fake.NewClientBuilder().Build())
	scalingContext := testScalingContext(t, &pa)

	result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
		PodAutoscaler:   pa,
		ScalingContext:  scalingContext,
		CurrentReplicas: 4,
		ReplicaState: ReplicaState{
			ReadyReplicas:   2,
			PendingReplicas: 2,
		},
		Pods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "ready-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ready-2"}},
		},
		Timestamp: time.Now(),
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, int32(4), result.DesiredReplicas)
	assert.Equal(t, "HPA managed by Kubernetes", result.Reason)
}

func TestRecommendationMetricValueConvertsNumericMetadata(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  interface{}
		want float64
	}{
		{name: "float64", raw: 12.5, want: 12.5},
		{name: "float32", raw: float32(12.5), want: 12.5},
		{name: "int", raw: 12, want: 12},
		{name: "int64", raw: int64(12), want: 12},
		{name: "json number", raw: json.Number("12.5"), want: 12.5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recommendation := &algorithm.ScalingRecommendation{
				Metadata: map[string]interface{}{
					"current_value": tt.raw,
				},
			}

			assert.Equal(t, tt.want, recommendationMetricValue(recommendation))
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

func testPodAutoscaler(strategy autoscalingv1alpha1.ScalingStrategyType) autoscalingv1alpha1.PodAutoscaler {
	return autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-pending-guard",
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "test-llm",
			},
			MaxReplicas:     20,
			ScalingStrategy: strategy,
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					TargetMetric:     "gpu_cache_usage_perc",
					TargetValue:      "50",
				},
			},
		},
	}
}

func testScalingContext(t *testing.T, pa *autoscalingv1alpha1.PodAutoscaler) scalingctx.ScalingContext {
	t.Helper()

	scalingContext := scalingctx.NewBaseScalingContext()
	scalingContext.MaxReplicas = 20
	scalingContext.MaxScaleUpRate = 4
	require.NoError(t, scalingContext.UpdateByPaTypes(pa))
	return scalingContext
}
