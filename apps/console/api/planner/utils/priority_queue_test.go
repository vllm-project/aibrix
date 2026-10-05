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
	"reflect"
	"testing"
)

type priorityQueueTestItem string

func (item priorityQueueTestItem) Key() string {
	return string(item)
}

func TestPriorityQueueForEachUsesPriorityAndFIFOOrder(t *testing.T) {
	queue := NewPriorityQueue[priorityQueueTestItem]()
	queue.Push("low", 1)
	queue.Push("high-first", 2)
	queue.Push("high-second", 2)

	var got []string
	queue.ForEach(func(item priorityQueueTestItem) bool {
		got = append(got, string(item))
		return true
	})

	want := []string{"high-first", "high-second", "low"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ForEach order = %v, want %v", got, want)
	}
}
