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
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	// The reasons an instance records, when its phase alone does not say why it
	// stands where it does.
	instanceReasonWaitingForRoom = "WaitingForRoom"
	instanceReasonNoRoomToWake   = "NoRoomToWake"
	instanceReasonWakeFailed     = "WakeFailed"

	// wakeRequestLifetime is how long a wake request waits for room before it
	// is taken back. A client still asking writes a new one.
	wakeRequestLifetime = 5 * time.Minute
)

const (
	// The reasons of the Ready condition, and of the route, that say more
	// than a claim's phase: a request waits for room, or the claim moves.
	readyReasonWaitingForRoom = constants.ModelClaimRouteReasonWaitingForRoom
	readyReasonMoving         = constants.ModelClaimRouteReasonMoving
)

// whyMoved words, for the Event of a move, why an instance left its pod.
func whyMoved(reason string) string {
	switch reason {
	case instanceReasonNoRoomToWake:
		return "because its card could not take it back from sleep,"
	case instanceReasonWakeFailed:
		return "because it could not be woken there,"
	}
	return "after terminal engine failure"
}

// movingReason reports whether an instance is marked to be moved, rather than
// failed for good.
func movingReason(reason string) bool {
	return reason == instanceReasonNoRoomToWake || reason == instanceReasonWakeFailed
}

// wakeRequested wakes the sleeping engines of a claim that a request has asked
// for, and takes back a request that has nothing left to do. It returns true
// when it started a wake in this pass, or put a neighbour to sleep to make room
// for one. The claim is then looked at again soon.
//
// The gateway does not wake an engine itself. It writes a wake request on the
// pod whose engine sleeps, and this controller decides. An engine is woken only
// while its card is promised no more than it has, and once its neighbours are
// held to their shares. The request charges the engine its wake reserve again.
// An engine that kept its reserve therefore fits, unless a declaration grew
// while it slept. In a pool that keeps no wake reserve, its room may have gone
// to other models, and the neighbour idle longest may be put to sleep to make
// room. An account that cannot be judged does not stop a wake where the reserve
// was kept, since the engine's room is still there. In a pool that keeps none,
// a wake on a card waits until the account can be judged. A pod without a card
// has no room to give away.
//
// An engine that cannot wake where it is is moved, when another pod can take
// its claim. That is an engine whose card is promised more than it has, and one
// whose runtime reports that it could not wake it. The instance is marked
// failed, with the reason, and the mark is written before anything else is done
// to the engine. The replacement that follows in the same pass then stops the
// engine and places the claim anew. A mark that cannot be written ends the pass,
// and leaves the engine as it was. A card that cannot take the
// engine back, with no other pod to go to, keeps the request waiting. A wake
// that failed with no other pod to go to takes the request back, so the next
// request for the model asks again. A wake whose runtime was not reached, did
// not answer in time, or failed without a report of its own, is asked again on
// a later pass, with the same operation: the engine may be waking already. Such
// a failure can come from a proxy on the way, and says nothing of the engine.
//
// A move for want of room that no other pod has taken yet is called off once
// the engine's card can take it back. The engine then still sleeps there, and
// the mark would otherwise stand until another pod took the claim.
//
// A request stays while the engine boots, and goes once the engine serves, or
// when there is nothing to wake. A request not met within wakeRequestLifetime
// is taken back. A client that still asks writes a new one.
func (r *ModelClaimReconciler) wakeRequested(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) (woke bool, err error) {
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name
	served := servedModelName(pm)
	// The claims are listed for the account once in a pass, the first time an
	// instance needs it, and not again for each instance.
	var claims *modelv1alpha1.ModelClaimList
	var listErr error
	listed := false
	ledgersOf := func(pods []corev1.Pod) map[string]podLedger {
		if !listed {
			claims, listErr = r.listClaimsForAccount(ctx, pm.Namespace)
			listed = true
		}
		// The room held on a card for a new claim is not taken off: a request
		// to wake an engine goes before a claim that has not served yet.
		return podLedgersFrom(claims, listErr, pods, readings.ofPods(ctx, pods), r.podsWithoutWakeReserve(ctx, pods))
	}
	for i := range pm.Status.Instances {
		inst := &pm.Status.Instances[i]
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: inst.Pod}, pod); err != nil {
			continue
		}
		if err := r.callOffMove(ctx, pm, i, pod, ledgersOf, readings); err != nil {
			return woke, err
		}
		requestedAt, asked := pod.Annotations[key]
		if !asked {
			r.setWaitingForRoom(ctx, pm, inst, pod, false)
			continue
		}
		switch inst.Phase {
		case modelv1alpha1.ModelClaimSleeping:
		case modelv1alpha1.ModelClaimActivating:
			// Booting after a wake, or after a restart. The request is done
			// once the engine serves, and until then it keeps the gateway from
			// asking again. One that outlives its lifetime is taken back
			// quietly, since the wake was carried out, so an engine that never
			// finishes booting leaves no request behind.
			if !ensuresAwake(pm) && r.wakeRequestExpired(pod, key, requestedAt) {
				r.takeBackWakeRequest(ctx, pod, key)
			}
			continue
		default:
			r.takeBackWakeRequest(ctx, pod, key)
			continue
		}
		if pod.Status.PodIP == "" {
			continue
		}
		if !ensuresAwake(pm) && r.wakeRequestExpired(pod, key, requestedAt) {
			r.Recorder.Eventf(pm, corev1.EventTypeWarning, "WakeRequestExpired",
				"model %s stays asleep on pod %s: it could not be woken within %s of the request",
				served, pod.Name, wakeRequestLifetime)
			r.takeBackWakeRequest(ctx, pod, key)
			r.setWaitingForRoom(ctx, pm, inst, pod, false)
			continue
		}

		pods := []corev1.Pod{*pod}
		ledger := ledgersOf(pods)[pod.Name]
		if !ledger.judgeable && podHasGPUs(*pod, ledger.accelerators) && ledger.keepsNoWakeReserve {
			r.waitForRoom(ctx, pm, inst, pod, "its card cannot be accounted for: "+ledger.blocked)
			continue
		}
		turn := wakeNow
		if ledger.judgeable {
			turn, ledger = r.wakeTurnOn(ctx, pm, pod, ledger, readings)
		}
		if turn != wakeNow {
			// The card has no room for it. Its neighbours' floors leave none,
			// or they hold KV beyond their floors, which the card lent them
			// while the engine slept. A smaller limit would not make a busy
			// neighbour give that KV back: kvcached keeps a page while any
			// block on it is in use, and an engine keeps what finished
			// requests used as its prefix cache. Only a sleep gives an
			// engine's memory back. So the first request on the card that
			// room can be made for puts the neighbour idle longest to sleep,
			// one a pass, while a sleep gives room back. A neighbour that
			// serves is left alone.
			if turn == wakeMakesRoom && r.sleepToMakeRoom(ctx, pm, pod, ledger, readings) {
				r.waitForRoom(ctx, pm, inst, pod, promisedMoreThanItHas)
				woke = true
				continue
			}
			if canPlaceElsewhere(pm, candidates, ledgersOf) {
				if err := r.markMoving(ctx, pm, i, pod, instanceReasonNoRoomToWake,
					fmt.Sprintf("its card on pod %s is promised more than it has", pod.Name)); err != nil {
					return woke, err
				}
				continue
			}
			r.waitForRoom(ctx, pm, inst, pod, promisedMoreThanItHas)
			continue
		}
		r.setWaitingForRoom(ctx, pm, inst, pod, false)
		if !r.cardArrangedForWake(ctx, pm, pod, ledger, readings) {
			continue
		}

		// One operation per request, so the runtime applies a request once
		// however many passes see it.
		operationID := fmt.Sprintf("controller-wake/%s/%s/%s", pod.UID, pm.UID, requestedAt)
		if request, owned := policyWakeOnPod(pm, pod); owned {
			operationID = request.OperationID
		}
		var resp *RuntimeOperationResponse
		resp, err = r.Runtime.Wake(ctx, pod.Status.PodIP, DefaultRuntimePort, &WakeRequest{
			ModelName:   served,
			OperationID: operationID,
		})
		readings.forget(pod.Name)
		if err != nil && !refusedByRuntime(err) {
			r.notePolicyWakeFailure(pm, inst, pod, err)
			// Nothing is known to have failed. The request stays, and a later
			// pass asks again, or sees the engine wake.
			klog.InfoS("wake not answered; asking again on a later pass",
				"pod", klog.KObj(pod), "model", pm.Name, "err", err)
			continue
		}
		if err != nil {
			if canPlaceElsewhere(pm, candidates, ledgersOf) {
				if err := r.markMoving(ctx, pm, i, pod, instanceReasonWakeFailed,
					fmt.Sprintf("the runtime on pod %s could not wake it: %v", pod.Name, err)); err != nil {
					return woke, err
				}
				continue
			}
			r.Recorder.Eventf(pm, corev1.EventTypeWarning, "WakeFailed",
				"model %s could not be woken on pod %s, and no other pod can take it: %v", served, pod.Name, err)
			if ensuresAwake(pm) {
				inst.Reason = instanceReasonWakeFailed
			}
			r.takeBackWakeRequest(ctx, pod, key)
			continue
		}
		if inst.Reason == instanceReasonWakeFailed {
			inst.Reason = ""
		}
		// A pass that asks again before the engine is seen to wake gets the same
		// operation back, which the runtime does not apply twice. Only the call
		// that woke the engine says so: client-go drops an object's Events once
		// it has raised 25 in a burst.
		if resp == nil || resp.Applied {
			r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Waking",
				"model %s is waking on pod %s, as asked at %s", served, pod.Name, requestedAt)
		}
		woke = true
	}
	return woke, nil
}

// waitForRoom has a sleeping instance wait for room on its card. The wait is
// raised as an Event once, when it starts. The instance and the claim's Ready
// condition say it for as long as it lasts. The claim is looked at every few
// seconds, and client-go drops an object's Events once it has raised 25 in a
// burst, so an Event on every pass would crowd out the ones that follow, such
// as the wake's.
func (r *ModelClaimReconciler) waitForRoom(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
	why string,
) {
	if inst.Reason != instanceReasonWaitingForRoom {
		r.Recorder.Eventf(pm, corev1.EventTypeWarning, "WaitingForRoom",
			"model %s stays asleep on pod %s: %s", servedModelName(pm), pod.Name, why)
	}
	r.setWaitingForRoom(ctx, pm, inst, pod, true)
}

// promisedMoreThanItHas says why an engine waits on a card that cannot take it.
const promisedMoreThanItHas = "its card is promised more than it has"

// wakeTurn is where a request to wake an engine stands on its card.
type wakeTurn int

const (
	// wakeNow is a request the card has room for.
	wakeNow wakeTurn = iota
	// wakeMakesRoom is the first request on the card that room can be made
	// for, by putting idle engines there to sleep.
	wakeMakesRoom
	// wakeWaitsBehind is a request that room can be made for once an earlier
	// one has been made room for.
	wakeWaitsBehind
	// wakeCannotFit is a request that no room can be made for on its card now,
	// even by putting every idle engine there to sleep.
	wakeCannotFit
)

// wakeTurnOn decides where a claim's request to wake its engine stands on its
// card, and returns the account to judge the wake by.
//
// The requests on a card are taken in the order they were asked, as Kueue's
// BestEffortFIFO takes workloads. Each is judged with the reserves of the
// requests before it that the card can meet, and without those of the ones
// asked after it. A request the card has room for wakes. The first one that
// room can be made for, by putting idle engines on the card to sleep, makes
// room, and keeps the room it is owed, so that no later request takes it. Two
// requests that each put a neighbour to sleep for themselves would take one
// card's room twice. A request that no room can be made for does not hold up
// the ones behind it: it is charged only what its engine holds asleep, and it
// waits until it fits or room can be made for it, or until it expires.
//
// Only an engine whose reserve the account gave back while it slept can be
// judged this way. Any other is charged its whole seat whether a request asks
// for it or not.
func (r *ModelClaimReconciler) wakeTurnOn(
	ctx context.Context,
	waker *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	ledger podLedger,
	readings *runtimeReadings,
) (wakeTurn, podLedger) {
	asleep := map[string]bool{}
	for _, engine := range ledger.engines {
		if engine.asleep {
			asleep[engine.claimName] = true
		}
	}
	type request struct{ claim, at string }
	var requests []request
	for key, at := range pod.Annotations {
		claim, isWake := strings.CutPrefix(key, constants.ModelClaimWakeAnnotationPrefix)
		if isWake && (asleep[claim] || claim == waker.Name) {
			requests = append(requests, request{claim, at})
		}
	}
	sort.Slice(requests, func(i, j int) bool {
		return askedBefore(requests[i].at, requests[i].claim, requests[j].at, requests[j].claim)
	})
	judged := ledger
	for _, req := range requests {
		judged = judged.withWakeReserve(req.claim, false)
	}
	lifecycle, canMakeRoom := r.roomCanBeMadeOn(ctx, waker, pod, ledger)
	var idle []idleEngine
	if canMakeRoom {
		idle = r.idleEngines(ctx, waker, pod, lifecycle.sleepToMakeRoomAfter(), readings)
	}
	roomMade := false
	for _, req := range requests {
		withIt := judged.withWakeReserve(req.claim, true)
		turn := wakeCannotFit
		switch {
		case withIt.maximumRoomBytes() >= 0 && withIt.heldRoomBytes() >= 0:
			turn = wakeNow
		case canMakeRoom && r.sleepsMakeRoom(withIt, idle, 0):
			turn = wakeMakesRoom
			if roomMade {
				turn = wakeWaitsBehind
			}
			roomMade = true
		}
		if req.claim == waker.Name {
			return turn, withIt
		}
		if turn != wakeCannotFit {
			judged = withIt
		}
	}
	return wakeCannotFit, ledger
}

// sleepsMakeRoom reports whether putting every engine of a list to sleep would
// leave a card with need bytes of room. An engine that has been seen asleep
// gives back what it is charged, less what it held then. One that has not is
// counted at what it is charged, the most it could give back.
func (r *ModelClaimReconciler) sleepsMakeRoom(ledger podLedger, sleepers []idleEngine, need int64) bool {
	charged := make(map[string]engineOnPod, len(ledger.engines))
	for _, engine := range ledger.engines {
		charged[engine.claimName] = engine
	}
	floors, holding := ledger.maximumRoomBytes()-need, ledger.heldRoomBytes()-need
	if floors >= 0 && holding >= 0 {
		return true
	}
	for _, sleeper := range sleepers {
		engine, found := charged[sleeper.claim.Name]
		if !found {
			continue
		}
		asleep, _ := r.footprints().seenAsleep(sleeper.claim)
		floors += engine.minimumReserveBytes() - asleep
		holding += engine.heldBytes() - asleep
		if floors >= 0 && holding >= 0 {
			return true
		}
	}
	return false
}

// askedBefore orders two wake requests by when they were asked, then by claim
// name. A request whose time cannot be read comes first, as it is the first to
// be taken back.
func askedBefore(at, name, otherAt, otherName string) bool {
	asked, err := time.Parse(time.RFC3339, at)
	otherAsked, otherErr := time.Parse(time.RFC3339, otherAt)
	switch {
	case err != nil && otherErr != nil:
		return name < otherName
	case err != nil:
		return true
	case otherErr != nil:
		return false
	case !asked.Equal(otherAsked):
		return asked.Before(otherAsked)
	}
	return name < otherName
}

// sleepToMakeRoom puts to sleep the engine idle longest on a card, to make room
// for a claim whose engine is to wake there. It returns true when it put one to
// sleep. One goes to sleep a pass. The next pass sees what it holds asleep, and
// asks again whether the waking engine fits.
func (r *ModelClaimReconciler) sleepToMakeRoom(
	ctx context.Context,
	waker *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	ledger podLedger,
	readings *runtimeReadings,
) bool {
	lifecycle, can := r.roomCanBeMadeOn(ctx, waker, pod, ledger)
	if !can {
		return false
	}
	idle := r.idleEngines(ctx, waker, pod, lifecycle.sleepToMakeRoomAfter(), readings)
	return len(idle) > 0 && r.putIdleEngineToSleep(ctx, idle[0], pod, waker, readings)
}

// roomCanBeMadeOn reports whether putting engines to sleep on a card gives room
// back, and returns the pool's lifecycle policy when it does. That is a pool
// that keeps no wake reserve, and a card where every engine asleep was
// measured. An engine whose memory asleep is not known keeps its reserve, so
// the next one put to sleep would most likely free nothing either. A card held
// for another claim keeps its room for that claim, so no engine there is put
// to sleep for this one.
func (r *ModelClaimReconciler) roomCanBeMadeOn(
	ctx context.Context,
	forClaim *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	ledger podLedger,
) (*poolLifecyclePolicy, bool) {
	if ledger.reservedBytes > 0 {
		return nil, false
	}
	lifecycle := r.poolLifecycleOf(ctx, pod)
	if lifecycle == nil || !lifecycle.NoWakeReserveWhileAsleep {
		return nil, false
	}
	for _, engine := range ledger.engines {
		if engine.claimName != forClaim.Name && engine.asleep && engine.sleepingFootprintBytes == 0 {
			return nil, false
		}
	}
	return lifecycle, true
}

// putIdleEngineToSleep puts an idle engine to sleep to make room for a claim,
// and reports whether it did. The claim the engine serves is told whom the room
// is for, and what its engine holds asleep.
func (r *ModelClaimReconciler) putIdleEngineToSleep(
	ctx context.Context,
	idle idleEngine,
	pod *corev1.Pod,
	forClaim *modelv1alpha1.ModelClaim,
	readings *runtimeReadings,
) bool {
	operationID := fmt.Sprintf("make-room/%s/%s/%s/%d",
		pod.UID, snapshotActivityKey(idle.model), forClaim.UID, idle.idleSince.UnixNano())
	if err := r.putEngineToSleep(ctx, idle.claim, pod, idle.port, idle.model.ModelName, operationID, readings); err != nil {
		if errors.Is(err, errEngineServing) {
			klog.V(2).InfoS("an engine found idle serves a request, so it is left awake",
				"pod", klog.KObj(pod), "model", idle.model.ModelName, "for", forClaim.Name)
		} else {
			klog.ErrorS(err, "could not put an idle engine to sleep to make room",
				"pod", klog.KObj(pod), "model", idle.model.ModelName, "for", forClaim.Name)
		}
		return false
	}
	asleep := r.sleptEngine(ctx, pod, idle.claim, readings)
	if footprint, known := sleepingFootprintOf(asleep); known {
		r.footprints().note(idle.claim, footprint)
	}
	r.Recorder.Eventf(idle.claim, corev1.EventTypeNormal, "SleptToMakeRoom",
		"model %s idle for %s; put to sleep on pod %s to make room for model %s%s",
		idle.model.ModelName, r.poolPolicyManager().now().Sub(idle.idleSince).Round(time.Second),
		pod.Name, servedModelName(forClaim), sleepingFootprintNote(asleep))
	r.warnOfAnUnmeasuredSleep(idle.claim, pod.Name, asleep, true)
	return true
}

// idleEngine is an engine that may be put to sleep to make room.
type idleEngine struct {
	claim     *modelv1alpha1.ModelClaim
	model     RuntimeSnapshotModel
	port      int32
	idleSince time.Time
}

// idleEngines lists the engines on a pod that may be put to sleep to make room
// for a claim, the one idle longest first. Such an engine is awake and routed,
// has no request running or waiting, and has been idle for at least idleFor.
// Idle time counts from when the pool policy last saw the engine busy, from the
// engine's last change of phase, or from when it was last routed, whichever is
// latest, as the idle timer counts it. An engine the pool policy has not seen
// yet is not known to be idle.
func (r *ModelClaimReconciler) idleEngines(
	ctx context.Context,
	forClaim *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	idleFor time.Duration,
	readings *runtimeReadings,
) []idleEngine {
	snapshot, err := readings.of(ctx, pod)
	if err != nil || snapshot == nil {
		return nil
	}
	claims := &modelv1alpha1.ModelClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(pod.Namespace)); err != nil {
		return nil
	}
	manager := r.poolPolicyManager()
	var idle []idleEngine
	for _, model := range snapshot.Models {
		if model.Phase != runtimePhaseActive || !model.Alive || !model.Ready || !model.RequestMetricsObserved ||
			model.RequestsRunning > 0 || model.RequestsWaiting > 0 {
			continue
		}
		claim := claimForRuntimeSnapshot(claims, model)
		if claim == nil || claim.Name == forClaim.Name || !isVLLMModel(claim) || neverSleeps(claim) {
			continue
		}
		port, active := activeClaimInstancePort(claim, pod.Name)
		if !active {
			continue
		}
		idleSince, seen := manager.lastActive(poolActivityKey(pod, model))
		if !seen {
			continue
		}
		if model.LastTransition != nil && model.LastTransition.After(idleSince) {
			idleSince = *model.LastTransition
		}
		if routed := routedSince(claim); routed.After(idleSince) {
			idleSince = routed
		}
		if manager.now().Sub(idleSince) < idleFor {
			continue
		}
		idle = append(idle, idleEngine{claim: claim, model: model, port: port, idleSince: idleSince})
	}
	sort.SliceStable(idle, func(i, j int) bool {
		if !idle[i].idleSince.Equal(idle[j].idleSince) {
			return idle[i].idleSince.Before(idle[j].idleSince)
		}
		return idle[i].claim.Name < idle[j].claim.Name
	})
	return idle
}

// wakeDivision divides a card for an engine about to wake. Every move of the
// plan is carried out, and each moved engine's claim is told: the neighbours
// give back room they did not ask to give up.
var wakeDivision = division{announce: true}

// cardArrangedForWake makes sure an engine's card is held as its plan has it
// before the engine wakes, and reports whether it is. An engine that slept
// without a wake reserve left room that its neighbours may have been given.
// They are held to their shares again first, or they could grow into the
// memory the engine wakes into. The engine itself is raised to its floor,
// since it was held to only the KV it had mapped while it slept.
//
// A card whose engines are already held as planned is left alone, as a card
// normally is where the engine kept its reserve. A card that cannot be planned,
// or could not be divided, is tried again on a later pass, and the engine
// sleeps until then. So does one whose claim could not be read back after the
// division wrote it.
func (r *ModelClaimReconciler) cardArrangedForWake(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	ledger podLedger,
	readings *runtimeReadings,
) bool {
	if !ledger.judgeable || !podHasGPUs(*pod, ledger.accelerators) {
		return true
	}
	limits, err := planKVLimits(ledger.plannableBytes(), ledger.engines)
	if err != nil {
		// A card with room for the engine can be planned, so this is not
		// expected. The engine is not woken into a card held to no plan.
		klog.V(2).InfoS("wake waits for its card to be planned",
			"model", pm.Name, "pod", klog.KObj(pod), "err", err)
		return false
	}
	if heldAsPlannedForWake(ledger.engines, limits, pm.Name) {
		return true
	}
	if _, err := r.arrangeCard(ctx, pod, ledger, ledger.engines, wakeDivision, readings); err != nil {
		klog.V(2).InfoS("wake waits for its card to be divided again",
			"model", pm.Name, "pod", klog.KObj(pod), "err", err)
		return false
	}
	// The division wrote this claim, so the status this pass writes would
	// meet a conflict unless the claim is read back. When it cannot be, the
	// engine is woken on the next pass, which finds the card arranged.
	return r.catchUpWithRecordedLimit(ctx, pm, pod.Name)
}

// catchUpWithRecordedLimit brings the claim this pass holds up to the KV limit
// that a division recorded on it for one pod, and reports whether it could. The
// division wrote the claim itself, so without this the status written at the
// end of the pass would conflict with that record. Nothing else writes a
// claim's status while the controller reconciles it, so the limit is the one
// field to take over.
func (r *ModelClaimReconciler) catchUpWithRecordedLimit(ctx context.Context, pm *modelv1alpha1.ModelClaim, podName string) bool {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	fresh := &modelv1alpha1.ModelClaim{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(pm), fresh); err != nil {
		klog.V(2).InfoS("wake waits for its claim to be read back",
			"model", pm.Name, "pod", podName, "err", err)
		return false
	}
	for _, recorded := range fresh.Status.Instances {
		if recorded.Pod != podName {
			continue
		}
		for i := range pm.Status.Instances {
			if pm.Status.Instances[i].Pod == podName {
				pm.Status.Instances[i].KVLimitBytes = recorded.KVLimitBytes
			}
		}
	}
	pm.ResourceVersion = fresh.ResourceVersion
	return true
}

// heldAsPlannedForWake reports whether no engine on a card is held to more
// than its plan gives it, and the engine about to wake to no less. An engine
// without a KV segment holds nothing to compare.
func heldAsPlannedForWake(engines []engineOnPod, limits []plannedKVLimit, waking string) bool {
	planned := make(map[string]int64, len(limits))
	for _, limit := range limits {
		planned[limit.claimName] = limit.kvLimitBytes
	}
	for _, engine := range engines {
		if engine.kvCapacityBytes < 0 {
			continue
		}
		limit := planned[engine.claimName]
		if engine.kvCapacityBytes > limit || (engine.claimName == waking && engine.kvCapacityBytes < limit) {
			return false
		}
	}
	return true
}

// markMoving marks an instance whose engine cannot wake where it is, so that
// the replacement of failed instances moves its claim to another pod.
//
// The mark is written at once. The replacement stops the engine before it
// writes its own record, and a pass that ended between the two, on a conflict
// or with no pod that answers, would otherwise leave the claim saying that the
// engine sleeps, with no engine there. Written, the mark stands until the claim
// has moved, as a failed instance does. The route says at once that the claim
// moves, so a client is told how long to wait, even while the replacement
// waits its turn.
func (r *ModelClaimReconciler) markMoving(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	slot int,
	pod *corev1.Pod,
	reason, why string,
) error {
	was := pm.Status.Instances[slot]
	pm.Status.Instances[slot].Phase = modelv1alpha1.ModelClaimFailed
	pm.Status.Instances[slot].Reason = reason
	if err := r.Status().Update(ctx, pm); err != nil {
		pm.Status.Instances[slot] = was
		return err
	}
	r.Recorder.Eventf(pm, corev1.EventTypeWarning, "Moving",
		"model %s cannot wake on pod %s, and moves to another pod: %s", servedModelName(pm), pod.Name, why)
	if err := r.annotateWarmPodWithState(ctx, pm, pod, 0, constants.ModelClaimRoutingStateFailed,
		readyReasonMoving); err != nil {
		klog.ErrorS(err, "could not say on the route that a claim moves", "pod", klog.KObj(pod), "model", pm.Name)
	}
	return nil
}

// callOffMove calls off the move of an instance marked to move for want of
// room, once its card can take its engine back. The move has not started while
// the engine still sleeps there: the replacement stops it only once another
// pod takes the claim. The card is judged as a wake is, so a wake asked for
// after the call goes ahead on that card. A card that still has no room, or
// cannot be judged, keeps the mark, and any other instance is left as it is.
// An error is a call that could not be written, and the mark then stands.
func (r *ModelClaimReconciler) callOffMove(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	slot int,
	pod *corev1.Pod,
	ledgersOf func([]corev1.Pod) map[string]podLedger,
	readings *runtimeReadings,
) error {
	inst := pm.Status.Instances[slot]
	if inst.Phase != modelv1alpha1.ModelClaimFailed || inst.Reason != instanceReasonNoRoomToWake ||
		pod.Status.PodIP == "" {
		return nil
	}
	snapshot, err := readings.of(ctx, pod)
	if err != nil {
		return nil
	}
	served := servedModelName(pm)
	engine := snapshotModelForClaim(snapshot, pm, served)
	if engine == nil || !engine.Alive || engine.Phase != runtimePhaseSleeping {
		return nil
	}
	ledger := ledgersOf([]corev1.Pod{*pod})[pod.Name]
	if !ledger.judgeable || ledger.maximumRoomBytes() < 0 || ledger.heldRoomBytes() < 0 {
		return nil
	}
	pm.Status.Instances[slot].Phase = modelv1alpha1.ModelClaimSleeping
	pm.Status.Instances[slot].Reason = ""
	if err := r.Status().Update(ctx, pm); err != nil {
		pm.Status.Instances[slot] = inst
		return err
	}
	r.Recorder.Eventf(pm, corev1.EventTypeNormal, "MoveCalledOff",
		"model %s stays asleep on pod %s, whose card can take it back now", served, pod.Name)
	// The route says at once that the engine sleeps there, as markMoving says
	// at once that the claim moves. Until the next health check, the gateway
	// would otherwise tell a client that the claim moves, and ask no wake.
	if err := r.annotateWarmPodWithState(ctx, pm, pod, 0, constants.ModelClaimRoutingStateSleeping,
		bindingReason(&pm.Status.Instances[slot])); err != nil {
		klog.ErrorS(err, "could not say on the route that a claim stays", "pod", klog.KObj(pod), "model", pm.Name)
	}
	return nil
}

// setWaitingForRoom records on a sleeping instance whether a request waits
// for room on its card, and tells the gateway on the route at once.
func (r *ModelClaimReconciler) setWaitingForRoom(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
	waiting bool,
) {
	if inst.Phase != modelv1alpha1.ModelClaimSleeping || (inst.Reason == instanceReasonWaitingForRoom) == waiting {
		return
	}
	inst.Reason = ""
	if waiting {
		inst.Reason = instanceReasonWaitingForRoom
	}
	if err := r.annotateWarmPodWithState(ctx, pm, pod, 0, constants.ModelClaimRoutingStateSleeping,
		bindingReason(inst)); err != nil {
		klog.ErrorS(err, "could not say on the route that a wake waits for room", "pod", klog.KObj(pod), "model", pm.Name)
	}
}

// wakeRequestExpired reports whether a request has waited longer than
// wakeRequestLifetime. The wait is timed on this controller's clock, from when
// it first saw the request. The time a request carries is on the clock of the
// gateway that wrote it, which may be off from this one, so it only tells one
// request from the next.
func (r *ModelClaimReconciler) wakeRequestExpired(pod *corev1.Pod, key, requestedAt string) bool {
	now := r.now()
	seen := r.wakeRequestsSeen().firstSeen(string(pod.UID)+"/"+key+"/"+requestedAt, now)
	return now.Sub(seen) > wakeRequestLifetime
}

// wakeRequestClock remembers when this controller first saw each wake request.
// A restart forgets it, which gives a request that is still waiting another
// wakeRequestLifetime.
type wakeRequestClock struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// firstSeen returns when a request was first seen, and records now for one
// that is new. A request seen longer ago than twice its lifetime has been
// taken back, so it is forgotten.
func (c *wakeRequestClock) firstSeen(request string, now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	for known, at := range c.seen {
		if now.Sub(at) > 2*wakeRequestLifetime {
			delete(c.seen, known)
		}
	}
	if at, known := c.seen[request]; known {
		return at
	}
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	c.seen[request] = now
	return now
}

func (r *ModelClaimReconciler) wakeRequestsSeen() *wakeRequestClock {
	if r.WakeRequests == nil {
		r.WakeRequests = &wakeRequestClock{}
	}
	return r.WakeRequests
}

// canPlaceElsewhere reports whether placement could take the claim to another
// pod now. It judges as placement does, by the account that ledgersOf draws up,
// so a claim marked to move is moved in the same pass. Placement never picks a
// pod the claim is on, so neither does this.
func canPlaceElsewhere(
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	ledgersOf func([]corev1.Pod) map[string]podLedger,
) bool {
	perGPU, err := perGPUBytesOf(pm)
	if err != nil {
		return false
	}
	on := instancePods(pm)
	others := make([]corev1.Pod, 0, len(candidates))
	for _, candidate := range candidates {
		if !on[candidate.Name] {
			others = append(others, candidate)
		}
	}
	if len(others) == 0 {
		return false
	}
	admissible, _ := admissibleCandidates(others, ledgersOf(others), perGPU.minimumReserveBytes(), instanceGPUCount(pm))
	return len(admissible) > 0
}

// bindingReason is what the route says, beside its state, about why an
// instance is not served: the reason of its Ready condition, when it is more
// than the state.
func bindingReason(inst *modelv1alpha1.ModelClaimInstance) string {
	switch {
	case inst.Phase == modelv1alpha1.ModelClaimSleeping && inst.Reason == instanceReasonWaitingForRoom:
		return readyReasonWaitingForRoom
	case inst.Phase == modelv1alpha1.ModelClaimSleeping && inst.Reason == instanceReasonWakeFailed:
		return instanceReasonWakeFailed
	case inst.Phase == modelv1alpha1.ModelClaimFailed && movingReason(inst.Reason):
		return readyReasonMoving
	}
	return ""
}

// takeBackWakeRequest removes a wake request from its pod.
func (r *ModelClaimReconciler) takeBackWakeRequest(ctx context.Context, pod *corev1.Pod, key string) {
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Annotations, key)
	delete(pod.Annotations, constants.ModelClaimPolicyWakeAnnotationPrefix+strings.TrimPrefix(key, constants.ModelClaimWakeAnnotationPrefix))
	if err := r.Patch(ctx, pod, patch); err != nil {
		klog.ErrorS(err, "could not take back a wake request", "pod", klog.KObj(pod), "annotation", key)
	}
}

// wakeRequestsOf returns the wake requests on a pod, by annotation key.
func wakeRequestsOf(pod *corev1.Pod) map[string]string {
	var requests map[string]string
	for key, value := range pod.GetAnnotations() {
		if !strings.HasPrefix(key, constants.ModelClaimWakeAnnotationPrefix) {
			continue
		}
		if requests == nil {
			requests = map[string]string{}
		}
		requests[key] = value
	}
	return requests
}

// withoutWakeRequests returns a copy of a pod without its wake requests, and
// without the fields every write changes.
func withoutWakeRequests(pod *corev1.Pod) *corev1.Pod {
	stripped := pod.DeepCopy()
	for key := range wakeRequestsOf(pod) {
		delete(stripped.Annotations, key)
	}
	stripped.ResourceVersion = ""
	stripped.ManagedFields = nil
	return stripped
}

// notOnlyWakeRequests keeps the events of a pod that changed in more than its
// wake requests. A wake request concerns one claim, and wakeRequestsChanged
// enqueues that claim alone. Every claim in the namespace need not be looked at
// again for it.
func notOnlyWakeRequests() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			if !okOld || !okNew {
				return true
			}
			if equality.Semantic.DeepEqual(wakeRequestsOf(oldPod), wakeRequestsOf(newPod)) {
				return true
			}
			return !equality.Semantic.DeepEqual(withoutWakeRequests(oldPod), withoutWakeRequests(newPod))
		},
	}
}

// wakeRequestsChanged keeps the events of a pod whose wake requests were
// written or changed.
func wakeRequestsChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod, ok := e.Object.(*corev1.Pod)
			return ok && len(wakeRequestsOf(pod)) > 0
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			if !okOld || !okNew {
				return false
			}
			now := wakeRequestsOf(newPod)
			if len(now) == 0 {
				return false
			}
			before := wakeRequestsOf(oldPod)
			for key, value := range now {
				if before[key] != value {
					return true
				}
			}
			return false
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// enqueueRequestedWakes enqueues the claims a pod's wake requests name.
func enqueueRequestedWakes(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(wakeRequestsOf(pod)))
	for key := range wakeRequestsOf(pod) {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      strings.TrimPrefix(key, constants.ModelClaimWakeAnnotationPrefix),
		}})
	}
	return requests
}

var _ handler.MapFunc = enqueueRequestedWakes
