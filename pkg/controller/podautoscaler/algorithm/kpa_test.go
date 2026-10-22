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

package algorithm

import (
	"context"
	"strings"
	"testing"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

func TestKPAAlgorithm_ComputeTargetReplicas(t *testing.T) {
	algorithm := &KPAAlgorithm{}
	metricsName := "test-metrics"

	tests := []struct {
		name            string
		currentPodCount float64
		context         *mockScalingContext
		panic           *PanicState // nil: not panicking
		expected        int32
		description     string
	}{
		{
			name:            "stable_mode_basic_scaling",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 10.0,
					},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      10.0, // per-pod mean at target: 2 pods * 10/10 = 2
				PanicValue:       10.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    2,
			description: "Should scale to 2 pods based on stable value in stable mode",
		},
		{
			name:            "panic_mode_scaling_up",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 10.0,
					},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      30.0, // 30/10 = 3 pods needed
				PanicValue:       40.0, // 40/10 = 4 pods needed
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			panic:       &PanicState{PanicTime: kpaTestNow, MaxPanicPods: 2},
			expected:    4,
			description: "Should scale to 4 pods based on panic value in panic mode",
		},
		{
			name:            "panic_mode_no_scale_down",
			currentPodCount: 5.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 10.0,
					},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      4.0, // per-pod mean: 5 pods * 4/10 = 2 pods needed
				PanicValue:       4.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			panic:       &PanicState{PanicTime: kpaTestNow, MaxPanicPods: 5}, // Should not scale down below this
			expected:    5,
			description: "Should not scale down below maxPanicPods in panic mode",
		},
		{
			name:            "scale_to_zero",
			currentPodCount: 1.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 10.0,
					},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      0.0, // No load, should scale to 0
				PanicValue:       0.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    0,
			description: "Should scale to zero when no load",
		},
		{
			name:            "activation_scale_from_zero",
			currentPodCount: 0.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 10.0,
					},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      15.0, // 15/10 = 2 pods needed, but activation scale should apply
				PanicValue:       15.0,
				ActivationScale:  3, // Should scale to 3 when activating
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    3,
			description: "Should respect activation scale when scaling from zero",
		},
		{
			name:            "max_scale_up_rate_limit",
			currentPodCount: 2.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 10.0,
					},
				},
				MaxScaleUpRate:   1.5, // Can only scale up by 50%
				MaxScaleDownRate: 2.0,
				StableValue:      100.0, // Would need 10 pods, but limited by scale up rate
				PanicValue:       100.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      20,
			},
			expected:    3, // 2 * 1.5 = 3 (ceil)
			description: "Should be limited by max scale up rate",
		},
		{
			name:            "max_scale_down_rate_limit",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {
						TargetValue: 10.0,
					},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0, // Can scale down by 50% (4/2 = 2 minimum)
				StableValue:      5.0, // Would need 1 pod, but limited by scale down rate
				PanicValue:       5.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    2, // 4/2 = 2 (floor)
			description: "Should be limited by max scale down rate",
		},
		{
			name:            "per_pod_mean_over_target_scales_up",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 50.0, MetricType: autoscalingv1alpha1.POD},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      90.0, // 4 pods each at 90 vs target 50: 4 * 90/50 = 8
				PanicValue:       90.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    8,
			description: "A per-pod mean above target must scale up in proportion to the pod count (#2879)",
		},
		{
			name:            "ratio_metric_can_exceed_two_replicas",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 0.5, MetricType: autoscalingv1alpha1.POD},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      0.9, // gpu_cache_usage_perc style ratio: 4 * 0.9/0.5 = 7.2
				PanicValue:       0.9,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      8,
			},
			expected:    8,
			description: "A ratio metric must not cap the recommendation at ceil(1/target)",
		},
		{
			name:            "external_source_value_is_already_a_total",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 50.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				MaxScaleUpRate:   2.0,
				MaxScaleDownRate: 2.0,
				StableValue:      90.0, // one total value: ceil(90/50) = 2
				PanicValue:       90.0,
				ActivationScale:  1,
				PanicThreshold:   2.0,
				MinReplicas:      0,
				MaxReplicas:      10,
			},
			expected:    2,
			description: "External and domain sources keep the total/target formula",
		},
		{
			name:            "external_total_near_one_pod_target_scales_down",
			currentPodCount: 10.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              40.0,
				PanicValue:               40.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    5,
			description: "A total of one pod's worth is far below the band per pod (4 vs 40): step down to the max-scale-down floor of 10/2, not hold 10",
		},
		{
			name:            "domain_total_slightly_below_one_pod_target_scales_down",
			currentPodCount: 8.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.DOMAIN},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              38.0,
				PanicValue:               38.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    4,
			description: "38 over 8 pods is 4.75 per pod, below the band: step down",
		},
		{
			name:            "external_total_within_tolerance_per_pod_holds",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              164.0,
				PanicValue:               164.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    4,
			description: "164 over 4 pods is 41 per pod, inside the band: hold, not scale up to ceil(164/40)=5",
		},
		{
			name:            "external_single_pod_within_tolerance_holds",
			currentPodCount: 1.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              41.0,
				PanicValue:               41.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    1,
			description: "With one pod the total is the per-pod value",
		},
		{
			name:            "external_total_near_one_pod_target_activates_from_zero",
			currentPodCount: 0.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              40.0,
				PanicValue:               40.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    1,
			description: "With no pods, a total of one pod's worth activates one pod instead of holding at zero",
		},
		{
			name:            "external_total_at_upper_band_edge_holds",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              176.0,
				PanicValue:               176.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    4,
			description: "176 over 4 pods is exactly 44 per pod, the upper edge of the band: hold",
		},
		{
			name:            "external_total_above_upper_band_edge_scales_up",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              177.0,
				PanicValue:               177.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    5,
			description: "177 over 4 pods is just above 44 per pod: scale up to ceil(177/40)=5",
		},
		{
			name:            "external_total_at_lower_band_edge_holds",
			currentPodCount: 10.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              360.0,
				PanicValue:               360.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    10,
			description: "360 over 10 pods is exactly 36 per pod, the lower edge of the band: hold 10, not step to ceil(360/40)=9",
		},
		{
			name:            "external_panic_window_judged_per_pod",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.EXTERNAL},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              164.0,
				PanicValue:               164.0,
				ActivationScale:          1,
				PanicThreshold:           1.2,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    4,
			description: "With a 1.2 panic threshold, a total compared directly would ask for 5 and enter panic; per pod it is 41 and holds",
		},
		{
			name:            "pod_source_mean_within_tolerance_holds",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.POD},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              41.0,
				PanicValue:               41.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    4,
			description: "A per-pod mean is not divided by the pod count: 41 is inside the band and holds 4",
		},
		{
			name:            "pod_source_mean_above_tolerance_scales_up",
			currentPodCount: 4.0,
			context: &mockScalingContext{
				MetricTargets: map[string]scalingctx.MetricTarget{
					metricsName: {TargetValue: 40.0, MetricType: autoscalingv1alpha1.POD},
				},
				UpFluctuationTolerance:   0.1,
				DownFluctuationTolerance: 0.1,
				MaxScaleUpRate:           2.0,
				MaxScaleDownRate:         2.0,
				StableValue:              80.0,
				PanicValue:               80.0,
				ActivationScale:          1,
				PanicThreshold:           2.0,
				MinReplicas:              0,
				MaxReplicas:              100,
			},
			expected:    8,
			description: "A per-pod mean of 80 over 4 pods asks for ceil(4*80/40)=8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts, ok := algorithm.desiredPodCounts(tt.currentPodCount, tt.currentPodCount, tt.context, metricsName)
			if !ok {
				t.Fatal("desiredPodCounts found no target value")
			}
			state := tt.panic
			if state == nil {
				state = &PanicState{}
			}
			result, _ := state.apply(kpaTestNow, kpaTestStableWindow, counts)
			if result != tt.expected {
				t.Errorf("desired pod count = %d, expected %d. %s", result, tt.expected, tt.description)
			}
		})
	}
}

var (
	kpaTestNow          = time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)
	kpaTestStableWindow = 180 * time.Second
)

// kpaTarget is one KPA target observed over time, with its panic state kept
// across recommendations the way DefaultAutoScaler keeps it.
type kpaTarget struct {
	t     *testing.T
	ctx   *mockScalingContext
	state PanicState
}

func newKPATarget(t *testing.T) *kpaTarget {
	return &kpaTarget{t: t, ctx: &mockScalingContext{
		MetricTargets:    map[string]scalingctx.MetricTarget{"rps": {TargetValue: 10, MetricType: autoscalingv1alpha1.POD}},
		MaxScaleUpRate:   1000,
		MaxScaleDownRate: 1000,
		ActivationScale:  1,
		PanicThreshold:   2.0,
		MinReplicas:      0,
		MaxReplicas:      100,
	}}
}

// recommend returns the recommendation at kpaTestNow+at for per-pod means
// stable and panic on pods replicas.
func (k *kpaTarget) recommend(at time.Duration, pods int32, stable, panic float64) (int32, string) {
	k.t.Helper()
	rec, err := (&KPAAlgorithm{}).ComputeRecommendation(context.Background(), ScalingRequest{
		CurrentReplicas: pods,
		AggregatedMetrics: &types.AggregatedMetrics{
			MetricKey:   types.MetricKey{MetricName: "rps"},
			StableValue: stable,
			PanicValue:  panic,
		},
		ScalingContext: k.ctx,
		Timestamp:      kpaTestNow.Add(at),
		StableWindow:   kpaTestStableWindow,
		Panic:          &k.state,
	})
	if err != nil {
		k.t.Fatalf("ComputeRecommendation: %v", err)
	}
	return rec.DesiredReplicas, rec.Metadata["mode"].(string)
}

func (k *kpaTarget) require(at time.Duration, pods int32, stable, panic float64, wantReplicas int32, wantMode string) {
	k.t.Helper()
	got, mode := k.recommend(at, pods, stable, panic)
	if got != wantReplicas || mode != wantMode {
		k.t.Fatalf("at +%s on %d pods (stable %v, panic %v): got %d in %s mode, want %d in %s mode",
			at, pods, stable, panic, got, mode, wantReplicas, wantMode)
	}
}

// A surge enters panic, panic holds the pod count while the surge recedes,
// and it ends a whole stable window after the panic window last crossed the
// threshold. This is the hold #3082 found missing: the panic state now
// outlives a single recommendation.
func TestKPAAlgorithm_PanicHoldsUntilAStableWindowPasses(t *testing.T) {
	k := newKPATarget(t)

	// Steady at target: 4 pods each at 10.
	k.require(0, 4, 10, 10, 4, "stable")
	// Surge: the panic window wants 4*25/10 = 10 pods, 2.5x the 4 there are.
	k.require(10*time.Second, 4, 12, 25, 10, "panic")
	// The surge recedes. Without the hold, 10 pods at 3 would go back to 3.
	k.require(20*time.Second, 10, 4, 3, 10, "panic")
	k.require(kpaTestStableWindow, 10, 4, 3, 10, "panic")
	// A stable window after the last crossing, the panic ends and the stable
	// window decides again: 10*4/10 = 4.
	end := 10*time.Second + kpaTestStableWindow + time.Second
	k.require(end, 10, 4, 3, 4, "stable")
	// The next panic starts from its own count: 4*22/10 = 9, not the 10 the
	// previous panic held.
	k.require(end+10*time.Second, 4, 10, 22, 9, "panic")
}

// A metric with no target in the scaling context is an error, so
// DefaultAutoScaler skips it instead of counting it as a vote to stay at the
// current replicas. A validated spec always has a target for every metric.
func TestKPAAlgorithm_MetricWithoutTargetIsAnError(t *testing.T) {
	k := newKPATarget(t)
	_, err := (&KPAAlgorithm{}).ComputeRecommendation(context.Background(), ScalingRequest{
		CurrentReplicas: 3,
		AggregatedMetrics: &types.AggregatedMetrics{
			MetricKey:   types.MetricKey{MetricName: "other"},
			StableValue: 10,
			PanicValue:  10,
		},
		ScalingContext: k.ctx,
		Timestamp:      kpaTestNow,
		StableWindow:   kpaTestStableWindow,
		Panic:          &k.state,
	})
	if err == nil || !strings.Contains(err.Error(), `no target value for metric "other"`) {
		t.Fatalf("ComputeRecommendation: got error %v, want no target value for metric \"other\"", err)
	}
}

// Crossing the threshold again during a panic extends it, and a bigger surge
// raises the held count.
func TestKPAAlgorithm_PanicIsExtendedAndOnlyGrows(t *testing.T) {
	k := newKPATarget(t)

	k.require(0, 4, 10, 25, 10, "panic")
	// 10 pods at 25: wants 25, 2.5x the 10 there are, so the panic extends.
	k.require(100*time.Second, 10, 10, 25, 25, "panic")
	// Below the threshold again. The hold is 25 and runs for a stable window
	// from +100s, not from +0s.
	k.require(200*time.Second, 25, 5, 5, 25, "panic")
	k.require(100*time.Second+kpaTestStableWindow, 25, 5, 5, 25, "panic")
	k.require(100*time.Second+kpaTestStableWindow+time.Second, 25, 5, 5, 13, "stable")
}

// The panic window must ask for PanicThreshold times the current pods, not
// just exceed the stable value by that ratio.
func TestKPAAlgorithm_PanicThresholdIsRelativeToCurrentPods(t *testing.T) {
	tests := []struct {
		name          string
		pods          int32
		stable, panic float64
		wantReplicas  int32
		wantMode      string
	}{
		// Idle: nothing to scale for is not a panic (it used to be).
		{"idle", 2, 0, 0, 0, "stable"},
		// Panic 3x stable, but 4*6/10 = 3 pods is less than 2x the 4 there are.
		{"ratio to stable is not enough", 4, 2, 6, 1, "stable"},
		// Exactly 2x the current pods panics, as in Knative (>=).
		{"twice the current pods", 4, 10, 20, 8, "panic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newKPATarget(t).require(0, tt.pods, tt.stable, tt.panic, tt.wantReplicas, tt.wantMode)
		})
	}
}
