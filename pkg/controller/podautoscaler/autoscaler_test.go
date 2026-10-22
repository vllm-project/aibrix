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
	"fmt"
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
	ktypes "k8s.io/apimachinery/pkg/types"
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
		sourceType   autoscalingv1alpha1.MetricSourceType
		metricValue  float64
		currentPods  int32
		readyPods    int32
		pendingPods  int32
		wantReason   string
		wantReplicas int32
		wantGuard    bool
	}{
		{
			name:         "kpa scale-up adjusted",
			strategy:     autoscalingv1alpha1.KPA,
			sourceType:   autoscalingv1alpha1.POD,
			metricValue:  250,
			currentPods:  4,
			readyPods:    2,
			pendingPods:  2,
			wantReason:   "panic mode scaling",
			wantReplicas: 10,    // the 2 ready pods alone need ceil(2*250/50)
			wantGuard:    false, // KPA counts only the ready pods' load, so there is nothing to trim
		},
		{
			name:         "kpa scale-up dampened",
			strategy:     autoscalingv1alpha1.KPA,
			sourceType:   autoscalingv1alpha1.POD,
			metricValue:  100,
			currentPods:  4,
			readyPods:    3,
			pendingPods:  1,
			wantReason:   "stable mode scaling",
			wantReplicas: 6,     // the 3 ready pods alone need ceil(3*100/50)
			wantGuard:    false, // KPA counts only the ready pods' load, so there is nothing to trim
		},
		{
			name:         "kpa scale-up keeps what the ready pods need (pod source)",
			strategy:     autoscalingv1alpha1.KPA,
			sourceType:   autoscalingv1alpha1.POD,
			metricValue:  90,
			currentPods:  4,
			readyPods:    3,
			pendingPods:  1,
			wantReason:   "stable mode scaling",
			wantReplicas: 6,
			wantGuard:    false, // KPA counts only the ready pods' load, so there is nothing to trim
		},
		{
			name:         "kpa scale-up keeps what the ready pods need (resource source)",
			strategy:     autoscalingv1alpha1.KPA,
			sourceType:   autoscalingv1alpha1.RESOURCE,
			metricValue:  90,
			currentPods:  4,
			readyPods:    3,
			pendingPods:  1,
			wantReason:   "stable mode scaling",
			wantReplicas: 6,
			wantGuard:    false, // KPA counts only the ready pods' load, so there is nothing to trim
		},
		{
			name:         "apa gives the same answer for the same input",
			strategy:     autoscalingv1alpha1.APA,
			sourceType:   autoscalingv1alpha1.POD,
			metricValue:  90,
			currentPods:  4,
			readyPods:    3,
			pendingPods:  1,
			wantReason:   "scale-up adjusted: pending replicas treated as missing metrics",
			wantReplicas: 6,
			wantGuard:    true,
		},
		{
			name:         "kpa scale-down adjusted",
			strategy:     autoscalingv1alpha1.KPA,
			sourceType:   autoscalingv1alpha1.POD,
			metricValue:  10,
			currentPods:  10,
			readyPods:    5,
			pendingPods:  5,
			wantReason:   "scale-down adjusted: pending replicas treated at target utilization",
			wantReplicas: 6,
			wantGuard:    true,
		},
		{
			name:         "kpa external metric not adjusted",
			strategy:     autoscalingv1alpha1.KPA,
			sourceType:   autoscalingv1alpha1.EXTERNAL,
			metricValue:  350,
			currentPods:  4,
			readyPods:    2,
			pendingPods:  2,
			wantReason:   "stable mode scaling",
			wantReplicas: 7,
			wantGuard:    false,
		},
		{
			name:         "apa scale-up adjusted",
			strategy:     autoscalingv1alpha1.APA,
			sourceType:   autoscalingv1alpha1.POD,
			metricValue:  90,
			currentPods:  4,
			readyPods:    2,
			pendingPods:  2,
			wantReason:   "scale-up adjusted: pending replicas treated as missing metrics",
			wantReplicas: 4,
			wantGuard:    true,
		},
		{
			name:         "apa scale-down adjusted",
			strategy:     autoscalingv1alpha1.APA,
			sourceType:   autoscalingv1alpha1.POD,
			metricValue:  10,
			currentPods:  4,
			readyPods:    2,
			pendingPods:  2,
			wantReason:   "scale-down adjusted: pending replicas treated at target utilization",
			wantReplicas: 3,
			wantGuard:    true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pa := testPodAutoscaler(tt.strategy)
			pa.Spec.MetricsSources[0].MetricSourceType = tt.sourceType
			autoScaler := NewDefaultAutoScaler(&mockMetricFetcherFactory{
				mockMetricFetcher: mockMetricFetcher{metricsValue: tt.metricValue},
			}, fake.NewClientBuilder().Build())
			scalingContext := testScalingContext(t, &pa)

			result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
				PodAutoscaler:   pa,
				ScalingContext:  scalingContext,
				CurrentReplicas: tt.currentPods,
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
			assert.Equal(t, tt.wantGuard, result.PendingReplicaGuardActive)
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
	// unscrapable pods fail to report, as a pod that is not serving yet does.
	unscrapable map[string]bool
}

func (f *mockMetricFetcher) FetchPodMetrics(ctx context.Context, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	if f.unscrapable[pod.Name] {
		return 0, fmt.Errorf("pod %s is not serving", pod.Name)
	}
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

// The scaling context is rebuilt on every reconcile, so the KPA panic window
// has to be kept by the autoscaler. A panic must outlast the surge that started
// it, and deleting the PodAutoscaler must drop it.
func TestComputeDesiredReplicasKeepsKPAPanicAcrossReconciles(t *testing.T) {
	pa := testPodAutoscaler(autoscalingv1alpha1.KPA)
	pa.Spec.ObserveWindowSeconds = ptr.To[int64](120)
	pa.Spec.PanicWindowSeconds = ptr.To[int64](20)
	fetcher := &mockMetricFetcherFactory{}
	autoScaler := NewDefaultAutoScaler(fetcher, nil)
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	// totalLoad is spread over the pods, as a per-pod metric would be.
	compute := func(at time.Duration, pods int32, totalLoad float64) *ReplicaComputeResult {
		t.Helper()
		fetcher.metricsValue = totalLoad / float64(pods)
		podList := make([]corev1.Pod, pods)
		for i := range podList {
			podList[i].Name = fmt.Sprintf("pod-%d", i)
		}
		result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
			PodAutoscaler:   pa,
			ScalingContext:  testScalingContext(t, &pa), // fresh, as in the controller
			CurrentReplicas: pods,
			Pods:            podList,
			Timestamp:       start.Add(at),
		})
		require.NoError(t, err)
		return result
	}

	for at := time.Duration(0); at < 60*time.Second; at += 10 * time.Second {
		require.Equal(t, "stable mode scaling", compute(at, 4, 200).Reason)
	}
	surge := compute(60*time.Second, 4, 1600)
	require.Equal(t, "panic mode scaling", surge.Reason)

	// The surge is over and out of the panic window. The panic continues for
	// a stable window after it, and never recommends fewer pods.
	after := compute(90*time.Second, surge.DesiredReplicas, 200)
	assert.Equal(t, "panic mode scaling", after.Reason)
	assert.GreaterOrEqual(t, after.DesiredReplicas, surge.DesiredReplicas)

	autoScaler.ForgetPodAutoscaler(pa.Namespace, pa.Name)
	assert.Empty(t, autoScaler.panicStates)
}

// A per-pod metric falls as pods are added while the total load stays the same.
// Samples taken before a scale-up must not count for more after it: with a
// steady load that 10 pods carry at target, KPA and APA have to settle on 10
// rather than keep scaling up on the samples from when there were fewer pods.
// startup is how long a new pod takes to serve; until then it fails to report.
func TestComputeDesiredReplicasSettlesUnderSteadyLoad(t *testing.T) {
	for _, tt := range []struct {
		strategy autoscalingv1alpha1.ScalingStrategyType
		startup  time.Duration
	}{
		{autoscalingv1alpha1.KPA, 0},
		{autoscalingv1alpha1.APA, 0},
		{autoscalingv1alpha1.KPA, 90 * time.Second},
		{autoscalingv1alpha1.APA, 90 * time.Second},
	} {
		t.Run(fmt.Sprintf("%s/startup_%s", tt.strategy, tt.startup), func(t *testing.T) {
			pa := testPodAutoscaler(tt.strategy)
			pa.Spec.MaxReplicas = 100
			fetcher := &mockMetricFetcherFactory{}
			autoScaler := NewDefaultAutoScaler(fetcher, nil)
			start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

			const totalLoad = 500 // 10 pods at the target of 50
			var servingAt []time.Duration
			for range 4 {
				servingAt = append(servingAt, 0)
			}
			for at := time.Duration(0); at <= 10*time.Minute; at += 15 * time.Second {
				podList := make([]corev1.Pod, len(servingAt))
				fetcher.unscrapable = map[string]bool{}
				var ready int32
				for i, servesAt := range servingAt {
					podList[i].Name = fmt.Sprintf("pod-%d", i)
					if servesAt <= at {
						ready++
					} else {
						fetcher.unscrapable[podList[i].Name] = true
					}
				}
				fetcher.metricsValue = totalLoad / float64(ready)

				scalingContext := scalingctx.NewBaseScalingContext()
				scalingContext.MaxReplicas = 100
				scalingContext.MaxScaleUpRate = 4
				require.NoError(t, scalingContext.UpdateByPaTypes(&pa))
				current := int32(len(servingAt))
				result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
					PodAutoscaler:   pa,
					ScalingContext:  scalingContext,
					CurrentReplicas: current,
					ReplicaState:    ReplicaState{ReadyReplicas: ready, PendingReplicas: current - ready},
					Pods:            podList,
					Timestamp:       start.Add(at),
				})
				require.NoError(t, err)
				require.LessOrEqual(t, result.DesiredReplicas, int32(10), "at %s with %d pods, %d ready", at, current, ready)
				for range result.DesiredReplicas - current {
					servingAt = append(servingAt, at+tt.startup)
				}
				servingAt = servingAt[:result.DesiredReplicas]
			}
			assert.Len(t, servingAt, 10)
		})
	}
}

// A ready pod whose scrape fails is still serving, so the windows must not
// record a total that drops whenever some pods miss a scrape.
func TestComputeDesiredReplicasAssumesMeanLoadForUnscrapedReadyPod(t *testing.T) {
	pa := testPodAutoscaler(autoscalingv1alpha1.KPA)
	fetcher := &mockMetricFetcherFactory{}
	fetcher.metricsValue = 50 // at target
	autoScaler := NewDefaultAutoScaler(fetcher, nil)
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	podList := make([]corev1.Pod, 10)
	for i := range podList {
		podList[i].Name = fmt.Sprintf("pod-%d", i)
	}
	for i := range 12 {
		// Four of the pods miss every other scrape.
		fetcher.unscrapable = map[string]bool{}
		for _, pod := range podList[:4] {
			fetcher.unscrapable[pod.Name] = i%2 == 1
		}
		result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
			PodAutoscaler:   pa,
			ScalingContext:  testScalingContext(t, &pa),
			CurrentReplicas: 10,
			ReplicaState:    ReplicaState{ReadyReplicas: 10},
			Pods:            podList,
			Timestamp:       start.Add(time.Duration(i) * 15 * time.Second),
		})
		require.NoError(t, err)
		require.Equal(t, int32(10), result.DesiredReplicas, "reconcile %d", i)
	}
}

// With no pods there is no load to spread, and the windowed total must not be
// divided by zero: the reported metric has to stay a number.
func TestComputeDesiredReplicasReportsFiniteMetricWithNoPods(t *testing.T) {
	for _, strategy := range []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.KPA, autoscalingv1alpha1.APA} {
		t.Run(string(strategy), func(t *testing.T) {
			pa := testPodAutoscaler(strategy)
			autoScaler := NewDefaultAutoScaler(&mockMetricFetcherFactory{}, nil)

			result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
				PodAutoscaler:  pa,
				ScalingContext: testScalingContext(t, &pa),
				Timestamp:      time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
			assert.Equal(t, int32(0), result.DesiredReplicas)
			assert.Equal(t, 0.0, result.MetricValue)
		})
	}
}

// status.readyReplicas can lag the pod list after a scale-up. The pods that
// report are serving, so the total must count all of them, not only the ready
// count the status still shows.
func TestComputeDesiredReplicasCountsReportingPodsWhenReadyReplicasLag(t *testing.T) {
	for _, strategy := range []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.KPA, autoscalingv1alpha1.APA} {
		t.Run(string(strategy), func(t *testing.T) {
			pa := testPodAutoscaler(strategy)
			fetcher := &mockMetricFetcherFactory{}
			autoScaler := NewDefaultAutoScaler(fetcher, nil)
			start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

			const totalLoad = 500 // 10 pods at the target of 50
			for i := range 16 {
				pods, ready := int32(10), int32(10)
				if i < 4 {
					pods, ready = 4, 4
				} else if i < 8 {
					ready = 4 // all 10 pods report, the status still shows 4
				}
				podList := make([]corev1.Pod, pods)
				for j := range podList {
					podList[j].Name = fmt.Sprintf("pod-%d", j)
				}
				fetcher.metricsValue = totalLoad / float64(pods)
				result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
					PodAutoscaler:   pa,
					ScalingContext:  testScalingContext(t, &pa),
					CurrentReplicas: pods,
					ReplicaState:    ReplicaState{ReadyReplicas: ready},
					Pods:            podList,
					Timestamp:       start.Add(time.Duration(i) * 15 * time.Second),
				})
				require.NoError(t, err)
				require.Equal(t, int32(10), result.DesiredReplicas, "reconcile %d with %d pods, %d ready", i, pods, ready)
			}
		})
	}
}

// KPA sizes from every pod that reports, not only from readyReplicas, which can
// lag the pod list: 10 pods reporting 100 each against a target of 50 need 20,
// even while the status still shows 4 ready.
func TestComputeDesiredReplicasKPAScalesFromReportingPodsWhenReadyReplicasLag(t *testing.T) {
	pa := testPodAutoscaler(autoscalingv1alpha1.KPA)
	fetcher := &mockMetricFetcherFactory{}
	fetcher.metricsValue = 100
	autoScaler := NewDefaultAutoScaler(fetcher, nil)

	podList := make([]corev1.Pod, 10)
	for j := range podList {
		podList[j].Name = fmt.Sprintf("pod-%d", j)
	}
	result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
		PodAutoscaler:   pa,
		ScalingContext:  testScalingContext(t, &pa),
		CurrentReplicas: 10,
		ReplicaState:    ReplicaState{ReadyReplicas: 4},
		Pods:            podList,
		Timestamp:       time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, int32(20), result.DesiredReplicas)
}

// Inside the tolerance band KPA holds the current replicas, pods still starting
// included: 4 ready pods at the target with 6 more starting must stay at 10, not
// fall to the 4 that carry the load.
func TestComputeDesiredReplicasKPAHoldsPendingPodsWithinTolerance(t *testing.T) {
	pa := testPodAutoscaler(autoscalingv1alpha1.KPA)
	fetcher := &mockMetricFetcherFactory{}
	fetcher.metricsValue = 50
	autoScaler := NewDefaultAutoScaler(fetcher, nil)

	podList := make([]corev1.Pod, 4)
	for j := range podList {
		podList[j].Name = fmt.Sprintf("pod-%d", j)
	}
	result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
		PodAutoscaler:   pa,
		ScalingContext:  testScalingContext(t, &pa),
		CurrentReplicas: 10,
		ReplicaState:    ReplicaState{ReadyReplicas: 4},
		Pods:            podList,
		Timestamp:       time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, int32(10), result.DesiredReplicas)
	assert.False(t, result.PendingReplicaGuardActive, "the hold must come from KPA, not from the pending-replica guard")
}

// External and domain sources report one total, so their windows must not be
// rescaled by the pod count: a steady external load needs the same replicas
// before and after the ready pods change.
func TestComputeDesiredReplicasLeavesExternalSourceWindowsUnscaled(t *testing.T) {
	for _, strategy := range []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.KPA, autoscalingv1alpha1.APA} {
		t.Run(string(strategy), func(t *testing.T) {
			pa := testPodAutoscaler(strategy)
			pa.Spec.MetricsSources[0].MetricSourceType = autoscalingv1alpha1.EXTERNAL
			fetcher := &mockMetricFetcherFactory{}
			fetcher.metricsValue = 500 // 10 pods at the target of 50
			autoScaler := NewDefaultAutoScaler(fetcher, nil)
			start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

			for i := range 16 {
				pods := int32(4)
				if i >= 4 {
					pods = 10
				}
				podList := make([]corev1.Pod, pods)
				for j := range podList {
					podList[j].Name = fmt.Sprintf("pod-%d", j)
				}
				result, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
					PodAutoscaler:   pa,
					ScalingContext:  testScalingContext(t, &pa),
					CurrentReplicas: pods,
					ReplicaState:    ReplicaState{ReadyReplicas: pods},
					Pods:            podList,
					Timestamp:       start.Add(time.Duration(i) * 15 * time.Second),
				})
				require.NoError(t, err)
				require.Equal(t, int32(10), result.DesiredReplicas, "reconcile %d with %d pods", i, pods)
			}
		})
	}
}

// Editing a PodAutoscaler's spec must not leave panic state behind until the
// PodAutoscaler is deleted: a removed metric loses its state, and so does a
// PodAutoscaler that stops using KPA.
func TestComputeDesiredReplicasPrunesStalePanicStates(t *testing.T) {
	pa := testPodAutoscaler(autoscalingv1alpha1.KPA)
	fetcher := &mockMetricFetcherFactory{}
	fetcher.metricsValue = 10
	autoScaler := NewDefaultAutoScaler(fetcher, nil)
	key := ktypes.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}

	compute := func() {
		t.Helper()
		_, err := autoScaler.ComputeDesiredReplicas(context.TODO(), ReplicaComputeRequest{
			PodAutoscaler:   pa,
			ScalingContext:  testScalingContext(t, &pa),
			CurrentReplicas: 1,
			Pods:            []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod-0"}}},
			Timestamp:       time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
	}

	removed := "removed_metric"
	pa.Spec.MetricsSources = append(pa.Spec.MetricsSources, autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.POD,
		TargetMetric:     removed,
		TargetValue:      "50",
	})
	compute()
	require.Contains(t, autoScaler.panicStates[key], removed)

	pa.Spec.MetricsSources = pa.Spec.MetricsSources[:1]
	compute()
	assert.NotContains(t, autoScaler.panicStates[key], removed)
	assert.Contains(t, autoScaler.panicStates[key], pa.Spec.MetricsSources[0].TargetMetric)

	pa.Spec.ScalingStrategy = autoscalingv1alpha1.APA
	compute()
	assert.NotContains(t, autoScaler.panicStates, key)
}
