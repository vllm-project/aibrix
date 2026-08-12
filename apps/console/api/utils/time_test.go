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

// TestUnixToTimePtrIsUTC: the result is written straight into a DB column, so
// its zone must come from the value rather than from the container's TZ.
func TestUnixToTimePtrIsUTC(t *testing.T) {
	got := UnixToTimePtr(1786507994)
	if got == nil {
		t.Fatal("UnixToTimePtr returned nil for a positive timestamp")
	}
	if loc := got.Location(); loc != time.UTC {
		t.Errorf("location = %v, want UTC", loc)
	}
	if want := "2026-08-12T04:13:14"; got.Format("2006-01-02T15:04:05") != want {
		t.Errorf("wall clock = %s, want %s", got.Format("2006-01-02T15:04:05"), want)
	}
	if got.Unix() != 1786507994 {
		t.Errorf("instant changed: %d", got.Unix())
	}
}

func TestUnixToTimePtrNilForNonPositive(t *testing.T) {
	for _, sec := range []int64{0, -1} {
		if got := UnixToTimePtr(sec); got != nil {
			t.Errorf("UnixToTimePtr(%d) = %v, want nil", sec, got)
		}
	}
}
