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

package utils

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var completionWindowPattern = regexp.MustCompile(`^(?:([0-9]+)d)?(?:([0-9]+)h)?(?:([0-9]+)(?:min|m))?$`)

// ParseCompletionWindow parses a positive completion window composed of days,
// hours, and minutes, for example "1d2h", "1h38min", or "72min".
func ParseCompletionWindow(value string) (time.Duration, error) {
	normalized := strings.TrimSpace(value)
	matches := completionWindowPattern.FindStringSubmatch(normalized)
	if normalized == "" || matches == nil {
		return 0, fmt.Errorf("must use a positive combination of d, h, and min (or m)")
	}

	units := []time.Duration{24 * time.Hour, time.Hour, time.Minute}
	var duration time.Duration
	for index, unit := range units {
		if matches[index+1] == "" {
			continue
		}
		count, err := strconv.ParseInt(matches[index+1], 10, 64)
		if err != nil || count > int64((time.Duration(1<<63-1)-duration)/unit) {
			return 0, fmt.Errorf("completion window is too large")
		}
		duration += time.Duration(count) * unit
	}
	if duration <= 0 {
		return 0, fmt.Errorf("must use a positive combination of d, h, and min (or m)")
	}
	return duration, nil
}

// FormatCompletionWindow rounds a positive duration down to the nearest minute
// and formats it using the largest applicable units in d, h, min order.
func FormatCompletionWindow(duration time.Duration) (string, error) {
	duration = duration.Truncate(time.Minute)
	if duration < time.Minute {
		return "", fmt.Errorf("completion window must be at least 1min")
	}

	remaining := duration
	units := []struct {
		duration time.Duration
		suffix   string
	}{
		{24 * time.Hour, "d"},
		{time.Hour, "h"},
		{time.Minute, "min"},
	}
	var formatted strings.Builder
	for _, unit := range units {
		count := remaining / unit.duration
		if count == 0 {
			continue
		}
		fmt.Fprintf(&formatted, "%d%s", count, unit.suffix)
		remaining %= unit.duration
	}
	return formatted.String(), nil
}
