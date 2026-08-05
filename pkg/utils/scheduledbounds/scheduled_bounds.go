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

package scheduledbounds

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const ValidationWeekDays = 7

const MaxDuration = ValidationWeekDays * 24 * time.Hour

type Cron struct {
	minute   int
	hours    [24]bool
	weekdays [7]bool
}

type window struct {
	start time.Time
	end   time.Time
}

func Location(timezone string) (*time.Location, error) {
	if timezone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(timezone)
}

func ParseCron(expression string) (Cron, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return Cron{}, fmt.Errorf("unsupported cron syntax: expected five fields")
	}

	minute, err := parseNumber(fields[0], 0, 59)
	if err != nil {
		return Cron{}, fmt.Errorf("unsupported cron syntax: minute must be a single number from 0 to 59")
	}
	hours, err := parseNumberSet(fields[1], 0, 23, nil)
	if err != nil {
		return Cron{}, fmt.Errorf("unsupported cron syntax: hour must be numeric values or ranges from 0 to 23")
	}
	if fields[2] != "*" {
		return Cron{}, fmt.Errorf("unsupported cron syntax: day-of-month must be *")
	}
	if fields[3] != "*" {
		return Cron{}, fmt.Errorf("unsupported cron syntax: month must be *")
	}
	weekdays, err := parseNumberSet(fields[4], 0, 7, weekdayNames)
	if err != nil {
		return Cron{}, fmt.Errorf("unsupported cron syntax: day-of-week must be *, values, or ranges")
	}

	var schedule Cron
	schedule.minute = minute
	for _, hour := range hours {
		schedule.hours[hour] = true
	}
	for _, weekday := range weekdays {
		schedule.weekdays[weekday%7] = true
	}
	return schedule, nil
}

func IsActive(scheduled autoscalingv1alpha1.ScheduledReplicaBounds, now time.Time) bool {
	if scheduled.StartTime != nil && now.Before(scheduled.StartTime.Time) {
		return false
	}
	if scheduled.EndTime != nil && !now.Before(scheduled.EndTime.Time) {
		return false
	}

	location, err := Location(scheduled.Timezone)
	if err != nil {
		return false
	}
	schedule, err := ParseCron(scheduled.Cron)
	if err != nil {
		return false
	}
	localNow := now.In(location)
	latest, found := schedule.latestOccurrence(localNow)

	return found && localNow.Before(latest.Add(scheduled.Duration.Duration))
}

func Overlap(left, right autoscalingv1alpha1.ScheduledReplicaBounds) bool {
	if !lifetimesOverlap(left.StartTime, left.EndTime, right.StartTime, right.EndTime) {
		return false
	}

	leftSchedule, err := ParseCron(left.Cron)
	if err != nil {
		return false
	}
	rightSchedule, err := ParseCron(right.Cron)
	if err != nil {
		return false
	}
	leftLocation, err := Location(left.Timezone)
	if err != nil {
		return false
	}
	rightLocation, err := Location(right.Timezone)
	if err != nil {
		return false
	}

	weekStart, weekEnd := overlapWeek(left, right)
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

var weekdayNames = map[string]int{
	"SUN": 0,
	"MON": 1,
	"TUE": 2,
	"WED": 3,
	"THU": 4,
	"FRI": 5,
	"SAT": 6,
}

func parseNumberSet(field string, min, max int, names map[string]int) ([]int, error) {
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
			start, err := parseValue(bounds[0], min, max, names)
			if err != nil {
				return nil, err
			}
			end, err := parseValue(bounds[1], min, max, names)
			if err != nil || start > end {
				return nil, fmt.Errorf("invalid range")
			}
			for value := start; value <= end; value++ {
				values = append(values, value)
			}
			continue
		}

		value, err := parseValue(part, min, max, names)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func parseValue(value string, min, max int, names map[string]int) (int, error) {
	if names != nil {
		if named, ok := names[strings.ToUpper(value)]; ok {
			return named, nil
		}
	}
	return parseNumber(value, min, max)
}

func parseNumber(value string, min, max int) (int, error) {
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

func (schedule Cron) latestOccurrence(now time.Time) (time.Time, bool) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var latest time.Time
	for dayOffset := 0; dayOffset <= ValidationWeekDays; dayOffset++ {
		day := dayStart.AddDate(0, 0, -dayOffset)
		for hour, enabled := range schedule.hours {
			if !enabled || !schedule.weekdays[day.Weekday()] {
				continue
			}
			occurrence := time.Date(day.Year(), day.Month(), day.Day(), hour, schedule.minute, 0, 0, now.Location())
			if occurrence.Year() != day.Year() ||
				occurrence.Month() != day.Month() ||
				occurrence.Day() != day.Day() ||
				occurrence.Hour() != hour ||
				occurrence.Minute() != schedule.minute {
				continue
			}
			if !occurrence.After(now) && (latest.IsZero() || occurrence.After(latest)) {
				latest = occurrence
			}
		}
	}
	return latest, !latest.IsZero()
}

func overlapWeek(left, right autoscalingv1alpha1.ScheduledReplicaBounds) (time.Time, time.Time) {
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
			anchor = left.EndTime.Time.AddDate(0, 0, -ValidationWeekDays)
			hasEnd = true
		}
		if right.EndTime != nil {
			candidate := right.EndTime.Time.AddDate(0, 0, -ValidationWeekDays)
			if !hasEnd || candidate.Before(anchor) {
				anchor = candidate
			}
		}
	}

	dayStart := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC)
	return dayStart, dayStart.AddDate(0, 0, ValidationWeekDays)
}

func (schedule Cron) windowsOverlappingWeek(
	scheduled autoscalingv1alpha1.ScheduledReplicaBounds,
	location *time.Location,
	weekStart, weekEnd time.Time,
) []window {
	localStart := weekStart.In(location)
	dayStart := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), 0, 0, 0, 0, location)
	windows := make([]window, 0)
	for dayOffset := -ValidationWeekDays - 1; dayOffset <= ValidationWeekDays+1; dayOffset++ {
		day := dayStart.AddDate(0, 0, dayOffset)
		if !schedule.weekdays[day.Weekday()] {
			continue
		}
		for hour, enabled := range schedule.hours {
			if !enabled {
				continue
			}
			start := time.Date(day.Year(), day.Month(), day.Day(), hour, schedule.minute, 0, 0, location).UTC()
			localStart := start.In(location)
			if localStart.Year() != day.Year() ||
				localStart.Month() != day.Month() ||
				localStart.Day() != day.Day() ||
				localStart.Hour() != hour ||
				localStart.Minute() != schedule.minute {
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
				windows = append(windows, window{start: start, end: end})
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
