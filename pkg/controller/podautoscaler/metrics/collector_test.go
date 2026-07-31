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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	patypes "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
	corev1 "k8s.io/api/core/v1"
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
