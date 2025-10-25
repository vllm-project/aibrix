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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"

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
