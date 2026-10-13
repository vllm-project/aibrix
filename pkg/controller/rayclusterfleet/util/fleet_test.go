/*
Copyright 2024 The Aibrix Team.

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

package util

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestNewDeploymentCondition(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 123456789, time.UTC)
	oldNowFn, calls := NowFn, 0
	NowFn = func() time.Time {
		calls++
		return now.Add(time.Duration(calls-1) * time.Second)
	}
	t.Cleanup(func() { NowFn = oldNowFn })
	cond := NewDeploymentCondition(orchestrationv1alpha1.RayClusterFleetProgressing, corev1.ConditionUnknown, PausedDeployReason, "Deployment is paused")
	want := orchestrationv1alpha1.RayClusterFleetCondition{
		Type: orchestrationv1alpha1.RayClusterFleetProgressing, Status: corev1.ConditionUnknown,
		Reason: PausedDeployReason, Message: "Deployment is paused",
		LastUpdateTime: metav1.NewTime(now), LastTransitionTime: metav1.NewTime(now),
	}
	if calls != 1 || !reflect.DeepEqual(*cond, want) {
		t.Fatalf("condition = %+v, clock calls = %d, want %+v and one call", cond, calls, want)
	}
	data, err := json.Marshal(cond)
	if err != nil {
		t.Fatal(err)
	}
	var stored orchestrationv1alpha1.RayClusterFleetCondition
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	want.LastUpdateTime, want.LastTransitionTime = metav1.NewTime(now.Truncate(time.Second)), metav1.NewTime(now.Truncate(time.Second))
	if stored.Type != want.Type || stored.Status != want.Status || stored.Reason != want.Reason || stored.Message != want.Message ||
		!stored.LastUpdateTime.Equal(&want.LastUpdateTime) || !stored.LastTransitionTime.Equal(&want.LastTransitionTime) {
		t.Errorf("serialized condition = %+v, want %+v", stored, want)
	}
}

func TestSetDeploymentCondition(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status corev1.ConditionStatus
		reason string
	}{
		{"same status and reason", corev1.ConditionUnknown, PausedDeployReason},
		{"same status, new reason", corev1.ConditionUnknown, ResumedDeployReason},
		{"new status", corev1.ConditionTrue, ReplicaSetUpdatedReason},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			oldNowFn := NowFn
			NowFn = func() time.Time { return now }
			t.Cleanup(func() { NowFn = oldNowFn })
			initial := NewDeploymentCondition(orchestrationv1alpha1.RayClusterFleetProgressing, corev1.ConditionUnknown, PausedDeployReason, "Deployment is paused")
			status := orchestrationv1alpha1.RayClusterFleetStatus{}
			SetDeploymentCondition(&status, *initial)
			now = now.Add(time.Hour)
			candidate := NewDeploymentCondition(initial.Type, tt.status, tt.reason, "new message")
			want := *candidate
			if tt.status == initial.Status {
				want.LastTransitionTime = initial.LastTransitionTime
				if tt.reason == initial.Reason {
					want = *initial
				}
			}
			SetDeploymentCondition(&status, *candidate)
			if len(status.Conditions) != 1 || !reflect.DeepEqual(status.Conditions[0], want) {
				t.Errorf("conditions = %+v, want only %+v", status.Conditions, want)
			}
		})
	}
}

func newRS(annotations map[string]string) *orchestrationv1alpha1.RayClusterReplicaSet {
	rs := &orchestrationv1alpha1.RayClusterReplicaSet{ObjectMeta: metav1.ObjectMeta{}}
	rs.Annotations = annotations
	return rs
}

func TestSetReplicasAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		existing    map[string]string
		desired     int32
		max         int32
		wantUpdated bool
		wantDesired string
		wantMax     string
	}{
		{
			name:        "nil annotations gets set",
			existing:    nil,
			desired:     3,
			max:         5,
			wantUpdated: true,
			wantDesired: "3",
			wantMax:     "5",
		},
		{
			name:        "values already match, no update",
			existing:    map[string]string{DesiredReplicasAnnotation: "3", MaxReplicasAnnotation: "5"},
			desired:     3,
			max:         5,
			wantUpdated: false,
			wantDesired: "3",
			wantMax:     "5",
		},
		{
			name:        "desired changed triggers update",
			existing:    map[string]string{DesiredReplicasAnnotation: "2", MaxReplicasAnnotation: "5"},
			desired:     3,
			max:         5,
			wantUpdated: true,
			wantDesired: "3",
			wantMax:     "5",
		},
		{
			name:        "max changed triggers update",
			existing:    map[string]string{DesiredReplicasAnnotation: "3", MaxReplicasAnnotation: "4"},
			desired:     3,
			max:         5,
			wantUpdated: true,
			wantDesired: "3",
			wantMax:     "5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newRS(tt.existing)
			got := SetReplicasAnnotations(rs, tt.desired, tt.max)
			if got != tt.wantUpdated {
				t.Errorf("updated = %v, want %v", got, tt.wantUpdated)
			}
			if rs.Annotations[DesiredReplicasAnnotation] != tt.wantDesired {
				t.Errorf("desired annotation = %q, want %q", rs.Annotations[DesiredReplicasAnnotation], tt.wantDesired)
			}
			if rs.Annotations[MaxReplicasAnnotation] != tt.wantMax {
				t.Errorf("max annotation = %q, want %q", rs.Annotations[MaxReplicasAnnotation], tt.wantMax)
			}
		})
	}
}

func TestReplicasAnnotationsNeedUpdate(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		desired  int32
		max      int32
		want     bool
	}{
		{
			name:     "nil annotations need update",
			existing: nil,
			desired:  3,
			max:      5,
			want:     true,
		},
		{
			name:     "matching annotations no update needed",
			existing: map[string]string{DesiredReplicasAnnotation: "3", MaxReplicasAnnotation: "5"},
			desired:  3,
			max:      5,
			want:     false,
		},
		{
			name:     "mismatched desired needs update",
			existing: map[string]string{DesiredReplicasAnnotation: "2", MaxReplicasAnnotation: "5"},
			desired:  3,
			max:      5,
			want:     true,
		},
		{
			name:     "mismatched max needs update",
			existing: map[string]string{DesiredReplicasAnnotation: "3", MaxReplicasAnnotation: "10"},
			desired:  3,
			max:      5,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newRS(tt.existing)
			got := ReplicasAnnotationsNeedUpdate(rs, tt.desired, tt.max)
			if got != tt.want {
				t.Errorf("needsUpdate = %v, want %v", got, tt.want)
			}
		})
	}
}
