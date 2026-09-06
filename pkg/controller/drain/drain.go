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

package drain

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
)

const (
	EventStarted      = "PodDrainStarted"
	EventCompleted    = "PodDrainCompleted"
	EventCancelled    = "PodDrainCancelled"
	EventStateInvalid = "PodDrainStateInvalid"

	drainingAnnotationValue = "true"
)

type Result struct {
	Changed      bool
	RequeueAfter time.Duration
}

func (r *Result) Merge(other Result) {
	r.Changed = r.Changed || other.Changed
	if other.RequeueAfter <= 0 {
		return
	}
	if r.RequeueAfter <= 0 || other.RequeueAfter < r.RequeueAfter {
		r.RequeueAfter = other.RequeueAfter
	}
}

func DeletePods(ctx context.Context, cli client.Client, recorder record.EventRecorder, owner runtime.Object, pods []*corev1.Pod, spec *orchestrationv1alpha1.RoleDrainSpec, reason string, now time.Time) (Result, error) {
	if len(pods) == 0 {
		return Result{}, nil
	}
	if drainTimeout(spec) <= 0 {
		return deletePodsImmediately(ctx, cli, pods)
	}

	var result Result
	timeout := drainTimeout(spec)
	for _, pod := range pods {
		podResult, err := processPod(ctx, cli, recorder, owner, pod, reason, timeout, now)
		if err != nil {
			return result, err
		}
		result.Merge(podResult)
	}
	return result, nil
}

func drainTimeout(spec *orchestrationv1alpha1.RoleDrainSpec) time.Duration {
	if spec == nil || spec.TimeoutSeconds == nil || *spec.TimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(*spec.TimeoutSeconds) * time.Second
}

func deletePodsImmediately(ctx context.Context, cli client.Client, pods []*corev1.Pod) (Result, error) {
	var result Result
	for _, pod := range pods {
		if pod == nil || pod.DeletionTimestamp != nil {
			continue
		}
		if err := cli.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return result, err
		}
		result.Changed = true
	}
	return result, nil
}

func CancelPods(ctx context.Context, cli client.Client, recorder record.EventRecorder, owner runtime.Object, pods []*corev1.Pod, reason string) (Result, error) {
	var result Result
	for _, pod := range pods {
		if pod == nil || pod.DeletionTimestamp != nil {
			continue
		}
		annotations := pod.GetAnnotations()
		if annotations[aibrixconst.PodDrainingAnnotationKey] != drainingAnnotationValue {
			continue
		}
		if reason != "" && annotations[aibrixconst.PodDrainReasonAnnotationKey] != reason {
			continue
		}
		before := pod.DeepCopy()
		next := copyStringMap(annotations)
		delete(next, aibrixconst.PodDrainingAnnotationKey)
		delete(next, aibrixconst.PodDrainStartTimeAnnotationKey)
		delete(next, aibrixconst.PodDrainReasonAnnotationKey)
		delete(next, aibrixconst.PodDrainTargetActionAnnotationKey)
		pod.SetAnnotations(next)
		if err := cli.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return result, err
		}
		recordEvent(recorder, owner, corev1.EventTypeNormal, EventCancelled, "cancelled drain for pod %s", pod.Name)
		result.Changed = true
	}
	return result, nil
}

func processPod(ctx context.Context, cli client.Client, recorder record.EventRecorder, owner runtime.Object, pod *corev1.Pod, reason string, timeout time.Duration, now time.Time) (Result, error) {
	if pod == nil || pod.DeletionTimestamp != nil {
		return Result{}, nil
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return deletePodsImmediately(ctx, cli, []*corev1.Pod{pod})
	}
	annotations := pod.GetAnnotations()
	if annotations[aibrixconst.PodDrainingAnnotationKey] != drainingAnnotationValue {
		return startDrain(ctx, cli, recorder, owner, pod, reason, timeout, now, false, "")
	}

	startTime, valid, invalidReason := validateDrainState(annotations, now)
	if !valid {
		return startDrain(ctx, cli, recorder, owner, pod, reason, timeout, now, true, invalidReason)
	}

	elapsed := now.Sub(startTime)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed >= timeout {
		if err := cli.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return Result{}, err
		}
		recordEvent(recorder, owner, corev1.EventTypeNormal, EventCompleted, "drain completed for pod %s", pod.Name)
		return Result{Changed: true}, nil
	}
	remaining := timeout - elapsed
	klog.V(4).InfoS("Waiting for pod drain timeout before deletion",
		"namespace", pod.Namespace,
		"pod", pod.Name,
		"owner", ownerName(owner),
		"reason", annotations[aibrixconst.PodDrainReasonAnnotationKey],
		"targetAction", annotations[aibrixconst.PodDrainTargetActionAnnotationKey],
		"startTime", startTime.UTC().Format(time.RFC3339),
		"timeout", timeout,
		"remaining", remaining,
	)
	return Result{RequeueAfter: remaining}, nil
}

func validateDrainState(annotations map[string]string, now time.Time) (time.Time, bool, string) {
	if annotations[aibrixconst.PodDrainTargetActionAnnotationKey] != aibrixconst.PodDrainTargetActionDelete {
		return time.Time{}, false, "missing or unexpected drain target action"
	}
	rawStart := annotations[aibrixconst.PodDrainStartTimeAnnotationKey]
	if rawStart == "" {
		return time.Time{}, false, "missing drain start time"
	}
	startTime, err := time.Parse(time.RFC3339, rawStart)
	if err != nil {
		return time.Time{}, false, fmt.Sprintf("malformed drain start time %q", rawStart)
	}
	return startTime, true, ""
}

func startDrain(ctx context.Context, cli client.Client, recorder record.EventRecorder, owner runtime.Object, pod *corev1.Pod, reason string, timeout time.Duration, now time.Time, repaired bool, repairReason string) (Result, error) {
	before := pod.DeepCopy()
	annotations := pod.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	} else {
		annotations = copyStringMap(annotations)
	}
	annotations[aibrixconst.PodDrainingAnnotationKey] = drainingAnnotationValue
	annotations[aibrixconst.PodDrainStartTimeAnnotationKey] = now.UTC().Format(time.RFC3339)
	annotations[aibrixconst.PodDrainReasonAnnotationKey] = reason
	annotations[aibrixconst.PodDrainTargetActionAnnotationKey] = aibrixconst.PodDrainTargetActionDelete
	pod.SetAnnotations(annotations)
	if err := cli.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
		if apierrors.IsNotFound(err) {
			return Result{}, nil
		}
		return Result{}, err
	}
	if repaired {
		recordEvent(recorder, owner, corev1.EventTypeWarning, EventStateInvalid, "reset invalid drain state for pod %s: %s", pod.Name, repairReason)
	} else {
		recordEvent(recorder, owner, corev1.EventTypeNormal, EventStarted, "started draining pod %s before deletion", pod.Name)
	}
	return Result{Changed: true, RequeueAfter: timeout}, nil
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func recordEvent(recorder record.EventRecorder, owner runtime.Object, eventType, reason, messageFmt string, args ...interface{}) {
	if recorder == nil || owner == nil {
		return
	}
	recorder.Eventf(owner, eventType, reason, messageFmt, args...)
}

func ownerName(owner runtime.Object) string {
	obj, ok := owner.(client.Object)
	if !ok || obj == nil {
		return ""
	}
	return obj.GetName()
}
