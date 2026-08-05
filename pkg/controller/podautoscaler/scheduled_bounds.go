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
	"strconv"
	"strings"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const scheduledBoundsValidationWeekDays = 7

type simpleCron struct {
	minute   int
	hours    [24]bool
	weekdays [7]bool
}

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

		if _, err := parseSimpleCron(scheduled.Cron); err != nil {
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
	schedule, err := parseSimpleCron(scheduled.Cron)
	if err != nil {
		return false
	}
	localNow := now.In(location)
	latest, found := schedule.latestOccurrence(localNow)

	return found && localNow.Before(latest.Add(scheduled.Duration.Duration))
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
	if !lifetimesOverlap(left.StartTime, left.EndTime, right.StartTime, right.EndTime) {
		return false
	}

	leftSchedule, err := parseSimpleCron(left.Cron)
	if err != nil {
		return false
	}
	rightSchedule, err := parseSimpleCron(right.Cron)
	if err != nil {
		return false
	}
	leftLocation, err := scheduledBoundsLocation(left.Timezone)
	if err != nil {
		return false
	}
	rightLocation, err := scheduledBoundsLocation(right.Timezone)
	if err != nil {
		return false
	}

	weekStart, weekEnd := scheduledBoundsOverlapWeek(left, right)
	leftWindows := leftSchedule.windowsOverlappingWeek(left, leftLocation, weekStart, weekEnd)
	rightWindows := rightSchedule.windowsOverlappingWeek(right, rightLocation, weekStart, weekEnd)
	for _, leftWindow := range leftWindows {
		for _, rightWindow := range rightWindows {
			if leftWindow.start.Before(rightWindow.end) && rightWindow.start.Before(leftWindow.end) {
				return true
			}
		}
	}
	return false
}

func scheduledBoundsOverlapWeek(left, right autoscalingv1alpha1.ScheduledReplicaBounds) (time.Time, time.Time) {
	anchor := time.Date(2000, time.January, 3, 0, 0, 0, 0, time.UTC)
	hasStart := false
	if left.StartTime != nil && left.StartTime.After(anchor) {
		anchor = left.StartTime.Time
		hasStart = true
	}
	if right.StartTime != nil && right.StartTime.After(anchor) {
		anchor = right.StartTime.Time
		hasStart = true
	}
	if !hasStart {
		hasEnd := false
		if left.EndTime != nil {
			anchor = left.EndTime.Time.AddDate(0, 0, -scheduledBoundsValidationWeekDays)
			hasEnd = true
		}
		if right.EndTime != nil {
			candidate := right.EndTime.Time.AddDate(0, 0, -scheduledBoundsValidationWeekDays)
			if !hasEnd || candidate.Before(anchor) {
				anchor = candidate
			}
		}
	}

	dayStart := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := dayStart.AddDate(0, 0, -((int(dayStart.Weekday()) + 6) % scheduledBoundsValidationWeekDays))
	return weekStart, weekStart.AddDate(0, 0, scheduledBoundsValidationWeekDays)
}

func parseSimpleCron(expression string) (simpleCron, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return simpleCron{}, fmt.Errorf("unsupported cron syntax: expected five fields")
	}

	minute, err := parseCronNumber(fields[0], 0, 59)
	if err != nil {
		return simpleCron{}, fmt.Errorf("unsupported cron syntax: minute must be a single number from 0 to 59")
	}
	hours, err := parseCronNumberSet(fields[1], 0, 23, nil)
	if err != nil {
		return simpleCron{}, fmt.Errorf("unsupported cron syntax: hour must be numeric values or ranges from 0 to 23")
	}
	if fields[2] != "*" {
		return simpleCron{}, fmt.Errorf("unsupported cron syntax: day-of-month must be *")
	}
	if fields[3] != "*" {
		return simpleCron{}, fmt.Errorf("unsupported cron syntax: month must be *")
	}
	weekdays, err := parseCronNumberSet(fields[4], 0, 7, cronWeekdayNames)
	if err != nil {
		return simpleCron{}, fmt.Errorf("unsupported cron syntax: day-of-week must be *, values, or ranges")
	}

	var schedule simpleCron
	schedule.minute = minute
	for _, hour := range hours {
		schedule.hours[hour] = true
	}
	for _, weekday := range weekdays {
		schedule.weekdays[weekday%7] = true
	}
	return schedule, nil
}

var cronWeekdayNames = map[string]int{
	"SUN": 0,
	"MON": 1,
	"TUE": 2,
	"WED": 3,
	"THU": 4,
	"FRI": 5,
	"SAT": 6,
}

func parseCronNumberSet(field string, min, max int, names map[string]int) ([]int, error) {
	if field == "*" {
		if names == nil {
			return nil, fmt.Errorf("wildcards are not supported")
		}
		values := make([]int, 0, max-min+1)
		for value := min; value <= max; value++ {
			values = append(values, value)
		}
		return values, nil
	}

	values := make([]int, 0)
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return nil, fmt.Errorf("empty list value")
		}
		if strings.Count(part, "-") > 1 {
			return nil, fmt.Errorf("invalid range")
		}
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			start, err := parseCronValue(bounds[0], min, max, names)
			if err != nil {
				return nil, err
			}
			end, err := parseCronValue(bounds[1], min, max, names)
			if err != nil || start > end {
				return nil, fmt.Errorf("invalid range")
			}
			for value := start; value <= end; value++ {
				values = append(values, value)
			}
			continue
		}

		value, err := parseCronValue(part, min, max, names)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func parseCronValue(value string, min, max int, names map[string]int) (int, error) {
	if names != nil {
		if named, ok := names[strings.ToUpper(value)]; ok {
			return named, nil
		}
	}
	return parseCronNumber(value, min, max)
}

func parseCronNumber(value string, min, max int) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("not a number")
		}
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < min || number > max {
		return 0, fmt.Errorf("number out of range")
	}
	return number, nil
}

func (schedule simpleCron) latestOccurrence(now time.Time) (time.Time, bool) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var latest time.Time
	for dayOffset := 0; dayOffset <= scheduledBoundsValidationWeekDays; dayOffset++ {
		day := dayStart.AddDate(0, 0, -dayOffset)
		for hour, enabled := range schedule.hours {
			if !enabled || !schedule.weekdays[day.Weekday()] {
				continue
			}
			occurrence := time.Date(day.Year(), day.Month(), day.Day(), hour, schedule.minute, 0, 0, now.Location())
			if occurrence.Year() != day.Year() || occurrence.Month() != day.Month() || occurrence.Day() != day.Day() || occurrence.Hour() != hour || occurrence.Minute() != schedule.minute {
				continue
			}
			if !occurrence.After(now) && (latest.IsZero() || occurrence.After(latest)) {
				latest = occurrence
			}
		}
	}
	return latest, !latest.IsZero()
}

type scheduledWindow struct {
	start time.Time
	end   time.Time
}

func (schedule simpleCron) windowsOverlappingWeek(scheduled autoscalingv1alpha1.ScheduledReplicaBounds, location *time.Location, weekStart, weekEnd time.Time) []scheduledWindow {
	localStart := weekStart.In(location)
	dayStart := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), 0, 0, 0, 0, location)
	windows := make([]scheduledWindow, 0)
	for dayOffset := -scheduledBoundsValidationWeekDays - 1; dayOffset <= scheduledBoundsValidationWeekDays+1; dayOffset++ {
		day := dayStart.AddDate(0, 0, dayOffset)
		if !schedule.weekdays[day.Weekday()] {
			continue
		}
		for hour, enabled := range schedule.hours {
			if !enabled {
				continue
			}
			start := time.Date(day.Year(), day.Month(), day.Day(), hour, schedule.minute, 0, 0, location).UTC()
			if start.In(location).Year() != day.Year() || start.In(location).Month() != day.Month() || start.In(location).Day() != day.Day() || start.In(location).Hour() != hour || start.In(location).Minute() != schedule.minute {
				continue
			}
			end := start.Add(scheduled.Duration.Duration)
			if scheduled.StartTime != nil && start.Before(scheduled.StartTime.Time) {
				start = scheduled.StartTime.Time
			}
			if scheduled.EndTime != nil && scheduled.EndTime.Time.Before(end) {
				end = scheduled.EndTime.Time
			}
			if start.Before(end) && start.Before(weekEnd) && weekStart.Before(end) {
				if start.Before(weekStart) {
					start = weekStart
				}
				if weekEnd.Before(end) {
					end = weekEnd
				}
				windows = append(windows, scheduledWindow{start: start, end: end})
			}
		}
	}
	return windows
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
