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

package podautoscaler

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

type effectiveReplicaBounds struct {
	MinReplicas  int32
	MaxReplicas  int32
	ScheduleName string
}

func resolveEffectiveReplicaBounds(pa *autoscalingv1alpha1.PodAutoscaler, now time.Time) (effectiveReplicaBounds, error) {
	if pa == nil {
		return effectiveReplicaBounds{}, fmt.Errorf("PodAutoscaler is nil")
	}
	if errs := validateScheduledBounds(pa); len(errs) > 0 {
		return effectiveReplicaBounds{}, fmt.Errorf("invalid scheduled bounds: %w", errs.ToAggregate())
	}

	bounds := effectiveReplicaBounds{MaxReplicas: pa.Spec.MaxReplicas}
	if pa.Spec.MinReplicas != nil {
		bounds.MinReplicas = *pa.Spec.MinReplicas
	} else {
		bounds.MinReplicas = 1
	}

	for _, scheduled := range pa.Spec.ScheduledBounds {
		if !isScheduleActive(scheduled, now) {
			continue
		}

		if scheduled.MinReplicas != nil {
			bounds.MinReplicas = *scheduled.MinReplicas
		}
		if scheduled.MaxReplicas != nil {
			bounds.MaxReplicas = *scheduled.MaxReplicas
		}
		bounds.ScheduleName = scheduled.Name
		return bounds, nil
	}

	return bounds, nil
}

func validateScheduledBounds(pa *autoscalingv1alpha1.PodAutoscaler) field.ErrorList {
	if pa == nil {
		return field.ErrorList{field.Required(field.NewPath("spec"), "PodAutoscaler is required")}
	}

	var errs field.ErrorList
	path := field.NewPath("spec", "scheduledBounds")
	names := make(map[string]int)
	for i, scheduled := range pa.Spec.ScheduledBounds {
		schedulePath := path.Index(i)
		if scheduled.Name == "" {
			errs = append(errs, field.Required(schedulePath.Child("name"), "must be specified"))
		} else if previous, exists := names[scheduled.Name]; exists {
			errs = append(errs, field.Duplicate(schedulePath.Child("name"), fmt.Sprintf("duplicates scheduledBounds[%d].name", previous)))
		} else {
			names[scheduled.Name] = i
		}

		if _, err := cron.ParseStandard(scheduled.Cron); err != nil {
			errs = append(errs, field.Invalid(schedulePath.Child("cron"), scheduled.Cron, err.Error()))
		}
		if scheduled.Duration.Duration <= 0 {
			errs = append(errs, field.Invalid(schedulePath.Child("duration"), scheduled.Duration.Duration.String(), "must be positive"))
		}
		if _, err := scheduledBoundsLocation(scheduled.Timezone); err != nil {
			errs = append(errs, field.Invalid(schedulePath.Child("timezone"), scheduled.Timezone, err.Error()))
		}
		if scheduled.StartTime != nil && scheduled.EndTime != nil && !scheduled.StartTime.Before(scheduled.EndTime) {
			errs = append(errs, field.Invalid(schedulePath, scheduled, "startTime must be earlier than endTime"))
		}
		if scheduled.MinReplicas == nil && scheduled.MaxReplicas == nil {
			errs = append(errs, field.Required(schedulePath, "at least one of minReplicas or maxReplicas must be specified"))
			continue
		}

		minReplicas := baseMinReplicas(pa)
		if scheduled.MinReplicas != nil {
			minReplicas = *scheduled.MinReplicas
		}
		maxReplicas := pa.Spec.MaxReplicas
		if scheduled.MaxReplicas != nil {
			maxReplicas = *scheduled.MaxReplicas
		}
		if minReplicas < 0 {
			errs = append(errs, field.Invalid(schedulePath.Child("minReplicas"), minReplicas, "effective minReplicas must not be negative"))
		}
		if maxReplicas <= 0 {
			errs = append(errs, field.Invalid(schedulePath.Child("maxReplicas"), maxReplicas, "effective maxReplicas must be positive"))
		}
		if minReplicas > maxReplicas {
			errs = append(errs, field.Invalid(schedulePath, scheduled, "effective minReplicas must not be greater than effective maxReplicas"))
		}
	}

	for i := range pa.Spec.ScheduledBounds {
		for j := 0; j < i; j++ {
			if obviousScheduleOverlap(pa.Spec.ScheduledBounds[j], pa.Spec.ScheduledBounds[i]) {
				errs = append(errs, field.Invalid(path.Index(i), pa.Spec.ScheduledBounds[i].Name, "overlaps another scheduled bounds window"))
			}
		}
	}

	return errs
}

func isScheduleActive(scheduled autoscalingv1alpha1.ScheduledReplicaBounds, now time.Time) bool {
	if scheduled.StartTime != nil && now.Before(scheduled.StartTime.Time) {
		return false
	}
	if scheduled.EndTime != nil && !now.Before(scheduled.EndTime.Time) {
		return false
	}

	location, _ := scheduledBoundsLocation(scheduled.Timezone)
	schedule, _ := cron.ParseStandard(scheduled.Cron)
	localNow := now.In(location)
	occurrence := schedule.Next(localNow.Add(-scheduled.Duration.Duration - time.Nanosecond))
	var latest time.Time
	for !occurrence.After(localNow) {
		latest = occurrence
		occurrence = schedule.Next(occurrence)
	}

	return !latest.IsZero() && localNow.Before(latest.Add(scheduled.Duration.Duration))
}

func baseMinReplicas(pa *autoscalingv1alpha1.PodAutoscaler) int32 {
	if pa.Spec.MinReplicas != nil {
		return *pa.Spec.MinReplicas
	}
	return 1
}

func scheduledBoundsLocation(timezone string) (*time.Location, error) {
	if timezone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(timezone)
}

func obviousScheduleOverlap(left, right autoscalingv1alpha1.ScheduledReplicaBounds) bool {
	if left.Cron != right.Cron || left.Timezone != right.Timezone {
		return false
	}
	return lifetimesOverlap(left.StartTime, left.EndTime, right.StartTime, right.EndTime)
}

func lifetimesOverlap(leftStart, leftEnd, rightStart, rightEnd *metav1.Time) bool {
	if leftEnd != nil && rightStart != nil && !rightStart.Time.Before(leftEnd.Time) {
		return false
	}
	if rightEnd != nil && leftStart != nil && !leftStart.Time.Before(rightEnd.Time) {
		return false
	}
	return true
}
