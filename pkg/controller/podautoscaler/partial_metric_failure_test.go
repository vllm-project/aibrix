/*
Copyright 2024 The Aibrix Team.

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
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// drainEvents returns the events recorded so far whose reason is one of reasons.
func drainEvents(recorder *record.FakeRecorder, reasons ...string) []string {
	var matched []string
	for len(recorder.Events) > 0 {
		event := <-recorder.Events
		for _, reason := range reasons {
			if strings.Contains(event, " "+reason+" ") {
				matched = append(matched, event)
			}
		}
	}
	return matched
}

// withQueueLength adds an external queue_length source to pa, the source the
// tests below make fail.
func withQueueLength(pa *autoscalingv1alpha1.PodAutoscaler) {
	pa.Spec.MetricsSources = append(pa.Spec.MetricsSources, autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.EXTERNAL,
		TargetMetric:     "queue_length",
		TargetValue:      "50",
	})
}

// TestReconcileReportsFailedMetricSourcesOnChange checks that a failed metric
// source raises one warning naming it, not one per reconcile: the event
// recorder rate-limits per object, so a warning on every pass would crowd out
// the PodAutoscaler's other events. Recovery is reported once too.
func TestReconcileReportsFailedMetricSourcesOnChange(t *testing.T) {
	pa := stabilityPodAutoscaler(autoscalingv1alpha1.KPA)
	h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
	recorder := record.NewFakeRecorder(100)
	h.reconciler.EventRecorder = recorder
	autoScaler := &fakeAutoScaler{}
	h.reconciler.autoScaler = autoScaler

	reconcileWith := func(failed []string) {
		t.Helper()
		autoScaler.result = &ReplicaComputeResult{DesiredReplicas: 1, Algorithm: "kpa", Valid: true, FailedMetrics: failed}
		if _, err := h.reconcile(); err != nil {
			t.Fatalf("Reconcile returned error: %v", err)
		}
	}

	for i := 0; i < 3; i++ {
		reconcileWith([]string{"queue_length"})
	}
	events := drainEvents(recorder, ReasonFailedGetMetrics, ReasonRecoveredMetrics)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning "+ReasonFailedGetMetrics) ||
		!strings.Contains(events[0], "queue_length") {
		t.Fatalf("after 3 reconciles with queue_length failing, want one Warning %s naming it, got %v",
			ReasonFailedGetMetrics, events)
	}

	for i := 0; i < 2; i++ {
		reconcileWith(nil)
	}
	events = drainEvents(recorder, ReasonFailedGetMetrics, ReasonRecoveredMetrics)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Normal "+ReasonRecoveredMetrics) ||
		!strings.Contains(events[0], "queue_length") {
		t.Fatalf("after recovery, want one Normal %s naming queue_length, got %v", ReasonRecoveredMetrics, events)
	}
}

// TestComputeScaleDecisionHoldsScaleDownWhileAMetricFails covers the decision
// a recommendation turns into while queue_length fails, with and without
// hold-scale-down-on-metric-failure. The hold keeps the replicas, a scale-up
// still goes ahead, and minReplicas and maxReplicas still apply.
func TestComputeScaleDecisionHoldsScaleDownWhileAMetricFails(t *testing.T) {
	for _, tt := range []struct {
		name        string
		hold        bool
		failed      []string
		recommended int32
		current     int32
		minReplicas int32
		wantDesired int32
		wantHeld    bool
		wantReason  string
	}{
		{name: "hold keeps the replicas", hold: true, failed: []string{"queue_length"},
			recommended: 2, current: 4, minReplicas: 1, wantDesired: 4, wantHeld: true,
			wantReason: "Scale-down held (queue_length could not be read)"},
		{name: "best-effort default scales down on the remaining sources", failed: []string{"queue_length"},
			recommended: 2, current: 4, minReplicas: 1, wantDesired: 2,
			wantReason: "Available metrics below target (queue_length could not be read)"},
		{name: "hold does not apply while every source is read", hold: true,
			recommended: 2, current: 4, minReplicas: 1, wantDesired: 2,
			wantReason: "All metrics below target"},
		{name: "hold still scales up", hold: true, failed: []string{"queue_length"},
			recommended: 8, current: 4, minReplicas: 1, wantDesired: 8,
			wantReason: "kpa above target"},
		{name: "hold still applies maxReplicas", hold: true, failed: []string{"queue_length"},
			recommended: 2, current: 12, minReplicas: 1, wantDesired: 10,
			wantReason: "current replicas above maximum"},
		{name: "hold still applies minReplicas", hold: true, failed: []string{"queue_length"},
			recommended: 2, current: 4, minReplicas: 6, wantDesired: 6,
			wantReason: "current replicas below minimum"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pa := stabilityPodAutoscaler(autoscalingv1alpha1.KPA)
			pa.Spec.MinReplicas = ptr.To(tt.minReplicas)
			pa.Annotations = map[string]string{types.ScaleDownCooldownWindowLabel: "0s"}
			if tt.hold {
				pa.Annotations[types.HoldScaleDownOnMetricFailureLabel] = "true"
			}
			h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
			h.reconciler.autoScaler = &fakeAutoScaler{result: &ReplicaComputeResult{
				DesiredReplicas: tt.recommended,
				Algorithm:       "kpa",
				Valid:           true,
				FailedMetrics:   tt.failed,
			}}
			scaleObj := buildScaleObject("apps/v1", "Deployment", ns, pa.Spec.ScaleTargetRef.Name)

			decision, err := h.reconciler.computeScaleDecision(context.Background(), *pa, scaleObj, tt.current)

			if err != nil {
				t.Fatalf("computeScaleDecision returned error: %v", err)
			}
			if decision.DesiredReplicas != tt.wantDesired || decision.ShouldScale != (tt.wantDesired != tt.current) {
				t.Fatalf("decision = %d replicas, shouldScale %t; want %d", decision.DesiredReplicas, decision.ShouldScale, tt.wantDesired)
			}
			if decision.ScaleDownHeld != tt.wantHeld {
				t.Fatalf("ScaleDownHeld = %t, want %t", decision.ScaleDownHeld, tt.wantHeld)
			}
			if decision.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", decision.Reason, tt.wantReason)
			}
		})
	}
}

// TestHeldRecommendationsStayOutOfTheCooldownWindow checks that, as in
// Kubernetes HPA, a held scale-down recommendation is not recorded for the
// cooldown windows, so with a scale-up window the data the hold does not trust
// cannot delay a scale-up. Without the hold the recommendation is recorded.
func TestHeldRecommendationsStayOutOfTheCooldownWindow(t *testing.T) {
	for _, tt := range []struct {
		name        string
		hold        bool
		wantDesired int32
	}{
		{name: "held recommendations are not recorded", hold: true, wantDesired: 6},
		{name: "best-effort recommendations are recorded", hold: false, wantDesired: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pa := stabilityPodAutoscaler(autoscalingv1alpha1.KPA)
			pa.Annotations = map[string]string{
				types.ScaleUpCooldownWindowLabel:   "60s",
				types.ScaleDownCooldownWindowLabel: "0s",
			}
			if tt.hold {
				pa.Annotations[types.HoldScaleDownOnMetricFailureLabel] = "true"
			}
			h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
			clock := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
			h.reconciler.now = func() time.Time { return clock }
			autoScaler := &fakeAutoScaler{}
			h.reconciler.autoScaler = autoScaler
			scaleObj := buildScaleObject("apps/v1", "Deployment", ns, pa.Spec.ScaleTargetRef.Name)
			decide := func(recommended int32) *ScaleDecision {
				t.Helper()
				autoScaler.result = &ReplicaComputeResult{
					DesiredReplicas: recommended, Algorithm: "kpa", Valid: true, FailedMetrics: []string{"queue_length"},
				}
				decision, err := h.reconciler.computeScaleDecision(context.Background(), *pa, scaleObj, 4)
				if err != nil {
					t.Fatalf("computeScaleDecision returned error: %v", err)
				}
				return decision
			}

			// queue_length fails throughout. The remaining source first asks for 2,
			// then, 10s later and inside the 60s scale-up window, for 6.
			decide(2)
			clock = clock.Add(10 * time.Second)
			if got := decide(6).DesiredReplicas; got != tt.wantDesired {
				t.Fatalf("DesiredReplicas = %d, want %d", got, tt.wantDesired)
			}
		})
	}
}

// TestReconcileHoldsScaleDownEndToEnd runs the real autoscaler through
// Reconcile with queue_length failing throughout and the hold enabled. A pod
// metric that asks for 2 replicas is held at 4 without a reconcile error, and
// the same pod metric later asking for 8 scales up.
func TestReconcileHoldsScaleDownEndToEnd(t *testing.T) {
	for _, strategy := range []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.KPA, autoscalingv1alpha1.APA} {
		t.Run(string(strategy), func(t *testing.T) {
			pa := stabilityPodAutoscaler(strategy)
			pa.Annotations = map[string]string{
				types.HoldScaleDownOnMetricFailureLabel: "true",
				types.ScaleDownCooldownWindowLabel:      "0s",
			}
			pa.Spec.MetricsSources = []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.POD,
				ProtocolType:     autoscalingv1alpha1.HTTP,
				Port:             "8000",
				Path:             "/metrics",
				TargetMetric:     "gpu_cache_usage_perc",
				TargetValue:      "50",
			}}
			withQueueLength(pa)
			objs := []client.Object{stabilityDeployment(pa.Spec.ScaleTargetRef.Name)}
			for i := 0; i < 4; i++ {
				objs = append(objs, readyPod(ns, fmt.Sprintf("ready-%d", i), map[string]string{"app": "foo"}))
			}
			h := newStatusStabilityHarness(t, pa, objs...)
			h.scaleClient.counts = &ReplicaCounts{Configured: 4, Observed: 4}
			recorder := record.NewFakeRecorder(100)
			h.reconciler.EventRecorder = recorder
			// Fixed timestamps, far enough apart that the metric windows do not
			// average the two pod values.
			clock := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
			h.reconciler.now = func() time.Time { return clock }
			fetcher := &mockMetricFetcherFactory{mockMetricFetcher: mockMetricFetcher{
				metricsValues:  map[string]float64{"gpu_cache_usage_perc": 25},
				failingMetrics: map[string]bool{"queue_length": true},
			}}
			h.reconciler.autoScaler = NewDefaultAutoScaler(fetcher, fake.NewClientBuilder().Build())

			if _, err := h.reconcile(); err != nil {
				t.Fatalf("held Reconcile returned error: %v", err)
			}
			if len(h.scaleClient.scaledTo) != 0 {
				t.Fatalf("held scale-down rescaled to %v", h.scaleClient.scaledTo)
			}
			requireCondition(t, h.latest(), ConditionScalingActive, metav1.ConditionFalse, ReasonScaleDownHeld)
			if held := drainEvents(recorder, ReasonScaleDownHeld); len(held) != 1 || !strings.Contains(held[0], "queue_length") {
				t.Fatalf("want one %s event naming queue_length, got %v", ReasonScaleDownHeld, held)
			}

			clock = clock.Add(10 * time.Minute)
			fetcher.metricsValues["gpu_cache_usage_perc"] = 100
			if _, err := h.reconcile(); err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}
			if want := []int32{8}; fmt.Sprint(h.scaleClient.scaledTo) != fmt.Sprint(want) {
				t.Fatalf("scaled to %v, want %v", h.scaleClient.scaledTo, want)
			}
		})
	}
}

// TestReconcileAllMetricsFailingWithHold checks that with every source failing
// the hold changes nothing: it is the existing failure path, reported once.
func TestReconcileAllMetricsFailingWithHold(t *testing.T) {
	pa := stabilityPodAutoscaler(autoscalingv1alpha1.KPA)
	pa.Annotations = map[string]string{types.HoldScaleDownOnMetricFailureLabel: "true"}
	h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
	recorder := record.NewFakeRecorder(100)
	h.reconciler.EventRecorder = recorder
	h.reconciler.autoScaler = &fakeAutoScaler{
		result: &ReplicaComputeResult{Valid: false},
		err:    fmt.Errorf("all 2 metric sources failed"),
	}

	if _, err := h.reconcile(); err == nil {
		t.Fatal("Reconcile returned no error with every metric source failing")
	}

	requireCondition(t, h.latest(), ConditionScalingActive, metav1.ConditionFalse, ReasonFailedComputeScale)
	events := drainEvents(recorder, ReasonFailedComputeScale, ReasonFailedGetMetrics, ReasonScaleDownHeld)
	if len(events) != 1 || !strings.Contains(events[0], ReasonFailedComputeScale) {
		t.Fatalf("want one %s event, got %v", ReasonFailedComputeScale, events)
	}
}
