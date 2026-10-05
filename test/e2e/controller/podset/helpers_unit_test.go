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

package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
)

func TestObserveConsistentlyUsesParentContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0

	err := observeConsistently(ctx, time.Millisecond, 10*time.Millisecond, func(observeCtx context.Context) error {
		calls++
		if err := observeCtx.Err(); err != nil {
			t.Fatalf("observer received expired context: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("observeConsistently() error = %v", err)
	}
	if calls < 2 {
		t.Fatalf("observer calls = %d, want at least 2", calls)
	}
}

func TestNewPodSet(t *testing.T) {
	podSet := newPodSet("test-namespace", "test-podset", 3, 5)

	if podSet.APIVersion != "orchestration.aibrix.ai/v1alpha1" || podSet.Kind != "PodSet" {
		t.Fatalf("PodSet GVK = %s %s, want orchestration.aibrix.ai/v1alpha1 PodSet", podSet.APIVersion, podSet.Kind)
	}
	if podSet.Namespace != "test-namespace" || podSet.Name != "test-podset" {
		t.Fatalf("PodSet key = %s/%s, want test-namespace/test-podset", podSet.Namespace, podSet.Name)
	}
	if podSet.Spec.PodGroupSize != 3 {
		t.Fatalf("PodGroupSize = %d, want 3", podSet.Spec.PodGroupSize)
	}
	if podSet.Spec.Drain == nil || podSet.Spec.Drain.TimeoutSeconds == nil || *podSet.Spec.Drain.TimeoutSeconds != 5 {
		t.Fatalf("drain timeout = %#v, want 5", podSet.Spec.Drain)
	}
	if got := podSet.Labels[podSetFixtureLabel]; got != "test-podset" {
		t.Fatalf("PodSet fixture label = %q, want test-podset", got)
	}
	if got := podSet.Spec.Template.Labels[podSetFixtureLabel]; got != "test-podset" {
		t.Fatalf("Pod template fixture label = %q, want test-podset", got)
	}
	if podSet.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Fatalf("restart policy = %q, want %q", podSet.Spec.Template.Spec.RestartPolicy, corev1.RestartPolicyAlways)
	}
	if len(podSet.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("container count = %d, want 1", len(podSet.Spec.Template.Spec.Containers))
	}
	container := podSet.Spec.Template.Spec.Containers[0]
	if container.Image != podSetE2EImage || container.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("container image = %q pull policy = %q, want %q/%q",
			container.Image, container.ImagePullPolicy, podSetE2EImage, corev1.PullIfNotPresent)
	}

	withoutDrain := newPodSet("test-namespace", "without-drain", 2, 0)
	if withoutDrain.Spec.Drain != nil {
		t.Fatalf("drain = %#v, want nil for zero timeout", withoutDrain.Spec.Drain)
	}
}

func TestPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "ready",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}}}},
			want: true,
		},
		{
			name: "not ready",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionFalse,
			}}}},
		},
		{
			name: "unrelated condition",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionTrue,
			}}}},
		},
		{name: "nil pod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podReady(tt.pod); got != tt.want {
				t.Fatalf("podReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodIndex(t *testing.T) {
	tests := []struct {
		name      string
		pod       *corev1.Pod
		wantIndex int
		wantOK    bool
	}{
		{
			name: "valid",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				controllerconstants.PodGroupIndexLabelKey: "2",
			}}},
			wantIndex: 2,
			wantOK:    true,
		},
		{name: "missing", pod: &corev1.Pod{}},
		{
			name: "malformed",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				controllerconstants.PodGroupIndexLabelKey: "two",
			}}},
		},
		{
			name: "negative",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				controllerconstants.PodGroupIndexLabelKey: "-1",
			}}},
		},
		{name: "nil pod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIndex, gotOK := podIndex(tt.pod)
			if gotIndex != tt.wantIndex || gotOK != tt.wantOK {
				t.Fatalf("podIndex() = (%d, %v), want (%d, %v)", gotIndex, gotOK, tt.wantIndex, tt.wantOK)
			}
		})
	}
}

func TestScaleInDrainStarted(t *testing.T) {
	complete := map[string]string{
		aibrixconst.PodDrainingAnnotationKey:          "true",
		aibrixconst.PodDrainStartTimeAnnotationKey:    "2026-09-19T00:00:00Z",
		aibrixconst.PodDrainReasonAnnotationKey:       aibrixconst.PodDrainReasonScaleIn,
		aibrixconst.PodDrainTargetActionAnnotationKey: aibrixconst.PodDrainTargetActionDelete,
	}

	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{name: "complete scale-in drain", annotations: complete, want: true},
		{name: "missing annotations"},
		{name: "draining is false", annotations: annotationsWith(complete, aibrixconst.PodDrainingAnnotationKey, "false")},
		{name: "missing start time", annotations: annotationsWithout(complete, aibrixconst.PodDrainStartTimeAnnotationKey)},
		{name: "missing reason", annotations: annotationsWithout(complete, aibrixconst.PodDrainReasonAnnotationKey)},
		{
			name:        "missing target action",
			annotations: annotationsWithout(complete, aibrixconst.PodDrainTargetActionAnnotationKey),
		},
		{
			name: "wrong reason",
			annotations: map[string]string{
				aibrixconst.PodDrainingAnnotationKey:          "true",
				aibrixconst.PodDrainStartTimeAnnotationKey:    "2026-09-19T00:00:00Z",
				aibrixconst.PodDrainReasonAnnotationKey:       aibrixconst.PodDrainReasonRollout,
				aibrixconst.PodDrainTargetActionAnnotationKey: aibrixconst.PodDrainTargetActionDelete,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}
			if got := scaleInDrainStarted(pod); got != tt.want {
				t.Fatalf("scaleInDrainStarted() = %v, want %v", got, tt.want)
			}
		})
	}
	if scaleInDrainStarted(nil) {
		t.Fatal("scaleInDrainStarted(nil) = true, want false")
	}
}

func TestDrainStartTime(t *testing.T) {
	want := time.Date(2026, time.September, 19, 1, 2, 3, 0, time.UTC)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		aibrixconst.PodDrainStartTimeAnnotationKey: want.Format(time.RFC3339),
	}}}
	got, err := drainStartTime(pod)
	if err != nil {
		t.Fatalf("drainStartTime() error = %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("drainStartTime() = %s, want %s", got, want)
	}

	for name, pod := range map[string]*corev1.Pod{
		"nil pod":       nil,
		"missing value": {},
		"malformed value": {ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			aibrixconst.PodDrainStartTimeAnnotationKey: "not-a-time",
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := drainStartTime(pod); err == nil {
				t.Fatal("drainStartTime() error = nil, want error")
			}
		})
	}
}

func TestPodUIDsByIndex(t *testing.T) {
	pods := []corev1.Pod{
		podWithIndexAndUID("pod-0", "0", "uid-0"),
		podWithIndexAndUID("pod-1", "1", "uid-1"),
	}
	got, err := podUIDsByIndex(pods)
	if err != nil {
		t.Fatalf("podUIDsByIndex() error = %v", err)
	}
	if len(got) != 2 || got[0] != types.UID("uid-0") || got[1] != types.UID("uid-1") {
		t.Fatalf("podUIDsByIndex() = %v, want map[0:uid-0 1:uid-1]", got)
	}

	duplicate := append(pods, podWithIndexAndUID("pod-1-copy", "1", "uid-copy"))
	if _, err := podUIDsByIndex(duplicate); err == nil {
		t.Fatal("podUIDsByIndex() duplicate index error = nil, want error")
	}

	malformed := []corev1.Pod{podWithIndexAndUID("pod-bad", "bad", "uid-bad")}
	if _, err := podUIDsByIndex(malformed); err == nil {
		t.Fatal("podUIDsByIndex() malformed index error = nil, want error")
	}

	missing := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod-missing", UID: "uid-missing"}}}
	if _, err := podUIDsByIndex(missing); err == nil {
		t.Fatal("podUIDsByIndex() missing index error = nil, want error")
	}
}

func annotationsWith(source map[string]string, key, value string) map[string]string {
	result := make(map[string]string, len(source))
	for sourceKey, sourceValue := range source {
		result[sourceKey] = sourceValue
	}
	result[key] = value
	return result
}

func annotationsWithout(source map[string]string, omitted string) map[string]string {
	result := make(map[string]string, len(source)-1)
	for key, value := range source {
		if key != omitted {
			result[key] = value
		}
	}
	return result
}

func podWithIndexAndUID(name, index, uid string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(uid),
			Labels: map[string]string{
				controllerconstants.PodGroupIndexLabelKey: index,
			},
		},
	}
}
