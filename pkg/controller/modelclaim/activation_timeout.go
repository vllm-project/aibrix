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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// instanceReasonActivationTimedOut is the reason of an instance that failed
// because its engine did not become routable within activationTimeout.
const instanceReasonActivationTimedOut = "ActivationTimedOut"

// activationTimeout is how long an instance may stay Activating with a live
// engine before the controller fails it.
const activationTimeout = 30 * time.Minute

// stuckActivationForgetAfter is how long a stuck instance may go unseen before
// its clock is dropped. A stuck instance is looked at every pass, so one not
// seen for this long has left its claim, or its claim is gone.
const stuckActivationForgetAfter = 10 * time.Minute

// stuckActivations remembers when the controller first saw each instance
// Activating with a live engine. It lives in memory only, so a restarted
// controller starts every clock over, which can only make a timeout later.
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
// activationTimeout.
//
// The time counts while the engine is alive or ready and not routed: while it
// boots, and after that, as when its KV limit does not take. An engine that
// restarts or stops is left to the runtime.
//
// The time is the shorter of two. The runtime dates the engine's last change
// of phase, such as its start or getting ready, on its own clock, so a
// restarted engine starts over. The controller notes when it first saw the
// instance this way, so an engine that served for hours and only now lost its
// route does not fail at once.
func (r *ModelClaimReconciler) activationTimedOut(
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	snapshot *RuntimeSnapshot,
	observed *RuntimeSnapshotModel,
	state engineState,
) bool {
	key := stuckActivationKey(pm, inst)
	if state.phase != modelv1alpha1.ModelClaimActivating || observed == nil ||
		observed.Phase == runtimePhaseStopping || (!observed.Alive && !observed.Ready) {
		r.stuckActivations().forget(key)
		return false
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
	return stuck >= activationTimeout
}

// failTimedOutInstance fails an instance that stayed Activating past
// activationTimeout. The failure is written at once, before the engine is
// stopped. Stopped first, a pass that ended before the write would leave the
// instance Activating with no engine, and the next pass would start the engine
// again. Written, the failure stands until the instance is replaced, as a move
// does. The route says so at once, so a request goes to another instance.
func (r *ModelClaimReconciler) failTimedOutInstance(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	slot int,
	pod *corev1.Pod,
) error {
	was := pm.Status.Instances[slot]
	pm.Status.Instances[slot].Phase = modelv1alpha1.ModelClaimFailed
	pm.Status.Instances[slot].Reason = instanceReasonActivationTimedOut
	if err := r.Status().Update(ctx, pm); err != nil {
		pm.Status.Instances[slot] = was
		return err
	}
	inst := &pm.Status.Instances[slot]
	r.stuckActivations().forget(stuckActivationKey(pm, inst))
	r.Recorder.Event(pm, corev1.EventTypeWarning, instanceReasonActivationTimedOut,
		activationTimeoutMessage(pm, inst))
	if err := r.annotateWarmPodWithState(ctx, pm, pod, 0, constants.ModelClaimRoutingStateFailed, ""); err != nil {
		klog.ErrorS(err, "could not say on the route that an instance timed out", "pod", klog.KObj(pod), "model", pm.Name)
	}
	return nil
}

// stopTimedOutEngine asks the runtime to stop the engine of an instance that
// timed out. A runtime that takes the stop finishes it on its own. A stop it
// did not take is asked again on the next pass.
func (r *ModelClaimReconciler) stopTimedOutEngine(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
	readings *runtimeReadings,
) {
	err := r.Runtime.Deactivate(ctx, pod.Status.PodIP, DefaultRuntimePort, &DeactivateRequest{
		ModelName: servedModelName(pm),
		Mode:      DeactivateStop,
	})
	readings.forget(pod.Name)
	if err != nil {
		klog.ErrorS(err, "could not stop the engine of an instance that timed out", "model", pm.Name, "pod", inst.Pod)
	}
}

// engineToStop reports whether a runtime reading shows an engine that no stop
// has reached. A stopping engine is stopped by the runtime. One that failed
// and is no longer alive has given its memory back for good.
func engineToStop(observed *RuntimeSnapshotModel) bool {
	if observed == nil || observed.Phase == runtimePhaseStopping {
		return false
	}
	return observed.Phase != runtimePhaseFailed || observed.Alive
}

// replaceableFailedSlots returns the positions of the failed instances that
// can be replaced now, in order. An instance that timed out waits until the
// runtime has taken the stop of its engine. Once a replacement takes its place,
// nothing would stop that engine, and it would keep its memory.
func (r *ModelClaimReconciler) replaceableFailedSlots(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	readings *runtimeReadings,
) []int {
	var slots []int
	for _, slot := range failedInstanceSlots(pm) {
		inst := &pm.Status.Instances[slot]
		if inst.Reason == instanceReasonActivationTimedOut && r.timedOutEngineRuns(ctx, pm, inst, readings) {
			continue
		}
		slots = append(slots, slot)
	}
	return slots
}

// timedOutEngineRuns reports whether the runtime of an instance that timed out
// still shows an engine that no stop has reached. A Pod that is gone, or a
// runtime that cannot be read, shows nothing, and the instance is replaced as
// any failed one is.
func (r *ModelClaimReconciler) timedOutEngineRuns(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	readings *runtimeReadings,
) bool {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: inst.Pod}, pod); err != nil ||
		pod.Status.PodIP == "" {
		return false
	}
	snapshot, err := readings.of(ctx, pod)
	if err != nil {
		return false
	}
	return engineToStop(snapshotModelForClaim(snapshot, pm, servedModelName(pm)))
}

// activationTimeoutMessage says why an instance failed on activationTimeout.
func activationTimeoutMessage(pm *modelv1alpha1.ModelClaim, inst *modelv1alpha1.ModelClaimInstance) string {
	return fmt.Sprintf("model %s on pod %s did not become routable within %s; stopping its engine",
		servedModelName(pm), inst.Pod, activationTimeout)
}
