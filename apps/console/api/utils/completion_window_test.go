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
	"testing"
	"time"
)

func TestParseCompletionWindow(t *testing.T) {
	tests := map[string]time.Duration{
		"72m":      72 * time.Minute,
		"1d1h1min": 25*time.Hour + time.Minute,
	}
	for value, want := range tests {
		t.Run(value, func(t *testing.T) {
			got, err := ParseCompletionWindow(value)
			if err != nil {
				t.Fatalf("ParseCompletionWindow: %v", err)
			}
			if got != want {
				t.Fatalf("duration = %v, want %v", got, want)
			}
		})
	}
}

func TestFormatCompletionWindow(t *testing.T) {
	tests := map[time.Duration]string{
		98 * time.Minute: "1h38min",
		25*time.Hour + time.Minute + 59*time.Second:  "1d1h1min",
		2*24*time.Hour + 3*time.Hour + 4*time.Second: "2d3h",
	}
	for duration, want := range tests {
		t.Run(want, func(t *testing.T) {
			got, err := FormatCompletionWindow(duration)
			if err != nil {
				t.Fatalf("FormatCompletionWindow: %v", err)
			}
			if got != want {
				t.Fatalf("formatted = %q, want %q", got, want)
			}
		})
	}
}

func TestParseCompletionWindowRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "0min", "59s", "1.5h", "38min1h"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseCompletionWindow(value); err == nil {
				t.Fatalf("ParseCompletionWindow(%q) unexpectedly succeeded", value)
			}
		})
	}
}
