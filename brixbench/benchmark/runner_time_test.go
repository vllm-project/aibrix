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

package benchmark

import (
	"testing"
	"time"
)

func TestFormatScenarioRunIDUsesUTC(t *testing.T) {
	localTime := time.Date(2026, 5, 11, 15, 4, 5, 0, time.FixedZone("PDT", -7*3600))

	runID := formatScenarioRunID(localTime, "AIBrix Hello World")

	const want = "20260511-220405-UTC-aibrix-hello-world"
	if runID != want {
		t.Fatalf("formatScenarioRunID() = %q, want %q", runID, want)
	}
}

func TestNowInUTCUsesUTCLocation(t *testing.T) {
	if got := nowInUTC().Location(); got != time.UTC {
		t.Fatalf("nowInUTC() location = %v, want %v", got, time.UTC)
	}
}
