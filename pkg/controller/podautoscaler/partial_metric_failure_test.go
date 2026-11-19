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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// TestReconcileReportsFailedMetricSources checks that a recommendation made
// while a metric source fails raises a warning event naming that source,
// instead of leaving the failure to the controller log.
func TestReconcileReportsFailedMetricSources(t *testing.T) {
	for _, tt := range []struct {
		name      string
		failed    []string
		wantEvent bool
	}{
		{name: "a failed source raises a warning", failed: []string{"queue_length"}, wantEvent: true},
		{name: "no failure raises nothing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pa := stabilityPodAutoscaler(autoscalingv1alpha1.KPA)
			h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
			recorder := record.NewFakeRecorder(100)
			h.reconciler.EventRecorder = recorder
			h.reconciler.autoScaler = &fakeAutoScaler{result: &ReplicaComputeResult{
				DesiredReplicas: 1,
				Algorithm:       "kpa",
				Valid:           true,
				FailedMetrics:   tt.failed,
			}}

			if _, err := h.reconcile(); err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}

			var warnings []string
			for len(recorder.Events) > 0 {
				if event := <-recorder.Events; strings.Contains(event, ReasonFailedGetMetrics) {
					warnings = append(warnings, event)
				}
			}
			if !tt.wantEvent {
				if len(warnings) != 0 {
					t.Fatalf("unexpected %s events: %v", ReasonFailedGetMetrics, warnings)
				}
				return
			}
			if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "Warning") ||
				!strings.Contains(warnings[0], "queue_length") {
				t.Fatalf("want one Warning %s event naming queue_length, got %v", ReasonFailedGetMetrics, warnings)
			}
		})
	}
}

// TestComputeScaleDecisionReasonNamesFailedMetricSources checks that a
// scale-down made while a metric source fails does not claim that all metrics
// are below target.
func TestComputeScaleDecisionReasonNamesFailedMetricSources(t *testing.T) {
	for _, tt := range []struct {
		name       string
		failed     []string
		wantReason string
	}{
		{name: "with a failed source", failed: []string{"queue_length"},
			wantReason: "Available metrics below target (queue_length could not be read)"},
		{name: "without a failure", wantReason: "All metrics below target"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pa := stabilityPodAutoscaler(autoscalingv1alpha1.KPA)
			pa.Annotations = map[string]string{types.ScaleDownCooldownWindowLabel: "0s"}
			h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
			h.reconciler.autoScaler = &fakeAutoScaler{result: &ReplicaComputeResult{
				DesiredReplicas: 2,
				Algorithm:       "kpa",
				Valid:           true,
				FailedMetrics:   tt.failed,
			}}
			scaleObj := buildScaleObject("apps/v1", "Deployment", ns, pa.Spec.ScaleTargetRef.Name)

			decision, err := h.reconciler.computeScaleDecision(context.Background(), *pa, scaleObj, 4)

			if err != nil {
				t.Fatalf("computeScaleDecision returned error: %v", err)
			}
			if !decision.ShouldScale || decision.DesiredReplicas != 2 {
				t.Fatalf("decision = %d replicas, shouldScale %t; want a scale-down to 2",
					decision.DesiredReplicas, decision.ShouldScale)
			}
			if decision.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", decision.Reason, tt.wantReason)
			}
		})
	}
}

// TestReconcileReportsHeldScaleDownWithoutError checks that a scale-down held by
// hold-scale-down-on-metric-failure is reported in the status and a warning
// naming the failed source, without failing the reconcile: the hold is an
// expected state, and the resync evaluates it again.
func TestReconcileReportsHeldScaleDownWithoutError(t *testing.T) {
	pa := stabilityPodAutoscaler(autoscalingv1alpha1.KPA)
	h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
	recorder := record.NewFakeRecorder(100)
	h.reconciler.EventRecorder = recorder
	h.reconciler.autoScaler = &fakeAutoScaler{
		result: &ReplicaComputeResult{Valid: false, FailedMetrics: []string{"queue_length"}},
		err:    fmt.Errorf("%w: metric sources queue_length could not be read", errScaleDownHeld),
	}

	if _, err := h.reconcile(); err != nil {
		t.Fatalf("Reconcile returned error %v for a held scale-down", err)
	}

	requireCondition(t, h.latest(), ConditionScalingActive, metav1.ConditionFalse, ReasonFailedComputeScale)
	var warned bool
	for len(recorder.Events) > 0 {
		event := <-recorder.Events
		warned = warned || (strings.HasPrefix(event, "Warning "+ReasonFailedComputeScale) &&
			strings.Contains(event, "queue_length"))
	}
	if !warned {
		t.Fatalf("want a Warning %s event naming queue_length", ReasonFailedComputeScale)
	}
}
