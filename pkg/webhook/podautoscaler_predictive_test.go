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

package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func predictivePA(strategy autoscalingv1alpha1.ScalingStrategyType, spec *autoscalingv1alpha1.PredictiveSpec) *autoscalingv1alpha1.PodAutoscaler {
	return &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
		ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
		MaxReplicas:     10,
		ScalingStrategy: strategy,
		Predictive:      spec,
		MetricsSources: []autoscalingv1alpha1.MetricSource{
			{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     "cpu",
				TargetValue:      "50",
			},
		},
	}}
}

func TestValidatePodAutoscalerPredictive(t *testing.T) {
	validator := &PodAutoscalerCustomValidator{}

	t.Run("preview mode is accepted", func(t *testing.T) {
		pa := predictivePA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.PredictiveSpec{
			Mode: autoscalingv1alpha1.PredictiveModePreview,
		})
		require.NoError(t, validator.validatePodAutoscaler(pa))
	})

	t.Run("auto mode with a horizon is accepted", func(t *testing.T) {
		pa := predictivePA(autoscalingv1alpha1.APA, &autoscalingv1alpha1.PredictiveSpec{
			Mode:           autoscalingv1alpha1.PredictiveModeAuto,
			HorizonSeconds: ptr.To[int64](120),
		})
		require.NoError(t, validator.validatePodAutoscaler(pa))
	})

	t.Run("hpa strategy is rejected", func(t *testing.T) {
		pa := predictivePA(autoscalingv1alpha1.HPA, &autoscalingv1alpha1.PredictiveSpec{
			Mode: autoscalingv1alpha1.PredictiveModePreview,
		})
		err := validator.validatePodAutoscaler(pa)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.predictive")
		assert.Contains(t, err.Error(), "scalingStrategy=HPA")
	})

	t.Run("unknown mode is rejected", func(t *testing.T) {
		pa := predictivePA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.PredictiveSpec{
			Mode: autoscalingv1alpha1.PredictiveMode("Reactive"),
		})
		err := validator.validatePodAutoscaler(pa)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.predictive.mode")
	})

	t.Run("horizon bounds are enforced", func(t *testing.T) {
		for name, horizon := range map[string]int64{"zero": 0, "too large": 3601} {
			t.Run(name, func(t *testing.T) {
				pa := predictivePA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.PredictiveSpec{
					HorizonSeconds: ptr.To(horizon),
				})
				err := validator.validatePodAutoscaler(pa)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "spec.predictive.horizonSeconds")
			})
		}
	})

	t.Run("absent block is accepted", func(t *testing.T) {
		pa := predictivePA(autoscalingv1alpha1.HPA, nil)
		require.NoError(t, validator.validatePodAutoscaler(pa))
	})
}
