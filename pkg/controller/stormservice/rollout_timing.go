/*
Copyright 2025 The Aibrix Team.

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
	"context"
	"time"

	"k8s.io/klog/v2"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/stormservice/metrics"
)

// RolloutStartedAtAnnotationKey records the RFC3339 timestamp at which a
// StormService most recently transitioned out of the Ready condition. It is
// maintained solely by trackRolloutDuration and, unlike
// Conditions[].LastTransitionTime (which is restamped on every reconcile,
// see NewCondition in pkg/controller/util/orchestration/util.go), is only
// ever written on an actual Ready<->not-Ready transition. Persisting it as
// an annotation on the object (rather than in-memory state) makes rollout
// timing survive a controller restart mid-rollout.
const RolloutStartedAtAnnotationKey = "stormservice.orchestration.aibrix.ai/rollout-started-at"

func rolloutStrategyLabel(stormService *orchestrationv1alpha1.StormService) string {
	strategy := string(stormService.Spec.UpdateStrategy.Type)
	if strategy == "" {
		strategy = string(orchestrationv1alpha1.RollingUpdateStormServiceStrategyType)
	}
	return strategy
}

// trackRolloutDuration maintains the RolloutStartedAtAnnotationKey annotation
// and observes StormServiceRolloutDuration whenever stormServiceReady
// transitions relative to the previously persisted Ready condition captured
// in checkpoint. It is purely additive alongside updateStatus's existing
// condition logic: it never modifies stormService.Status.Conditions.
//
// Because .status is a subresource on StormService, updates to
// stormService.Status made elsewhere in updateStatus are only persisted via
// a Status().Update() call, which the API server does not allow to also
// persist .metadata changes. So annotation changes made here are persisted
// with a separate Client.Update() call, issued only on an actual transition.
func (r *StormServiceReconciler) trackRolloutDuration(ctx context.Context, stormService *orchestrationv1alpha1.StormService, checkpoint *orchestrationv1alpha1.StormServiceStatus, stormServiceReady bool) error {
	wasReady := len(checkpoint.Conditions) > 0 && checkpoint.Conditions[0].Type == orchestrationv1alpha1.StormServiceReady
	if wasReady == stormServiceReady {
		return nil
	}

	namespace, name := stormService.Namespace, stormService.Name

	if stormServiceReady {
		startedAt, ok := stormService.Annotations[RolloutStartedAtAnnotationKey]
		if !ok {
			// No annotation to compute a duration from, e.g. the very first
			// reconcile of a brand-new object. Skip emitting a bogus
			// observation rather than guessing a start time.
			return nil
		}
		if startTime, err := time.Parse(time.RFC3339, startedAt); err != nil {
			klog.Warningf("stormservice %s/%s has invalid %s annotation %q: %v", namespace, name, RolloutStartedAtAnnotationKey, startedAt, err)
		} else {
			metrics.StormServiceRolloutDuration.WithLabelValues(namespace, name, rolloutStrategyLabel(stormService)).Observe(time.Since(startTime).Seconds())
		}
		delete(stormService.Annotations, RolloutStartedAtAnnotationKey)
		return r.Client.Update(ctx, stormService)
	}

	// Ready -> not-Ready
	if stormService.Annotations == nil {
		stormService.Annotations = map[string]string{}
	}
	stormService.Annotations[RolloutStartedAtAnnotationKey] = time.Now().Format(time.RFC3339)
	return r.Client.Update(ctx, stormService)
}
