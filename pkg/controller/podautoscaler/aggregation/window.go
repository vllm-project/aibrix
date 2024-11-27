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
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

/*
*
Referenced the knative implementation: pkg/autoscaler/aggregation/max/window.go,
but did not use the Ascending Minima Algorithm as we may need other aggregation functions beyond Max.
*/
type entry struct {
	value float64
	index int
}

func (e entry) String() string {
	return fmt.Sprintf("{%d, %.2f}", e.index, e.value)
}

// window is a sliding window that keeps track of recent {size} values.
type window struct {
	valueList     []entry
	first, length int
}

// newWindow creates a new window of specified size.
func newWindow(size int) *window {
	return &window{
		valueList: make([]entry, size),
	}
}

func (w *window) Size() int {
	return len(w.valueList)
}

func (w *window) index(i int) int {
	return i % w.Size()
}

// Record updates the window with a new value and index.
// It also removes all entries that are too old (index too small compared to the new index).
func (w *window) Record(value float64, index int) {
	// Remove elements that are outside the sliding window range.
	for w.length > 0 && w.valueList[w.first].index <= index-w.Size() {
		w.first = w.index(w.first + 1)
		w.length--
	}

	// Add the new value to the valueList.
	if w.length < w.Size() { // Ensure we do not exceed the buffer
		w.valueList[w.index(w.first+w.length)] = entry{value: value, index: index}
		w.length++
	}
}

func (w *window) Max() (float64, error) {
	if w.length == 0 {
		return 0, errors.New("no data available")
	}
	maxValue := w.valueList[w.first].value
	for i := 1; i < w.length; i++ {
		valueIndex := w.index(w.first + i)
		if w.valueList[valueIndex].value > maxValue {
			maxValue = w.valueList[valueIndex].value
		}
	}
	return maxValue, nil
}

func (w *window) Min() (float64, error) {
	if w.length == 0 {
		return 0, errors.New("no data available")
	}
	minValue := w.valueList[w.first].value
	for i := 1; i < w.length; i++ {
		valueIndex := w.index(w.first + i)
		if w.valueList[valueIndex].value < minValue {
			minValue = w.valueList[valueIndex].value
		}
	}
	return minValue, nil
}

func (w *window) Avg() (float64, error) {
	if w.length == 0 {
		return 0, errors.New("no data available")
	}
	sum := 0.0
	count := 0
	for i := 0; i < w.length; i++ {
		index := w.index(w.first + i)
		if w.valueList[index].index > w.valueList[w.first].index-w.Size() {
			sum += w.valueList[index].value
			count++
		}
	}
	if count == 0 {
		return 0, errors.New("no valid data in window")
	}
	return sum / float64(count), nil
}

func (w *window) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("window(size=%d, values=[", w.length))

	for i := 0; i < w.length; i++ {
		idx := (w.first + i) % len(w.valueList)
		sb.WriteString(fmt.Sprintf("%v", w.valueList[idx].String()))
		if i < w.length-1 {
			sb.WriteString(", ")
		}
	}

	sb.WriteString("])")

	return sb.String()
}

type TimeWindow struct {
	window      *window
	granularity time.Duration
}

func (tw *TimeWindow) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("TimeWindow(granularity=%v, window=", tw.granularity))
	sb.WriteString(tw.window.String())
	sb.WriteString(")")

	return sb.String()
}

func NewTimeWindow(duration, granularity time.Duration) *TimeWindow {
	buckets := int(math.Ceil(float64(duration) / float64(granularity)))
	return &TimeWindow{window: newWindow(buckets), granularity: granularity}
}

func (t *TimeWindow) Record(now time.Time, value float64) {
	index := int(now.Unix()) / int(t.granularity.Seconds())
	t.window.Record(value, index)
}

func (t *TimeWindow) Max() (float64, error) {
	return t.window.Max()
}

func (t *TimeWindow) Min() (float64, error) {
	return t.window.Min()
}

func (t *TimeWindow) Avg() (float64, error) {
	return t.window.Avg()
}
