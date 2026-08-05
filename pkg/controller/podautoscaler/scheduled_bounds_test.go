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

package podautoscaler

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestResolveEffectiveReplicaBounds(t *testing.T) {
	utc := time.UTC
	newPA := func(schedules ...autoscalingv1alpha1.ScheduledReplicaBounds) *autoscalingv1alpha1.PodAutoscaler {
		return &autoscalingv1alpha1.PodAutoscaler{
			Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MinReplicas:     ptr.To(int32(2)),
				MaxReplicas:     10,
				ScheduledBounds: schedules,
			},
		}
	}

	tests := []struct {
		name string
		pa   *autoscalingv1alpha1.PodAutoscaler
		now  time.Time
		want effectiveReplicaBounds
	}{
		{
			name: "no schedules uses base bounds",
			pa:   newPA(),
			now:  time.Date(2026, time.August, 5, 9, 30, 0, 0, utc),
			want: effectiveReplicaBounds{MinReplicas: 2, MaxReplicas: 10},
		},
		{
			name: "matching window overrides both bounds",
			pa: newPA(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name:        "morning-peak",
				Cron:        "0 9 * * *",
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(6)),
				MaxReplicas: ptr.To(int32(20)),
			}),
			now:  time.Date(2026, time.August, 5, 9, 30, 0, 0, utc),
			want: effectiveReplicaBounds{MinReplicas: 6, MaxReplicas: 20, ScheduleName: "morning-peak"},
		},
		{
			name: "partial override keeps base maximum",
			pa: newPA(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name:        "minimum-only",
				Cron:        "0 9 * * *",
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(6)),
			}),
			now:  time.Date(2026, time.August, 5, 9, 30, 0, 0, utc),
			want: effectiveReplicaBounds{MinReplicas: 6, MaxReplicas: 10, ScheduleName: "minimum-only"},
		},
		{
			name: "non-matching window uses base bounds",
			pa: newPA(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name:        "morning-peak",
				Cron:        "0 9 * * *",
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(6)),
				MaxReplicas: ptr.To(int32(20)),
			}),
			now:  time.Date(2026, time.August, 5, 11, 0, 0, 0, utc),
			want: effectiveReplicaBounds{MinReplicas: 2, MaxReplicas: 10},
		},
		{
			name: "timezone affects matching",
			pa: newPA(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name:        "new-york-morning",
				Timezone:    "America/New_York",
				Cron:        "0 9 * * *",
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(7)),
				MaxReplicas: ptr.To(int32(18)),
			}),
			now:  time.Date(2026, time.August, 5, 13, 30, 0, 0, utc),
			want: effectiveReplicaBounds{MinReplicas: 7, MaxReplicas: 18, ScheduleName: "new-york-morning"},
		},
		{
			name: "omitted timezone uses UTC",
			pa: newPA(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name:        "utc-morning",
				Cron:        "0 9 * * *",
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(7)),
				MaxReplicas: ptr.To(int32(18)),
			}),
			now:  time.Date(2026, time.August, 5, 9, 30, 0, 0, utc),
			want: effectiveReplicaBounds{MinReplicas: 7, MaxReplicas: 18, ScheduleName: "utc-morning"},
		},
		{
			name: "start and end lifetime gate matching",
			pa: newPA(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name:        "bounded-lifetime",
				Cron:        "0 9 * * *",
				Duration:    metav1.Duration{Duration: time.Hour},
				StartTime:   &metav1.Time{Time: time.Date(2026, time.August, 5, 9, 15, 0, 0, utc)},
				EndTime:     &metav1.Time{Time: time.Date(2026, time.August, 5, 9, 45, 0, 0, utc)},
				MinReplicas: ptr.To(int32(6)),
				MaxReplicas: ptr.To(int32(20)),
			}),
			now:  time.Date(2026, time.August, 5, 9, 15, 0, 0, utc),
			want: effectiveReplicaBounds{MinReplicas: 6, MaxReplicas: 20, ScheduleName: "bounded-lifetime"},
		},
		{
			name: "zero minimum is preserved",
			pa: newPA(autoscalingv1alpha1.ScheduledReplicaBounds{
				Name:        "scale-to-zero",
				Cron:        "0 9 * * *",
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(0)),
			}),
			now:  time.Date(2026, time.August, 5, 9, 30, 0, 0, utc),
			want: effectiveReplicaBounds{MinReplicas: 0, MaxReplicas: 10, ScheduleName: "scale-to-zero"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveEffectiveReplicaBounds(tt.pa, tt.now)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveEffectiveReplicaBoundsRespectsLifetimeBoundaries(t *testing.T) {
	utc := time.UTC
	schedule := autoscalingv1alpha1.ScheduledReplicaBounds{
		Name:        "bounded-lifetime",
		Cron:        "0 9 * * *",
		Duration:    metav1.Duration{Duration: time.Hour},
		StartTime:   &metav1.Time{Time: time.Date(2026, time.August, 5, 9, 15, 0, 0, utc)},
		EndTime:     &metav1.Time{Time: time.Date(2026, time.August, 5, 9, 45, 0, 0, utc)},
		MinReplicas: ptr.To(int32(6)),
	}
	pa := &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
		MinReplicas:     ptr.To(int32(2)),
		MaxReplicas:     10,
		ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{schedule},
	}}

	for _, now := range []time.Time{
		time.Date(2026, time.August, 5, 9, 14, 59, 0, utc),
		time.Date(2026, time.August, 5, 9, 45, 0, 0, utc),
	} {
		got, err := resolveEffectiveReplicaBounds(pa, now)
		require.NoError(t, err)
		assert.Equal(t, effectiveReplicaBounds{MinReplicas: 2, MaxReplicas: 10}, got)
	}
}

func TestResolveEffectiveReplicaBoundsTreatsWindowEndAsExclusive(t *testing.T) {
	pa := &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
		MinReplicas: ptr.To(int32(2)),
		MaxReplicas: 10,
		ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
			Name:        "morning-peak",
			Cron:        "0 9 * * *",
			Duration:    metav1.Duration{Duration: time.Hour},
			MinReplicas: ptr.To(int32(6)),
		}},
	}}

	got, err := resolveEffectiveReplicaBounds(pa, time.Date(2026, time.August, 5, 10, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, effectiveReplicaBounds{MinReplicas: 2, MaxReplicas: 10}, got)
}

func TestValidateScheduledBounds(t *testing.T) {
	validSchedule := func() autoscalingv1alpha1.ScheduledReplicaBounds {
		return autoscalingv1alpha1.ScheduledReplicaBounds{
			Name:        "weekday-peak",
			Cron:        "0 9 * * MON-FRI",
			Duration:    metav1.Duration{Duration: time.Hour},
			MinReplicas: ptr.To(int32(4)),
		}
	}

	tests := []struct {
		name           string
		pa             *autoscalingv1alpha1.PodAutoscaler
		wantError      bool
		wantErrorText  string
		wantFastReject bool
	}{
		{
			name: "accepts simple weekday business hours cron",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MinReplicas:     ptr.To(int32(2)),
				MaxReplicas:     10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{validSchedule()},
			}},
		},
		{
			name: "rejects step cron as unsupported",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
					Name: "frequent", Cron: "*/5 * * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
				}},
			}},
			wantError:      true,
			wantErrorText:  "unsupported cron syntax",
			wantFastReject: true,
		},
		{
			name: "rejects restricted day and month cron as unsupported",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
					Name: "leap-day", Cron: "0 9 29 FEB *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
				}},
			}},
			wantError:     true,
			wantErrorText: "unsupported cron syntax",
		},
		{
			name: "rejects invalid cron",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
					Name: "invalid-cron", Cron: "not cron", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
				}},
			}},
			wantError: true,
		},
		{
			name: "rejects non-positive duration",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
					Name: "invalid-duration", Cron: "0 9 * * *", MinReplicas: ptr.To(int32(1)),
				}},
			}},
			wantError: true,
		},
		{
			name: "rejects invalid timezone",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
					Name: "invalid-timezone", Timezone: "Mars/Olympus_Mons", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
				}},
			}},
			wantError: true,
		},
		{
			name: "rejects duplicate names",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas:     10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{validSchedule(), validSchedule()},
			}},
			wantError: true,
		},
		{
			name: "rejects missing overrides",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
					Name: "missing-overrides", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour},
				}},
			}},
			wantError: true,
		},
		{
			name: "rejects invalid effective bounds",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MinReplicas: ptr.To(int32(2)),
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
					Name: "invalid-effective-bounds", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(11)),
				}},
			}},
			wantError: true,
		},
		{
			name: "rejects invalid lifetime",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
					Name: "invalid-lifetime", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
					StartTime: &metav1.Time{Time: time.Date(2026, time.August, 5, 10, 0, 0, 0, time.UTC)},
					EndTime:   &metav1.Time{Time: time.Date(2026, time.August, 5, 10, 0, 0, 0, time.UTC)},
				}},
			}},
			wantError: true,
		},
		{
			name: "rejects overlapping windows with the same cron and timezone",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{
					{
						Name:        "weekday-peak-a",
						Cron:        "0 9 * * MON-FRI",
						Duration:    metav1.Duration{Duration: time.Hour},
						MinReplicas: ptr.To(int32(4)),
					},
					{
						Name:        "weekday-peak-b",
						Cron:        "0 9 * * MON-FRI",
						Duration:    metav1.Duration{Duration: 30 * time.Minute},
						MinReplicas: ptr.To(int32(5)),
					},
				},
			}},
			wantError: true,
		},
		{
			name: "rejects overlapping windows with different crons",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{
					{
						Name:        "morning-peak",
						Cron:        "0 9 * * *",
						Duration:    metav1.Duration{Duration: 2 * time.Hour},
						MinReplicas: ptr.To(int32(4)),
					},
					{
						Name:        "late-morning-peak",
						Cron:        "0 10 * * *",
						Duration:    metav1.Duration{Duration: time.Hour},
						MinReplicas: ptr.To(int32(5)),
					},
				},
			}},
			wantError: true,
		},
		{
			name: "rejects overlapping weekly windows after a late week lifetime start",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{
					{
						Name:        "monday-peak-a",
						Cron:        "0 9 * * MON",
						Duration:    metav1.Duration{Duration: time.Hour},
						StartTime:   &metav1.Time{Time: time.Date(2026, time.August, 9, 23, 0, 0, 0, time.UTC)},
						MinReplicas: ptr.To(int32(4)),
					},
					{
						Name:        "monday-peak-b",
						Cron:        "0 9 * * MON",
						Duration:    metav1.Duration{Duration: time.Hour},
						StartTime:   &metav1.Time{Time: time.Date(2026, time.August, 9, 23, 0, 0, 0, time.UTC)},
						MinReplicas: ptr.To(int32(5)),
					},
				},
			}},
			wantError: true,
		},
		{
			name: "rejects leap-day cron as unsupported instead of scanning for overlap",
			pa: &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				MaxReplicas: 10,
				ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{
					{
						Name:        "leap-day-morning",
						Cron:        "0 9 29 FEB *",
						Duration:    metav1.Duration{Duration: 2 * time.Hour},
						MinReplicas: ptr.To(int32(4)),
					},
					{
						Name:        "leap-day-late-morning",
						Cron:        "0 10 29 FEB *",
						Duration:    metav1.Duration{Duration: time.Hour},
						MinReplicas: ptr.To(int32(5)),
					},
				},
			}},
			wantError:     true,
			wantErrorText: "unsupported cron syntax",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := time.Now()
			errs := validateScheduledBounds(tt.pa)
			if tt.wantError {
				require.NotEmpty(t, errs)
				if tt.wantErrorText != "" {
					assert.True(t, strings.Contains(errs.ToAggregate().Error(), tt.wantErrorText), "expected error %q to contain %q", errs.ToAggregate(), tt.wantErrorText)
				}
				if tt.wantFastReject {
					assert.Less(t, time.Since(started), time.Second, "unsupported high-frequency cron must be rejected without occurrence scanning")
				}
				return
			}
			assert.Empty(t, errs)
		})
	}
}
