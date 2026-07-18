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
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	patypes "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type recordingMetricFetcher struct {
	namespace string
}

func (f *recordingMetricFetcher) FetchPodMetrics(ctx context.Context, pod corev1.Pod, source autoscalingv1alpha1.MetricSource) (float64, error) {
	f.namespace = pod.Namespace
	return 42, nil
}

type staticMetricFetcherFactory struct {
	fetcher MetricFetcher
}

func (f staticMetricFetcherFactory) For(source autoscalingv1alpha1.MetricSource) MetricFetcher {
	return f.fetcher
}

func TestCollectMetricsExternalUsesCollectionNamespace(t *testing.T) {
	fetcher := &recordingMetricFetcher{}
	spec := patypes.CollectionSpec{
		Namespace:  "tenant-a",
		TargetName: "worker",
		MetricName: "queue_depth",
		MetricSource: autoscalingv1alpha1.MetricSource{
			MetricSourceType: autoscalingv1alpha1.EXTERNAL,
			TargetMetric:     "queue_depth",
		},
		Timestamp: time.Now(),
	}

	snapshot, err := CollectMetrics(context.TODO(), spec, staticMetricFetcherFactory{fetcher: fetcher})

	require.NoError(t, err)
	assert.Equal(t, "tenant-a", fetcher.namespace)
	assert.Equal(t, []float64{42}, snapshot.Values)
}

type metricFetchResult struct {
	value float64
	err   error
}

type podNameMetricFetcher struct {
	results map[string]metricFetchResult
}

func (f podNameMetricFetcher) FetchPodMetrics(_ context.Context, pod corev1.Pod, _ autoscalingv1alpha1.MetricSource) (float64, error) {
	result := f.results[pod.Name]
	return result.value, result.err
}

func TestCollectMetricsCollectionStats(t *testing.T) {
	fetchError := errors.New("fetch failed")
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "tenant-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "tenant-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-c", Namespace: "tenant-a"}},
	}

	tests := []struct {
		name              string
		sourceType        autoscalingv1alpha1.MetricSourceType
		pods              []corev1.Pod
		results           map[string]metricFetchResult
		wantValues        []float64
		wantExpected      int
		wantSuccess       int
		wantFailure       int
		wantSnapshotError bool
	}{
		{
			name:       "pod all fetches succeed and raw values are preserved",
			sourceType: autoscalingv1alpha1.POD,
			pods:       pods,
			results: map[string]metricFetchResult{
				"pod-a": {value: math.NaN()},
				"pod-b": {value: math.Inf(1)},
				"pod-c": {value: -3},
			},
			wantValues:   []float64{math.NaN(), math.Inf(1), -3},
			wantExpected: 3,
			wantSuccess:  3,
		},
		{
			name:       "resource partial fetch error",
			sourceType: autoscalingv1alpha1.RESOURCE,
			pods:       pods,
			results: map[string]metricFetchResult{
				"pod-a": {value: 10},
				"pod-b": {err: fetchError},
				"pod-c": {value: 30},
			},
			wantValues:   []float64{10, 30},
			wantExpected: 3,
			wantSuccess:  2,
			wantFailure:  1,
		},
		{
			name:       "custom all fetches fail",
			sourceType: autoscalingv1alpha1.CUSTOM,
			pods:       pods,
			results: map[string]metricFetchResult{
				"pod-a": {err: fetchError},
				"pod-b": {err: fetchError},
				"pod-c": {err: fetchError},
			},
			wantExpected:      3,
			wantFailure:       3,
			wantSnapshotError: true,
		},
		{
			name:         "pod list is empty",
			sourceType:   autoscalingv1alpha1.POD,
			wantExpected: 0,
		},
		{
			name:       "external fetch succeeds",
			sourceType: autoscalingv1alpha1.EXTERNAL,
			results: map[string]metricFetchResult{
				"external-source": {value: math.Inf(-1)},
			},
			wantValues:   []float64{math.Inf(-1)},
			wantExpected: 1,
			wantSuccess:  1,
		},
		{
			name:       "external fetch fails",
			sourceType: autoscalingv1alpha1.EXTERNAL,
			results: map[string]metricFetchResult{
				"external-source": {err: fetchError},
			},
			wantExpected:      1,
			wantFailure:       1,
			wantSnapshotError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := patypes.CollectionSpec{
				Namespace:  "tenant-a",
				TargetName: "worker",
				MetricName: "queue_depth",
				MetricSource: autoscalingv1alpha1.MetricSource{
					MetricSourceType: tt.sourceType,
					TargetMetric:     "queue_depth",
				},
				Pods:      tt.pods,
				Timestamp: time.Now(),
			}

			snapshot, err := CollectMetrics(context.Background(), spec, staticMetricFetcherFactory{
				fetcher: podNameMetricFetcher{results: tt.results},
			})

			require.NoError(t, err)
			assert.Equal(t, tt.wantExpected, snapshot.CollectionStats.ExpectedCount)
			assert.Equal(t, tt.wantSuccess, snapshot.CollectionStats.FetchSuccessCount)
			assert.Equal(t, tt.wantFailure, snapshot.CollectionStats.FetchFailureCount)
			assert.Equal(t, snapshot.CollectionStats.ExpectedCount,
				snapshot.CollectionStats.FetchSuccessCount+snapshot.CollectionStats.FetchFailureCount)
			assertRawMetricValues(t, tt.wantValues, snapshot.Values)
			assert.Equal(t, tt.wantSnapshotError, snapshot.Error != nil)
		})
	}
}

func assertRawMetricValues(t *testing.T, expected, actual []float64) {
	t.Helper()
	require.Len(t, actual, len(expected))
	for i := range expected {
		if math.IsNaN(expected[i]) {
			assert.True(t, math.IsNaN(actual[i]), "value at index %d should remain NaN", i)
			continue
		}
		assert.Equal(t, expected[i], actual[i])
	}
}
