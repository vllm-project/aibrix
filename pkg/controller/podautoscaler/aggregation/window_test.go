/*
Copyright 2020 The Knative Authors

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

package aggregation

import (
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	win := newWindow(5)
	values := []float64{0, 1, 2, 3, 4}
	indices := []int{0, 1, 2, 3, 4}

	for i, v := range values {
		win.Record(v, indices[i])
	}

	if max, err := win.Max(); err != nil || max != 4 {
		t.Errorf("Expected max 4 after sliding out, got %f err: %v", max, err)
	}
	if min, err := win.Min(); err != nil || min != 0 {
		t.Errorf("Expected min 0 after sliding out, got %f err: %v", min, err)
	}

	// Test sliding out old values
	win.Record(6, 6) // This should slide out the 1
	if min, err := win.Min(); err != nil || min != 2 {
		t.Errorf("Expected min 2 after sliding out, got %f err: %v", min, err)
	}

	win.Record(8, 8) // This should slide out the 1
	if min, err := win.Min(); err != nil || min != 4 {
		t.Errorf("Expected min 2 after sliding out, got %f err: %v", min, err)
	}
}

func TestWindowWithExpiredData(t *testing.T) {
	win := newWindow(5)
	values := []float64{5, 6, 7, 8, 9} // Initial values
	indices := []int{0, 1, 2, 3, 4}    // Initial indices

	// Populate the window with initial values
	for i, v := range values {
		win.Record(v, indices[i])
	}

	// Record new values that should trigger expiration of the earliest entries
	win.Record(10, 10) // This should expire all entries with indices 0, 1, 2, 3 (value 5, 6, 7, 8)
	win.Record(11, 11) // This should expire the entry with index 4 (value 9)
	win.Record(12, 12) // Continue adding fresh data

	// After expirations, the window should now contain [10, 11, 12]
	if max, err := win.Max(); err != nil || max != 12 {
		t.Errorf("Expected max 12 after sliding out, got %f err: %v", max, err)
	}
	if min, err := win.Min(); err != nil || min != 10 {
		t.Errorf("Expected min 10 after sliding out, got %f err: %v", min, err)
	}

	// Further test to ensure completely sliding out all old values
	win.Record(15, 15)
	win.Record(16, 16)
	win.Record(17, 17)

	if max, err := win.Max(); err != nil || max != 17 {
		t.Errorf("Expected max 17 after sliding out all old values, got %f err: %v", max, err)
	}
	if min, err := win.Min(); err != nil || min != 15 {
		t.Errorf("Expected min 15 after sliding out all old values, got %f err: %v", min, err)
	}
}
func TestTimeWindow(t *testing.T) {
	tw := NewTimeWindow(5*time.Second, 1*time.Second)
	//now := time.Now()
	now := time.Time{}
	values := []float64{0, 1, 2, 3, 4}

	for i, v := range values {
		tw.Record(now.Add(time.Duration(i)*time.Second), v)
	}

	if max, err := tw.Max(); err != nil || max != 4 {
		t.Errorf("Expected max 4, got %f, err: %v", max, err)
	}

	// test sliding out old values
	tw.Record(now.Add(time.Duration(8)*time.Second), 8)

	if min, err := tw.Min(); err != nil || min != 4 {
		t.Errorf("Expected min 4, got %f, err: %v", min, err)
	}

	// test case of circular array
	tw.Record(now.Add(time.Duration(9)*time.Second), 9)

	if min, err := tw.Min(); err != nil || min != 8 {
		t.Errorf("Entry 8, the valid min bound is 9-5+1=5. Expected min 8, got %f err: %v", min, err)
	}
}

func TestDelayWindow(t *testing.T) {
	delayWindow := NewTimeWindow(10*time.Second, 1*time.Second)

	now := time.Now()
	desiredPodCount := float64(10)
	delayWindow.Record(now, desiredPodCount)

	// Simulate a short delay and a new decision within the same interval
	delayWindow.Record(now.Add(500*time.Millisecond), desiredPodCount-1)
	delayedPodCount, err := delayWindow.Max()
	if err != nil {
		t.Errorf("Unexpected error getting max delayed pod count: %v", err)
	}
	if delayedPodCount != desiredPodCount {
		t.Errorf("Expected delayed count to match original, got %f", delayedPodCount)
	}
}
