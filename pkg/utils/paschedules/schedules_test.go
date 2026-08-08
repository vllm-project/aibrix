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

package paschedules

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"k8s.io/utils/ptr"
)

func TestResolveScheduledBounds(t *testing.T) {
	pa := scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
		Name:        "business-hours",
		Timezone:    "Asia/Shanghai",
		DaysOfWeek:  []string{"mon", "TUE", "Wed"},
		StartTime:   "09:00",
		EndTime:     "18:00",
		MinReplicas: ptr.To[int32](3),
		MaxReplicas: ptr.To[int32](12),
	}})

	bounds, err := Resolve(pa, mustTime("2026-08-03T02:00:00Z"))
	require.NoError(t, err)
	assert.Equal(t, Bounds{MinReplicas: 3, MaxReplicas: 12, ActiveSchedule: "business-hours", ActiveScheduleHasMinReplicas: true}, bounds)

	bounds, err = Resolve(pa, mustTime("2026-08-03T10:00:00Z"))
	require.NoError(t, err)
	assert.Equal(t, Bounds{MinReplicas: 1, MaxReplicas: 10}, bounds)
}

func TestResolveUsesUTCAndEveryDayDefaults(t *testing.T) {
	pa := scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
		Name:        "utc-window",
		StartTime:   "09:00",
		EndTime:     "10:00",
		MinReplicas: ptr.To[int32](2),
	}})

	bounds, err := Resolve(pa, mustTime("2026-08-08T09:30:00Z"))
	require.NoError(t, err)
	assert.Equal(t, Bounds{MinReplicas: 2, MaxReplicas: 10, ActiveSchedule: "utc-window", ActiveScheduleHasMinReplicas: true}, bounds)
}

func TestResolveUsesHalfOpenWindow(t *testing.T) {
	pa := scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
		Name:        "half-open",
		StartTime:   "09:00",
		EndTime:     "10:00",
		MinReplicas: ptr.To[int32](2),
	}})

	bounds, err := Resolve(pa, mustTime("2026-08-08T09:00:00Z"))
	require.NoError(t, err)
	assert.Equal(t, "half-open", bounds.ActiveSchedule)

	bounds, err = Resolve(pa, mustTime("2026-08-08T10:00:00Z"))
	require.NoError(t, err)
	assert.Empty(t, bounds.ActiveSchedule)
	assert.Equal(t, int32(1), bounds.MinReplicas)
}

func TestValidateSchedules(t *testing.T) {
	tests := []struct {
		name    string
		pa      *autoscalingv1alpha1.PodAutoscaler
		wantErr string
	}{
		{
			name: "omitted schedules are valid",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
			}},
		},
		{
			name: "empty schedules are invalid",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				Schedules:   []autoscalingv1alpha1.PodAutoscalerSchedule{},
			}},
			wantErr: "must not be empty",
		},
		{
			name:    "non zero padded start time is invalid",
			pa:      scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{validSchedule("bad-time", "9:00", "18:00")}),
			wantErr: "startTime",
		},
		{
			name:    "seconds in end time are invalid",
			pa:      scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{validSchedule("bad-time", "09:00", "18:00:00")}),
			wantErr: "endTime",
		},
		{
			name:    "cross midnight is invalid",
			pa:      scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{validSchedule("overnight", "18:00", "09:00")}),
			wantErr: "startTime must be earlier than endTime",
		},
		{
			name: "empty daysOfWeek is invalid",
			pa: scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
				Name:        "empty-days",
				DaysOfWeek:  []string{},
				StartTime:   "09:00",
				EndTime:     "18:00",
				MinReplicas: ptr.To[int32](2),
			}}),
			wantErr: "daysOfWeek must not be empty",
		},
		{
			name: "full weekday names are invalid",
			pa: scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
				Name:        "full-days",
				DaysOfWeek:  []string{"Monday"},
				StartTime:   "09:00",
				EndTime:     "18:00",
				MinReplicas: ptr.To[int32](2),
			}}),
			wantErr: "unsupported weekday",
		},
		{
			name: "marker only schedules are invalid",
			pa: scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
				Name:      "marker",
				StartTime: "09:00",
				EndTime:   "18:00",
			}}),
			wantErr: "at least one",
		},
		{
			name:    "duplicate names are invalid",
			pa:      scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{validSchedule("same", "09:00", "10:00"), validSchedule("same", "10:00", "11:00")}),
			wantErr: "duplicate",
		},
		{
			name:    "overlapping schedules are invalid",
			pa:      scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{validSchedule("one", "09:00", "11:00"), validSchedule("two", "10:00", "12:00")}),
			wantErr: "overlaps",
		},
		{
			name:    "adjacent schedules are valid",
			pa:      scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{validSchedule("one", "09:00", "10:00"), validSchedule("two", "10:00", "11:00")}),
			wantErr: "",
		},
		{
			name: "mixed timezones are invalid",
			pa: scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{
				{
					Name:        "utc",
					Timezone:    "UTC",
					StartTime:   "09:00",
					EndTime:     "10:00",
					MinReplicas: ptr.To[int32](2),
				},
				{
					Name:        "los-angeles",
					Timezone:    "America/Los_Angeles",
					StartTime:   "02:30",
					EndTime:     "03:30",
					MinReplicas: ptr.To[int32](3),
				},
			}),
			wantErr: "mixed timezone",
		},
		{
			name: "effective min greater than max is invalid",
			pa: scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
				Name:        "bad-bounds",
				StartTime:   "09:00",
				EndTime:     "18:00",
				MinReplicas: ptr.To[int32](11),
			}}),
			wantErr: "greater than",
		},
		{
			name: "invalid schedule name is invalid",
			pa: scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
				Name:        "Bad_Name",
				StartTime:   "09:00",
				EndTime:     "18:00",
				MinReplicas: ptr.To[int32](2),
			}}),
			wantErr: "DNS label",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := Validate(tt.pa)
			if tt.wantErr == "" {
				require.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Contains(t, errs[0].Error(), tt.wantErr)
		})
	}
}

func TestNextTransition(t *testing.T) {
	pa := scheduledPA([]autoscalingv1alpha1.PodAutoscalerSchedule{{
		Name:        "business-hours",
		StartTime:   "09:00",
		EndTime:     "18:00",
		MinReplicas: ptr.To[int32](2),
	}})

	next, ok, err := NextTransition(pa, mustTime("2026-08-08T08:30:00Z"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 30*time.Minute, next)

	next, ok, err = NextTransition(pa, mustTime("2026-08-08T09:30:00Z"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 8*time.Hour+30*time.Minute, next)
}

func scheduledPA(schedules []autoscalingv1alpha1.PodAutoscalerSchedule) *autoscalingv1alpha1.PodAutoscaler {
	return &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MinReplicas: ptr.To[int32](1),
			MaxReplicas: 10,
			Schedules:   schedules,
		},
	}
}

func validSchedule(name, start, end string) autoscalingv1alpha1.PodAutoscalerSchedule {
	return autoscalingv1alpha1.PodAutoscalerSchedule{
		Name:        name,
		StartTime:   start,
		EndTime:     end,
		MinReplicas: ptr.To[int32](2),
	}
}

func mustTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}
