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
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// instanceReasonActivationTimedOut is the reason of an instance that failed
// because it stayed Activating past its pool's activation timeout after its
// engine had stopped booting.
const instanceReasonActivationTimedOut = "ActivationTimedOut"

// stuckActivationForgetAfter is how long a stuck instance may go unseen before
// its clock is dropped. A stuck instance is looked at every pass, so one not
// seen for this long has left its claim, or its claim is gone.
const stuckActivationForgetAfter = 10 * time.Minute

// stuckActivations remembers when the controller first saw each instance
// stuck: left Activating while its engine no longer boots. It lives in
// memory only, so a restarted controller starts every clock over, which can
// only make a timeout later.
type stuckActivations struct {
	mu   sync.Mutex
	seen map[string]stuckActivation
}

type stuckActivation struct {
	first, last time.Time
}

// observe records that an instance is stuck at now, and returns when it was
// first seen stuck.
func (s *stuckActivations) observe(key string, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	for known, at := range s.seen {
		if now.Sub(at.last) >= stuckActivationForgetAfter {
			delete(s.seen, known)
		}
	}
	if s.seen == nil {
		s.seen = map[string]stuckActivation{}
	}
	at, known := s.seen[key]
	if !known {
		at.first = now
	}
	at.last = now
	s.seen[key] = at
	return at.first
}

// forget drops the clock of an instance that is no longer stuck, so that it
// starts over if the instance gets stuck again.
func (s *stuckActivations) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, key)
}

func (r *ModelClaimReconciler) stuckActivations() *stuckActivations {
	if r.StuckActivations == nil {
		r.StuckActivations = &stuckActivations{}
	}
	return r.StuckActivations
}

// stuckActivationKey names an instance for its clock: its claim, by UID as
// well as by name, and its pod. A claim made again under the same name starts
// a clock of its own.
func stuckActivationKey(pm *modelv1alpha1.ModelClaim, inst *modelv1alpha1.ModelClaimInstance) string {
	return pm.Namespace + "/" + pm.Name + "/" + string(pm.UID) + "/" + inst.Pod
}

// activationTimedOut reports whether an instance has stayed Activating past
// its pool's activation timeout, and that timeout.
//
// The time counts only while the engine no longer boots. It is alive or
// ready, and past the boot window, or ready and still not routable, as when
// its KV limit does not take. An engine that boots, restarts or stops is left
// to the runtime.
//
// The stuck time is the shorter of two. The runtime dates the engine's last
// change of phase on its own clock, so a restarted engine starts over. The
// controller notes when it first saw the instance stuck, so an engine that
// served for hours and only now lost its route does not fail at once.
func (r *ModelClaimReconciler) activationTimedOut(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
	snapshot *RuntimeSnapshot,
	observed *RuntimeSnapshotModel,
	state engineState,
) (bool, time.Duration) {
	key := stuckActivationKey(pm, inst)
	if state.phase != modelv1alpha1.ModelClaimActivating || state.booting || observed == nil ||
		observed.Phase == runtimePhaseStopping || (!observed.Alive && !observed.Ready) {
		r.stuckActivations().forget(key)
		return false, 0
	}
	now := r.now()
	stuck := now.Sub(r.stuckActivations().observe(key, now))
	if observed.LastTransition != nil && !snapshot.ObservedAt.IsZero() {
		// A change dated after the reading comes from a clock that was set
		// back, and says nothing of how long the engine has been stuck.
		if since := snapshot.ObservedAt.Sub(*observed.LastTransition); since >= 0 && since < stuck {
			stuck = since
		}
	}
	timeout := r.activationTimeoutFor(ctx, pod)
	return stuck >= timeout, timeout
}

// stopTimedOutEngine stops the engine of an instance that timed out, and
// reports whether the runtime took the stop. The instance fails only after
// that. A failed instance is replaced on another pod, and its engine is
// stopped only once a pod is found. On the only pod that could hold the claim,
// the engine would otherwise keep its memory for as long as the claim exists.
// A stop that is not taken is asked again on the next pass.
func (r *ModelClaimReconciler) stopTimedOutEngine(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
	timeout time.Duration,
	readings *runtimeReadings,
) bool {
	err := r.Runtime.Deactivate(ctx, pod.Status.PodIP, DefaultRuntimePort, &DeactivateRequest{
		ModelName: servedModelName(pm),
		Mode:      DeactivateStop,
	})
	readings.forget(pod.Name)
	if err != nil {
		klog.ErrorS(err, "could not stop an engine that did not become routable in time",
			"model", pm.Name, "pod", inst.Pod, "timeout", timeout)
		return false
	}
	r.stuckActivations().forget(stuckActivationKey(pm, inst))
	return true
}

// activationTimeoutMessage says why an instance failed on its activation
// timeout.
func activationTimeoutMessage(pm *modelv1alpha1.ModelClaim, inst *modelv1alpha1.ModelClaimInstance, timeout time.Duration) string {
	return fmt.Sprintf("model %s on pod %s did not become routable within %s after its engine stopped booting; "+
		"its engine was stopped", servedModelName(pm), inst.Pod, timeout)
}
