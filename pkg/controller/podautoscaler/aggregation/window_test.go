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
	values := []int32{0, 1, 2, 3, 4}
	indices := []int{0, 1, 2, 3, 4}

	for i, v := range values {
		win.Record(v, indices[i])
	}

	if max := win.Max(); max != 4 {
		t.Errorf("Expected max 4, got %d", max)
	}
	if min := win.Min(); min != 0 {
		t.Errorf("Expected min 4, got %d", min)
	}

	// Test sliding out old values
	win.Record(6, 6) // This should slide out the 1
	if min := win.Min(); min != 2 {
		t.Errorf("Expected min 2 after sliding out, got %d", min)
	}

	win.Record(8, 8) // This should slide out the 1
	if min := win.Min(); min != 4 {
		t.Errorf("Expected min 4 after sliding out, got %d", min)
	}
}

func TestTimeWindow(t *testing.T) {
	tw := NewTimeWindow(5*time.Second, 1*time.Second)
	//now := time.Now()
	now := time.Time{}
	values := []int32{0, 1, 2, 3, 4}

	for i, v := range values {
		tw.Record(now.Add(time.Duration(i)*time.Second), v)
	}

	if max := tw.Max(); max != 4 {
		t.Errorf("Expected max 4, got %d", max)
	}

	// test sliding out old values
	tw.Record(now.Add(time.Duration(8)*time.Second), 8)

	if min := tw.Min(); min != 4 {
		t.Errorf("Entry 8, the valid min bound is 8-5+1=4. Expected min 4, got %d", min)
	}

	// test case of circular array
	tw.Record(now.Add(time.Duration(9)*time.Second), 9)

	if min := tw.Min(); min != 8 {
		t.Errorf("Entry 8, the valid min bound is 9-5+1=5. Expected min 8, got %d", min)
	}
}

func TestDelayWindow(t *testing.T) {
	delayWindow := NewTimeWindow(10*time.Second, 1*time.Second)

	now := time.Now()
	desiredPodCount := int32(10)
	delayWindow.Record(now, desiredPodCount)

	// Simulate a short delay and a new decision within the same interval
	delayWindow.Record(now.Add(500*time.Millisecond), desiredPodCount-1)
	delayedPodCount := delayWindow.Max()

	if delayedPodCount != desiredPodCount {
		t.Errorf("Expected delayed count to match original, got %d", delayedPodCount)
	}
}
