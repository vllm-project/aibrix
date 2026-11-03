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

package main

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMergeSlots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slots []int
		want  []SlotRange
	}{
		{name: "no slots", slots: nil, want: nil},
		{name: "one slot", slots: []int{7}, want: []SlotRange{{Start: 7, End: 7}}},
		{
			name:  "unsorted slots",
			slots: []int{5, 3, 4, 9},
			want:  []SlotRange{{Start: 3, End: 5}, {Start: 9, End: 9}},
		},
		{name: "every slot", slots: []int{0, 1, 2, 3}, want: []SlotRange{{Start: 0, End: 3}}},
		{
			name:  "first and last slot with ranges in between",
			slots: []int{4095, 0, 1, 2, 100, 101, 4094},
			want:  []SlotRange{{Start: 0, End: 2}, {Start: 100, End: 101}, {Start: 4094, End: 4095}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mergeSlots(tc.slots))
		})
	}
}

func TestMergedSlotRangesCoverEveryAssignedSlot(t *testing.T) {
	const totalSlots, virtualNodes = 4096, 100
	for _, podCount := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("%d pods", podCount), func(t *testing.T) {
			pods := make([]corev1.Pod, 0, podCount)
			for i := 0; i < podCount; i++ {
				pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("kvcache-%d", i)}})
			}

			assigned := 0
			for name, slots := range calculateSlotDistribution(pods, totalSlots, virtualNodes) {
				want := slices.Clone(slots)
				var got []int
				for _, r := range mergeSlots(slots) {
					require.LessOrEqual(t, r.Start, r.End, "pod %s has range %+v", name, r)
					for slot := r.Start; slot <= r.End; slot++ {
						got = append(got, slot)
					}
				}
				assert.Equal(t, want, got, "pod %s", name)
				assigned += len(got)
			}
			assert.Equal(t, totalSlots, assigned)
		})
	}
}
