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

package modeladapter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/modeladapter/scheduling"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

func scheduleTestPod(name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// schedulePods must score latency under the served base model. This covers the
// #3405 scenario end to end: the adapter name differs from the base model and one
// candidate pod has no scraped metrics, while the other can still be scored.
func TestSchedulePodsLeastLatencySkipsUnscoredPod(t *testing.T) {
	pods := []*corev1.Pod{scheduleTestPod("p1", "1.1.1.1"), scheduleTestPod("p2", "2.2.2.2")}
	store := cache.NewWithPodsModelMetricsForTest(pods, "base-model", map[string]map[string]metrics.MetricValue{
		"p1": {
			metrics.RequestQueueTimeSeconds:     &metrics.HistogramMetricValue{Sum: 0.1, Count: 1},
			metrics.RequestInferenceTimeSeconds: &metrics.HistogramMetricValue{Sum: 1, Count: 1},
		},
		// p2 has no metrics, as if its metric endpoint were unreachable.
	})
	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "my-adapter", Namespace: "default"},
		Spec:       modelv1alpha1.ModelAdapterSpec{BaseModel: ptr.To("base-model")},
	}
	reconciler := &ModelAdapterReconciler{scheduler: scheduling.NewLeastLatencyScheduler(store)}

	selected, err := reconciler.schedulePods(context.Background(), adapter, []corev1.Pod{*pods[0], *pods[1]}, 1)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, "p1", selected[0].Name)
}

func TestSchedulePodsScoresUnderBaseModel(t *testing.T) {
	tests := []struct {
		name      string
		baseModel *string
		podLabel  string
		wantModel string
	}{
		{
			name:      "spec base model",
			baseModel: ptr.To("base-model"),
			podLabel:  "base-model",
			wantModel: "base-model",
		},
		{
			name:      "pod label fallback",
			baseModel: nil,
			podLabel:  "base-model",
			wantModel: "base-model",
		},
		{
			name:      "adapter name fallback",
			baseModel: nil,
			podLabel:  "",
			wantModel: "my-adapter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := scheduleTestPod("p1", "1.1.1.1")
			if tt.podLabel != "" {
				pod.Labels = map[string]string{constants.ModelLabelName: tt.podLabel}
			}
			scheduler := &capturingScheduler{}
			reconciler := &ModelAdapterReconciler{scheduler: scheduler}
			adapter := &modelv1alpha1.ModelAdapter{
				ObjectMeta: metav1.ObjectMeta{Name: "my-adapter", Namespace: "default"},
				Spec:       modelv1alpha1.ModelAdapterSpec{BaseModel: tt.baseModel},
			}

			_, err := reconciler.schedulePods(context.Background(), adapter, []corev1.Pod{*pod}, 1)
			require.NoError(t, err)
			require.Equal(t, tt.wantModel, scheduler.model)
		})
	}
}

type capturingScheduler struct {
	model string
}

func (s *capturingScheduler) SelectPod(_ context.Context, model string, readyPods []corev1.Pod) (*corev1.Pod, error) {
	s.model = model
	return &readyPods[0], nil
}
