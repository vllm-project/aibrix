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

package stormservice

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

func progressingCondition(status corev1.ConditionStatus, reason string, updated, transitioned time.Time) orchestrationv1alpha1.Condition {
	lastUpdate := metav1.NewTime(updated)
	lastTransition := metav1.NewTime(transitioned)
	return orchestrationv1alpha1.Condition{
		Type:               orchestrationv1alpha1.StormServiceProgressing,
		Status:             status,
		Reason:             reason,
		LastUpdateTime:     &lastUpdate,
		LastTransitionTime: &lastTransition,
	}
}

func TestSyncStormServiceProgressingConditionTimesOutStalledRollout(t *testing.T) {
	started := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	now := started.Add(11 * time.Second)
	oldStatus := orchestrationv1alpha1.StormServiceStatus{
		Replicas:             2,
		UpdatedReplicas:      1,
		ReadyReplicas:        1,
		UpdatedReadyReplicas: 1,
		Conditions: orchestrationv1alpha1.Conditions{
			progressingCondition(corev1.ConditionTrue, ProgressingReason, started, started),
		},
	}
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: "stalled"},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			ProgressDeadlineSeconds: ptr.To[int32](10),
		},
		Status: *oldStatus.DeepCopy(),
	}

	syncStormServiceProgressingCondition(stormService, &oldStatus, now)

	condition := utils.GetCondition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionFalse, condition.Status)
	assert.Equal(t, ProgressDeadlineExceededReason, condition.Reason)
	assert.Equal(t, now, condition.LastUpdateTime.Time)
	assert.Equal(t, now, condition.LastTransitionTime.Time)
}

func TestSyncStormServiceProgressingConditionRefreshesOnlyOnProgress(t *testing.T) {
	started := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	now := started.Add(9 * time.Second)
	oldStatus := orchestrationv1alpha1.StormServiceStatus{
		Replicas:             2,
		UpdatedReplicas:      1,
		ReadyReplicas:        1,
		UpdatedReadyReplicas: 1,
		Conditions: orchestrationv1alpha1.Conditions{
			progressingCondition(corev1.ConditionTrue, ProgressingReason, started, started.Add(-time.Minute)),
		},
	}
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: "progressing"},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			ProgressDeadlineSeconds: ptr.To[int32](10),
		},
		Status: *oldStatus.DeepCopy(),
	}
	stormService.Status.UpdatedReplicas = 2

	syncStormServiceProgressingCondition(stormService, &oldStatus, now)

	condition := utils.GetCondition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Equal(t, ProgressingReason, condition.Reason)
	assert.Equal(t, now, condition.LastUpdateTime.Time)
	assert.Equal(t, started.Add(-time.Minute), condition.LastTransitionTime.Time)
}

func TestSyncStormServiceProgressingConditionDoesNotRefreshWithoutProgress(t *testing.T) {
	started := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	transitioned := started.Add(-time.Minute)
	oldStatus := orchestrationv1alpha1.StormServiceStatus{
		Replicas:             2,
		UpdatedReplicas:      1,
		ReadyReplicas:        1,
		UpdatedReadyReplicas: 1,
		Conditions: orchestrationv1alpha1.Conditions{
			progressingCondition(corev1.ConditionTrue, ProgressingReason, started, transitioned),
		},
	}
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: "stalled-within-deadline"},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			ProgressDeadlineSeconds: ptr.To[int32](10),
		},
		Status: *oldStatus.DeepCopy(),
	}

	syncStormServiceProgressingCondition(stormService, &oldStatus, started.Add(9*time.Second))

	condition := utils.GetCondition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Equal(t, ProgressingReason, condition.Reason)
	assert.Equal(t, started, condition.LastUpdateTime.Time)
	assert.Equal(t, transitioned, condition.LastTransitionTime.Time)
}

func TestSyncStormServiceProgressingConditionExcludesPausedTime(t *testing.T) {
	started := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	oldStatus := orchestrationv1alpha1.StormServiceStatus{
		Conditions: orchestrationv1alpha1.Conditions{
			progressingCondition(corev1.ConditionTrue, ProgressingReason, started, started),
		},
	}
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: "paused"},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Paused:                  true,
			ProgressDeadlineSeconds: ptr.To[int32](10),
		},
		Status: *oldStatus.DeepCopy(),
	}
	pausedAt := started.Add(time.Hour)

	syncStormServiceProgressingCondition(stormService, &oldStatus, pausedAt)
	pausedCondition := utils.GetCondition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	require.NotNil(t, pausedCondition)
	assert.Equal(t, corev1.ConditionUnknown, pausedCondition.Status)
	assert.Equal(t, PausedReason, pausedCondition.Reason)

	pausedStatus := stormService.Status.DeepCopy()
	stormService.Spec.Paused = false
	resumedAt := pausedAt.Add(time.Hour)
	syncStormServiceProgressingCondition(stormService, pausedStatus, resumedAt)
	resumedCondition := utils.GetCondition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	require.NotNil(t, resumedCondition)
	assert.Equal(t, corev1.ConditionUnknown, resumedCondition.Status)
	assert.Equal(t, ResumedReason, resumedCondition.Reason)
	assert.Equal(t, resumedAt, resumedCondition.LastUpdateTime.Time)
	assert.Equal(t, pausedAt, resumedCondition.LastTransitionTime.Time)
}

func TestSyncStormServiceProgressingConditionRecoversAfterProgress(t *testing.T) {
	started := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	now := started.Add(time.Minute)
	oldStatus := orchestrationv1alpha1.StormServiceStatus{
		Replicas:        2,
		UpdatedReplicas: 1,
		Conditions: orchestrationv1alpha1.Conditions{
			progressingCondition(corev1.ConditionFalse, ProgressDeadlineExceededReason, started, started),
		},
	}
	stormService := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: "recovered"},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			ProgressDeadlineSeconds: ptr.To[int32](10),
		},
		Status: *oldStatus.DeepCopy(),
	}
	stormService.Status.UpdatedReplicas = 2

	syncStormServiceProgressingCondition(stormService, &oldStatus, now)

	condition := utils.GetCondition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Equal(t, ProgressingReason, condition.Reason)
	assert.Equal(t, now, condition.LastUpdateTime.Time)
}

func TestProgressDeadlineRequeueAfter(t *testing.T) {
	started := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	stormService := &orchestrationv1alpha1.StormService{
		Spec: orchestrationv1alpha1.StormServiceSpec{
			ProgressDeadlineSeconds: ptr.To[int32](10),
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			Conditions: orchestrationv1alpha1.Conditions{
				progressingCondition(corev1.ConditionTrue, ProgressingReason, started, started),
			},
		},
	}

	assert.Equal(t, 8*time.Second, progressDeadlineRequeueAfter(stormService, started.Add(3*time.Second)))

	stormService.Spec.Paused = true
	assert.Equal(t, DefaultRequeueAfter, progressDeadlineRequeueAfter(stormService, started.Add(3*time.Second)))
}
