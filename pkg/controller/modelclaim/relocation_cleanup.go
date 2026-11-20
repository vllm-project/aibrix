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

package modelclaim

import (
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	relocationCheckInterval = 30 * time.Second
	relocationMaxBackoff    = 5 * time.Minute
	relocationCleanupBudget = 2 * time.Second
)

// Cancel the housekeeping call when its budget expires. The runtime client
// treats cancellation as the caller giving up, without marking the whole
// runtime silent for placement and health checks.
func relocationCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	timer := time.AfterFunc(relocationCleanupBudget, cancel)
	return ctx, func() {
		timer.Stop()
		cancel()
	}
}

type relocationEngine struct {
	claim ModelClaimRef
	model string
	port  int32
}

type relocationRetry struct {
	attempts int
	nextTry  time.Time
}

type relocationObservation struct {
	at      time.Time
	podUID  types.UID
	podIP   string
	engines map[relocationEngine]relocationRetry
}

func (o relocationObservation) matches(pod *corev1.Pod) bool {
	return o.podUID == pod.UID && o.podIP == pod.Status.PodIP
}

type relocationProbe struct {
	at      time.Time
	podName string
}

type relocationChecks struct {
	pods      map[types.NamespacedName]relocationObservation
	probes    map[string]relocationProbe
	nextPrune time.Time
}

func (s *relocationChecks) prune(now time.Time) {
	if now.Before(s.nextPrune) {
		return
	}
	for key, observation := range s.pods {
		if now.Sub(observation.at) > 2*relocationMaxBackoff {
			delete(s.pods, key)
		}
	}
	for namespace, probe := range s.probes {
		if now.Sub(probe.at) > 2*relocationMaxBackoff {
			delete(s.probes, namespace)
		}
	}
	s.nextPrune = now.Add(relocationCheckInterval)
}

type relocationCandidate struct {
	pod    *corev1.Pod
	engine relocationEngine
	retry  relocationRetry
}

func (r *ModelClaimReconciler) reconcileRelocationCleanup(
	ctx context.Context,
	namespace string,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) {
	now := r.now()
	checks := &r.relocationChecks
	if checks.pods == nil {
		checks.pods = make(map[types.NamespacedName]relocationObservation)
		checks.probes = make(map[string]relocationProbe)
	}
	checks.prune(now)

	due, snapshots := r.relocationReadings(ctx, namespace, candidates, readings, now)
	if len(due) == 0 {
		return
	}

	// Cached claims only nominate candidates. The one engine selected for the
	// stop budget below is checked against the API server before any mutation.
	claims := &modelv1alpha1.ModelClaimList{}
	for _, snapshot := range snapshots {
		if len(snapshot.models()) == 0 {
			continue
		}
		if err := r.List(ctx, claims, client.InNamespace(namespace)); err != nil {
			klog.ErrorS(err, "list cached claims for relocation cleanup", "namespace", namespace)
			r.resetRelocationObservations(due, now)
			return
		}
		break
	}
	ledgers := podLedgersFrom(claims, nil, due, snapshots, nil)
	var selected relocationCandidate
	for i := range due {
		pod := &due[i]
		key := client.ObjectKeyFromObject(pod)
		previous := checks.pods[key]
		current := relocationObservation{at: now, podUID: pod.UID, podIP: pod.Status.PodIP}
		for _, model := range ledgers[pod.Name].unclaimed {
			if model.ClaimRef == nil || model.ClaimRef.UID == "" {
				recordRelocationSkip(pod, model.ModelName, "missing_claim_ref")
				continue
			}
			owner := claimForRuntimeSnapshot(claims, model)
			if !relocatedFrom(owner, pod.Name, model.ModelName, claims) {
				continue
			}
			engine := relocationEngine{claim: *model.ClaimRef, model: model.ModelName, port: model.Port}
			var retry relocationRetry
			confirmed := false
			if previous.matches(pod) {
				retry, confirmed = previous.engines[engine]
			}
			if current.engines == nil {
				current.engines = make(map[relocationEngine]relocationRetry)
			}
			current.engines[engine] = retry
			// Prefer engines that have never been tried, then the oldest due
			// retry, so a stuck stop cannot starve other Pods.
			if confirmed && !now.Before(retry.nextTry) &&
				(selected.pod == nil || retry.nextTry.Before(selected.retry.nextTry)) {
				selected = relocationCandidate{pod: pod, engine: engine, retry: retry}
			}
		}
		checks.pods[key] = current
	}

	if selected.pod == nil {
		return
	}
	stopCtx, cancel := relocationCleanupContext(ctx)
	defer cancel()
	observation := checks.pods[client.ObjectKeyFromObject(selected.pod)]
	if r.tryRelocationStop(stopCtx, selected, readings) {
		retry := selected.retry
		retry.attempts++
		delay := relocationCheckInterval << min(retry.attempts-1, 4)
		retry.nextTry = r.now().Add(min(wait.Jitter(delay, 0.1), relocationMaxBackoff))
		observation.engines[selected.engine] = retry
	} else {
		// A failed preflight breaks the chain of observations for this engine.
		delete(observation.engines, selected.engine)
	}
}

func (r *ModelClaimReconciler) resetRelocationObservations(pods []corev1.Pod, now time.Time) {
	for i := range pods {
		pod := &pods[i]
		r.relocationChecks.pods[client.ObjectKeyFromObject(pod)] = relocationObservation{
			at: now, podUID: pod.UID, podIP: pod.Status.PodIP,
		}
	}
}

// Reuse snapshots taken by placement, policy, and health reconciliation.
// Rotate a bounded supplementary probe through the namespace so a source
// without any assigned claims can still be found after a restart.
func (r *ModelClaimReconciler) relocationReadings(
	ctx context.Context, namespace string, candidates []corev1.Pod, readings *runtimeReadings, now time.Time,
) ([]corev1.Pod, map[string]*RuntimeSnapshot) {
	checks := &r.relocationChecks
	probe := checks.probes[namespace]
	allowProbe := now.Sub(probe.at) >= relocationCheckInterval
	var due []corev1.Pod
	snapshots := make(map[string]*RuntimeSnapshot)
	var pick *corev1.Pod
	for i := range candidates {
		pod := &candidates[i]
		if pod.UID == "" {
			continue
		}
		previous := checks.pods[client.ObjectKeyFromObject(pod)]
		if previous.matches(pod) && now.Sub(previous.at) < relocationCheckInterval {
			continue
		}
		if reading, ok := readings.byPod[pod.Name]; ok {
			due = append(due, *pod)
			if reading.err == nil {
				snapshots[pod.Name] = reading.snapshot
			}
		} else if allowProbe && (pick == nil || nextRelocationProbe(pod.Name, pick.Name, probe.podName)) {
			pick = pod
		}
	}
	if pick != nil {
		probeCtx, cancel := relocationCleanupContext(ctx)
		snapshot, err := readings.of(probeCtx, pick)
		cancel()
		checks.probes[namespace] = relocationProbe{at: now, podName: pick.Name}
		due = append(due, *pick)
		if err == nil {
			snapshots[pick.Name] = snapshot
		}
	}
	return due, snapshots
}

// Start after the last probe and wrap around at the end of the pool.
func nextRelocationProbe(candidate, current, last string) bool {
	if (candidate > last) != (current > last) {
		return candidate > last
	}
	return candidate < current
}

// Require a live owner, including a claim that lost its replacement Pod.
// Protect any same-name instance recorded here, even if the runtime
// attributes it to a different Claim UID.
func relocatedFrom(owner *modelv1alpha1.ModelClaim, podName, modelName string, claims *modelv1alpha1.ModelClaimList) bool {
	if owner == nil || claims == nil || !owner.DeletionTimestamp.IsZero() || servedModelName(owner) != modelName {
		return false
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if servedModelName(claim) == modelName && slices.ContainsFunc(claim.Status.Instances, func(instance modelv1alpha1.ModelClaimInstance) bool {
			return instance.Pod == podName
		}) {
			return false
		}
	}
	return true
}

func (r *ModelClaimReconciler) tryRelocationStop(ctx context.Context, candidate relocationCandidate, readings *runtimeReadings) bool {
	pod, engine := candidate.pod, candidate.engine
	claims, err := r.listClaimsForAccount(ctx, pod.Namespace)
	if err != nil {
		recordRelocationSkip(pod, engine.model, "claim_list_failed")
		return false
	}
	owner := claimForRuntimeSnapshot(claims, RuntimeSnapshotModel{ClaimRef: &engine.claim})
	if !relocatedFrom(owner, pod.Name, engine.model, claims) {
		recordRelocationSkip(pod, engine.model, "claim_changed")
		return false
	}

	snapshot, err := r.Runtime.Snapshot(ctx, pod.Status.PodIP, DefaultRuntimePort)
	if err != nil {
		recordRelocationSkip(pod, engine.model, "snapshot_failed")
		return false
	}
	if !slices.ContainsFunc(snapshot.models(), func(model RuntimeSnapshotModel) bool {
		return model.ClaimRef != nil && *model.ClaimRef == engine.claim &&
			model.ModelName == engine.model && model.Port == engine.port &&
			model.Phase != runtimePhaseStopping && (model.Phase != runtimePhaseFailed || model.Alive)
	}) {
		recordRelocationSkip(pod, engine.model, "engine_changed")
		return false
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	current := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil ||
		current.UID != pod.UID || current.Status.PodIP != pod.Status.PodIP ||
		current.Status.Phase != corev1.PodRunning ||
		current.Labels[constants.ModelPoolLabelEnabled] != constants.ModelPoolLabelEnabledValue ||
		!current.DeletionTimestamp.IsZero() {
		recordRelocationSkip(pod, engine.model, "pod_unverified")
		return false
	}

	routeKey := constants.ModelClaimPodAnnotationPrefix + owner.Name
	wakeKey := constants.ModelClaimWakeAnnotationPrefix + owner.Name
	if _, routed := current.Annotations[routeKey]; routed || wakeAsked(current, owner.Name) {
		patch := client.MergeFromWithOptions(current.DeepCopy(), client.MergeFromWithOptimisticLock{})
		delete(current.Annotations, routeKey)
		delete(current.Annotations, wakeKey)
		if err := r.Patch(ctx, current, patch); err != nil {
			klog.ErrorS(err, "remove the relocated model's old route", "pod", klog.KObj(current), "model", engine.model)
			recordRelocationSkip(pod, engine.model, "route_patch_failed")
			return false
		}
	}

	// The single ModelClaim worker serializes cleanup and placement. An
	// instance is recorded before any managed start can reuse this name.
	attempt := candidate.retry.attempts + 1
	klog.InfoS("requesting stop for unclaimed model engine", "pod", klog.KObj(current),
		"claim", klog.KObj(owner), "model", engine.model, "port", engine.port, "attempt", attempt)
	err = r.Runtime.Deactivate(ctx, current.Status.PodIP, DefaultRuntimePort, &DeactivateRequest{
		ModelName: engine.model,
		Mode:      DeactivateStop,
	})
	readings.forget(current.Name)
	if err != nil {
		relocationCleanupTotal.WithLabelValues("failed", "stop_failed").Inc()
		klog.ErrorS(err, "failed to stop relocated model on its old pod", "pod", klog.KObj(current),
			"claim", klog.KObj(owner), "model", engine.model, "attempt", attempt)
		r.Recorder.Eventf(owner, corev1.EventTypeWarning, "RelocatedEngineStopFailed",
			"could not stop the old engine on pod %s (attempt %d): %v", current.Name, attempt, err)
	} else {
		relocationCleanupTotal.WithLabelValues("requested", "stop_accepted").Inc()
		r.Recorder.Eventf(owner, corev1.EventTypeNormal, "RelocatedEngineStopRequested",
			"requested a stop for the old engine on pod %s (attempt %d)", current.Name, attempt)
	}
	return true
}

func recordRelocationSkip(pod *corev1.Pod, modelName, reason string) {
	relocationCleanupTotal.WithLabelValues("skipped", reason).Inc()
	klog.V(2).InfoS("skipped relocation cleanup", "pod", klog.KObj(pod), "model", modelName, "reason", reason)
}
