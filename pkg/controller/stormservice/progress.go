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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

const (
	defaultProgressDeadlineSeconds int32 = 600

	ProgressingReason              = "Processing"
	ProgressDeadlineExceededReason = "ProgressDeadlineExceeded"
	PausedReason                   = "DeploymentPaused"
	ResumedReason                  = "DeploymentResumed"
)

func newProgressingCondition(status corev1.ConditionStatus, reason, message string, now time.Time) orchestrationv1alpha1.Condition {
	timestamp := metav1.NewTime(now)
	return orchestrationv1alpha1.Condition{
		Type:               orchestrationv1alpha1.StormServiceProgressing,
		Status:             status,
		LastUpdateTime:     &timestamp,
		LastTransitionTime: &timestamp,
		Reason:             reason,
		Message:            message,
	}
}

func stormServiceProgressing(oldStatus, newStatus *orchestrationv1alpha1.StormServiceStatus) bool {
	oldReplicas := oldStatus.Replicas - oldStatus.UpdatedReplicas
	newReplicas := newStatus.Replicas - newStatus.UpdatedReplicas
	return newStatus.UpdatedReplicas > oldStatus.UpdatedReplicas ||
		newReplicas < oldReplicas ||
		newStatus.ReadyReplicas > oldStatus.ReadyReplicas ||
		newStatus.UpdatedReadyReplicas > oldStatus.UpdatedReadyReplicas
}

func progressDeadline(stormService *orchestrationv1alpha1.StormService) time.Duration {
	seconds := defaultProgressDeadlineSeconds
	if stormService.Spec.ProgressDeadlineSeconds != nil {
		seconds = *stormService.Spec.ProgressDeadlineSeconds
	}
	return time.Duration(seconds) * time.Second
}

func syncStormServiceProgressingCondition(stormService *orchestrationv1alpha1.StormService, oldStatus *orchestrationv1alpha1.StormServiceStatus, now time.Time) {
	currentCondition := utils.GetCondition(oldStatus.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	var condition orchestrationv1alpha1.Condition

	switch {
	case stormService.Spec.Paused:
		if currentCondition != nil && currentCondition.Reason == ProgressDeadlineExceededReason {
			condition = *currentCondition
		} else if currentCondition != nil && currentCondition.Reason == PausedReason {
			condition = *currentCondition
		} else {
			condition = newProgressingCondition(
				corev1.ConditionUnknown,
				PausedReason,
				fmt.Sprintf("StormService %q is paused.", stormService.Name),
				now,
			)
		}
	case currentCondition != nil && currentCondition.Reason == PausedReason:
		condition = newProgressingCondition(
			corev1.ConditionUnknown,
			ResumedReason,
			fmt.Sprintf("StormService %q is resumed.", stormService.Name),
			now,
		)
		condition.LastTransitionTime = currentCondition.LastTransitionTime
	case stormServiceProgressing(oldStatus, &stormService.Status):
		condition = newProgressingCondition(
			corev1.ConditionTrue,
			ProgressingReason,
			fmt.Sprintf("StormService %q is progressing.", stormService.Name),
			now,
		)
		if currentCondition != nil && currentCondition.Status == corev1.ConditionTrue {
			condition.LastTransitionTime = currentCondition.LastTransitionTime
		}
	case currentCondition == nil || currentCondition.LastUpdateTime == nil:
		condition = newProgressingCondition(
			corev1.ConditionTrue,
			ProgressingReason,
			fmt.Sprintf("StormService %q is progressing.", stormService.Name),
			now,
		)
	case currentCondition.Reason == ProgressDeadlineExceededReason:
		condition = *currentCondition
	case !currentCondition.LastUpdateTime.Add(progressDeadline(stormService)).After(now):
		condition = newProgressingCondition(
			corev1.ConditionFalse,
			ProgressDeadlineExceededReason,
			fmt.Sprintf("StormService %q has timed out progressing.", stormService.Name),
			now,
		)
	default:
		condition = *currentCondition
	}

	stormService.Status.Conditions = append(
		utils.FilterOutCondition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing),
		condition,
	)
}

func progressDeadlineRequeueAfter(stormService *orchestrationv1alpha1.StormService, now time.Time) time.Duration {
	if stormService.Spec.Paused {
		return DefaultRequeueAfter
	}
	condition := utils.GetCondition(stormService.Status.Conditions, orchestrationv1alpha1.StormServiceProgressing)
	if condition == nil || condition.LastUpdateTime == nil || condition.Reason == ProgressDeadlineExceededReason {
		return DefaultRequeueAfter
	}

	requeueAfter := condition.LastUpdateTime.Add(progressDeadline(stormService)).Sub(now) + time.Second
	if requeueAfter < time.Second {
		return time.Second
	}
	if requeueAfter < DefaultRequeueAfter {
		return requeueAfter
	}
	return DefaultRequeueAfter
}
