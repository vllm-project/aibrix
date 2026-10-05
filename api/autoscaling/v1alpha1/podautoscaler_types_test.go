/*
Copyright 2024.

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

package v1alpha1

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestPodAutoscalerInitialization tests the initialization of a PodAutoscaler object
// and checks if the default values are as expected.
func TestPodAutoscalerInitialization(t *testing.T) {
	pa := &PodAutoscaler{
		Spec: PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Kind: "Deployment",
				Name: "example-deployment",
			},
			MinReplicas: nil, // expecting nil as default since it's a pointer and no value is assigned
			MaxReplicas: 5,
			MetricsSources: []MetricSource{
				{
					Endpoint: "service1.example.com",
					Path:     "/api/metrics/cpu",
				},
			},
			ScalingStrategy: "HPA",
		},
	}

	// Check if the ScaleTargetRef is set up correctly
	if got, want := pa.Spec.ScaleTargetRef.Name, "example-deployment"; got != want {
		t.Errorf("Spec.ScaleTargetRef.Name = %v, want %v", got, want)
	}

	// Check if MinReplicas is nil
	if pa.Spec.MinReplicas != nil {
		t.Errorf("Spec.MinReplicas expected to be nil, got %v", pa.Spec.MinReplicas)
	}

	// Check if MaxReplicas is set to 5
	if got, want := pa.Spec.MaxReplicas, int32(5); got != want {
		t.Errorf("Spec.MaxReplicas = %v, want %v", got, want)
	}

	// Check if the first MetricsSource is set up correctly
	expectedMetricSource := MetricSource{
		Endpoint: "service1.example.com",
		Path:     "/api/metrics/cpu",
	}
	if got, want := pa.Spec.MetricsSources[0], expectedMetricSource; !reflect.DeepEqual(got, want) {
		t.Errorf("Spec.MetricsSources[0] = %v, want %v", got, want)
	}

	// Check if the ScalingStrategy is "HPA"
	if got, want := pa.Spec.ScalingStrategy, HPA; got != want {
		t.Errorf("Spec.ScalingStrategy = %v, want %v", got, want)
	}

}

func TestPodAutoscalerScheduledBoundsJSONRoundTrip(t *testing.T) {
	minReplicas := int32(3)
	maxReplicas := int32(12)

	pa := &PodAutoscaler{
		Spec: PodAutoscalerSpec{
			Schedules: []PodAutoscalerSchedule{
				{
					Name:        "business-hours",
					Timezone:    "America/Los_Angeles",
					DaysOfWeek:  []string{"Mon", "Tue", "Wed", "Thu", "Fri"},
					StartTime:   "09:00",
					EndTime:     "17:00",
					MinReplicas: &minReplicas,
					MaxReplicas: &maxReplicas,
				},
			},
		},
	}

	data, err := json.Marshal(pa)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	for _, field := range []string{"schedules", "name", "timezone", "daysOfWeek", "startTime", "endTime", "minReplicas", "maxReplicas"} {
		if !bytes.Contains(data, []byte(`"`+field+`"`)) {
			t.Errorf("json.Marshal() output missing %q: %s", field, data)
		}
	}

	var roundTripped PodAutoscaler
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if got := len(roundTripped.Spec.Schedules); got != 1 {
		t.Fatalf("len(Spec.Schedules) = %d, want 1", got)
	}

	got := roundTripped.Spec.Schedules[0]
	if got.Name != "business-hours" || got.Timezone != "America/Los_Angeles" ||
		got.StartTime != "09:00" || got.EndTime != "17:00" {
		t.Errorf("round-tripped scheduled bounds = %#v, want all scalar fields preserved", got)
	}
	if !reflect.DeepEqual(got.DaysOfWeek, []string{"Mon", "Tue", "Wed", "Thu", "Fri"}) {
		t.Errorf("round-tripped daysOfWeek = %v, want weekdays", got.DaysOfWeek)
	}
	if got.MinReplicas == nil || *got.MinReplicas != minReplicas || got.MaxReplicas == nil || *got.MaxReplicas != maxReplicas {
		t.Errorf("round-tripped replica bounds = min %v, max %v; want %d and %d", got.MinReplicas, got.MaxReplicas, minReplicas, maxReplicas)
	}
}

// Additional test cases can be added here to further validate other aspects of the PodAutoscaler.
