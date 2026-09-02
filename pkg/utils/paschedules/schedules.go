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

package paschedules

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

const minutesPerDay = 24 * 60

var timeOfDayPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
var scheduleNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type Bounds struct {
	MinReplicas                  int32
	MaxReplicas                  int32
	ActiveSchedule               string
	ActiveScheduleHasMinReplicas bool
}

type parsedSchedule struct {
	schedule    autoscalingv1alpha1.PodAutoscalerSchedule
	location    *time.Location
	weekdays    [7]bool
	startMinute int
	endMinute   int
	minReplicas int32
	maxReplicas int32
}

func BaseBounds(pa *autoscalingv1alpha1.PodAutoscaler) Bounds {
	bounds := Bounds{MaxReplicas: pa.Spec.MaxReplicas}
	if pa.Spec.MinReplicas != nil {
		bounds.MinReplicas = *pa.Spec.MinReplicas
	} else {
		bounds.MinReplicas = 1
	}
	return bounds
}

func Resolve(pa *autoscalingv1alpha1.PodAutoscaler, now time.Time) (Bounds, error) {
	base := BaseBounds(pa)
	parsed, err := parseAll(pa)
	if err != nil {
		return Bounds{}, err
	}
	for _, schedule := range parsed {
		if !schedule.activeAt(now) {
			continue
		}
		return Bounds{
			MinReplicas:                  schedule.minReplicas,
			MaxReplicas:                  schedule.maxReplicas,
			ActiveSchedule:               schedule.schedule.Name,
			ActiveScheduleHasMinReplicas: schedule.schedule.MinReplicas != nil,
		}, nil
	}
	return base, nil
}

func NextTransition(pa *autoscalingv1alpha1.PodAutoscaler, now time.Time) (time.Duration, bool, error) {
	parsed, err := parseAll(pa)
	if err != nil {
		return 0, false, err
	}
	var next time.Time
	for _, schedule := range parsed {
		candidate, ok := schedule.nextTransitionAfter(now)
		if !ok {
			continue
		}
		if next.IsZero() || candidate.Before(next) {
			next = candidate
		}
	}
	if next.IsZero() {
		return 0, false, nil
	}
	d := next.Sub(now)
	if d < 0 {
		d = 0
	}
	return d, true, nil
}

func Validate(pa *autoscalingv1alpha1.PodAutoscaler) []error {
	_, err := parseAll(pa)
	if err != nil {
		return []error{err}
	}
	return nil
}

func parseAll(pa *autoscalingv1alpha1.PodAutoscaler) ([]parsedSchedule, error) {
	if pa == nil {
		return nil, fmt.Errorf("PodAutoscaler is nil")
	}
	if pa.Spec.Schedules != nil && len(pa.Spec.Schedules) == 0 {
		return nil, fmt.Errorf("schedules must not be empty")
	}
	if len(pa.Spec.Schedules) == 0 {
		return nil, nil
	}

	base := BaseBounds(pa)
	parsed := make([]parsedSchedule, 0, len(pa.Spec.Schedules))
	names := map[string]struct{}{}
	timezone := normalizedTimezone(pa.Spec.Schedules[0].Timezone)
	for i, schedule := range pa.Spec.Schedules {
		if current := normalizedTimezone(schedule.Timezone); current != timezone {
			return nil, fmt.Errorf("schedules[%d].timezone: mixed timezone values are not supported", i)
		}
		item, err := parseSchedule(schedule, base)
		if err != nil {
			return nil, fmt.Errorf("schedules[%d]: %w", i, err)
		}
		if _, ok := names[item.schedule.Name]; ok {
			return nil, fmt.Errorf("schedules[%d].name: duplicate name %q", i, item.schedule.Name)
		}
		names[item.schedule.Name] = struct{}{}
		parsed = append(parsed, item)
	}

	for i := range parsed {
		for j := 0; j < i; j++ {
			if schedulesOverlap(parsed[j], parsed[i]) {
				return nil, fmt.Errorf("schedules[%d]: overlaps schedule %q", i, parsed[j].schedule.Name)
			}
		}
	}
	return parsed, nil
}

func normalizedTimezone(value string) string {
	if value == "" {
		return "UTC"
	}
	return value
}

func parseSchedule(schedule autoscalingv1alpha1.PodAutoscalerSchedule, base Bounds) (parsedSchedule, error) {
	if schedule.Name == "" {
		return parsedSchedule{}, fmt.Errorf("name is required")
	}
	if !scheduleNamePattern.MatchString(schedule.Name) {
		return parsedSchedule{}, fmt.Errorf("name must use Kubernetes DNS label style")
	}
	location := time.UTC
	if schedule.Timezone != "" {
		var err error
		location, err = time.LoadLocation(schedule.Timezone)
		if err != nil {
			return parsedSchedule{}, fmt.Errorf("timezone: %w", err)
		}
	}
	weekdays, err := parseWeekdays(schedule.DaysOfWeek)
	if err != nil {
		return parsedSchedule{}, err
	}
	start, err := parseTimeOfDay(schedule.StartTime)
	if err != nil {
		return parsedSchedule{}, fmt.Errorf("startTime: %w", err)
	}
	end, err := parseTimeOfDay(schedule.EndTime)
	if err != nil {
		return parsedSchedule{}, fmt.Errorf("endTime: %w", err)
	}
	if start >= end {
		return parsedSchedule{}, fmt.Errorf("startTime must be earlier than endTime")
	}
	if schedule.MinReplicas == nil && schedule.MaxReplicas == nil {
		return parsedSchedule{}, fmt.Errorf("at least one of minReplicas or maxReplicas must be set")
	}

	minReplicas := base.MinReplicas
	if schedule.MinReplicas != nil {
		minReplicas = *schedule.MinReplicas
	}
	maxReplicas := base.MaxReplicas
	if schedule.MaxReplicas != nil {
		maxReplicas = *schedule.MaxReplicas
	}
	if minReplicas < 0 {
		return parsedSchedule{}, fmt.Errorf("effective minReplicas must be non-negative")
	}
	if maxReplicas <= 0 {
		return parsedSchedule{}, fmt.Errorf("effective maxReplicas must be positive")
	}
	if minReplicas > maxReplicas {
		return parsedSchedule{}, fmt.Errorf("effective minReplicas must not be greater than effective maxReplicas")
	}

	return parsedSchedule{
		schedule:    schedule,
		location:    location,
		weekdays:    weekdays,
		startMinute: start,
		endMinute:   end,
		minReplicas: minReplicas,
		maxReplicas: maxReplicas,
	}, nil
}

func parseTimeOfDay(value string) (int, error) {
	if !timeOfDayPattern.MatchString(value) {
		return 0, fmt.Errorf("must use HH:MM")
	}
	return int(value[0]-'0')*10*60 + int(value[1]-'0')*60 + int(value[3]-'0')*10 + int(value[4]-'0'), nil
}

func parseWeekdays(values []string) ([7]bool, error) {
	var weekdays [7]bool
	if values == nil {
		for i := range weekdays {
			weekdays[i] = true
		}
		return weekdays, nil
	}
	if len(values) == 0 {
		return weekdays, fmt.Errorf("daysOfWeek must not be empty")
	}
	for _, value := range values {
		weekday, ok := weekdayNames[strings.ToLower(value)]
		if !ok {
			return weekdays, fmt.Errorf("daysOfWeek: unsupported weekday %q", value)
		}
		weekdays[weekday] = true
	}
	return weekdays, nil
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

func (s parsedSchedule) activeAt(now time.Time) bool {
	local := now.In(s.location)
	if !s.weekdays[local.Weekday()] {
		return false
	}
	minute := local.Hour()*60 + local.Minute()
	return minute >= s.startMinute && minute < s.endMinute
}

func (s parsedSchedule) nextTransitionAfter(now time.Time) (time.Time, bool) {
	local := now.In(s.location)
	startOfDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.location)
	for day := 0; day <= 7; day++ {
		base := startOfDay.AddDate(0, 0, day)
		if !s.weekdays[base.Weekday()] {
			continue
		}
		for _, minute := range []int{s.startMinute, s.endMinute} {
			candidate := base.Add(time.Duration(minute) * time.Minute)
			if candidate.After(local) {
				return candidate.In(time.UTC), true
			}
		}
	}
	return time.Time{}, false
}

func schedulesOverlap(left, right parsedSchedule) bool {
	for day := time.Sunday; day <= time.Saturday; day++ {
		if left.weekdays[day] && right.weekdays[day] &&
			left.startMinute < right.endMinute &&
			right.startMinute < left.endMinute {
			return true
		}
	}
	return false
}
