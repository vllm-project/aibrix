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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// perGPUBytes is a claim's spec.perGPU in bytes: what one instance costs on
// each GPU it runs on.
type perGPUBytes struct {
	maximumFootprintBytes int64
	kvFloorBytes          int64
}

// minimumReserveBytes is what one instance takes off a card and does not give
// back while it is awake: its maximum footprint plus its KV floor. An engine's
// KV can be squeezed towards that floor but never past it, so this is a lower
// bound on what the instance occupies rather than an estimate of it.
func (p perGPUBytes) minimumReserveBytes() int64 {
	return p.maximumFootprintBytes + p.kvFloorBytes
}

// maximumDeclaredBytes is the most a claim may declare for one figure. No card
// holds a pebibyte, and figures this small can be added up for every instance
// on a card without running past an int64.
var maximumDeclaredBytes = resource.MustParse("1Pi")

// smallestRoundedBytes is the smallest figure that is rounded up to a whole
// number of bytes. A smaller one with part of a byte in it is refused.
var smallestRoundedBytes = resource.MustParse("1Mi")

// perGPUBytesOf reads what a claim declared one instance costs on a GPU, and
// says what is wrong with the declaration when it cannot be used.
//
// A quantity carries no schema bounds, so a figure that cannot be used is
// caught here. One that is not positive is refused rather than read as a model
// that costs nothing, which is what a zero would otherwise say. A small one
// with part of a byte in it is most likely a slip, such as 30m for 30M, and
// would otherwise be read as a single byte. A larger one comes from a decimal
// fraction of a binary unit, such as 5.6Gi, and is rounded up.
func perGPUBytesOf(pm *modelv1alpha1.ModelClaim) (perGPUBytes, error) {
	if pm == nil || pm.Spec.PerGPU == nil {
		return perGPUBytes{}, errors.New("spec.perGPU is missing")
	}
	maximumFootprintBytes, err := declaredBytes("maximumFootprint", pm.Spec.PerGPU.MaximumFootprint)
	if err != nil {
		return perGPUBytes{}, err
	}
	kvFloorBytes, err := declaredBytes("kvFloor", pm.Spec.PerGPU.KVFloor)
	if err != nil {
		return perGPUBytes{}, err
	}
	return perGPUBytes{maximumFootprintBytes: maximumFootprintBytes, kvFloorBytes: kvFloorBytes}, nil
}

// declaredBytes is one figure of spec.perGPU in bytes, or what is wrong with
// it.
func declaredBytes(name string, declared resource.Quantity) (int64, error) {
	wrong := ""
	switch {
	case declared.Sign() <= 0:
		wrong = "not positive"
	case declared.Cmp(maximumDeclaredBytes) > 0:
		wrong = "more than " + maximumDeclaredBytes.String()
	case declared.Cmp(smallestRoundedBytes) < 0 &&
		declared.Cmp(*resource.NewQuantity(declared.Value(), declared.Format)) != 0:
		wrong = "not a whole number of bytes"
	}
	if wrong != "" {
		return 0, fmt.Errorf("spec.perGPU.%s is %s, which is %s", name, declared.String(), wrong)
	}
	return declared.Value(), nil
}

// kvLimitUnknown stands for an engine whose KV segment could not be read, which
// is what an engine that has not built one yet and one that is gone have in
// common.
const kvLimitUnknown = int64(-1)

// engineOnPod is one instance on a card: what its claim declared it costs, and
// what its engine is doing with the memory right now. An instance that has been
// recorded but whose engine has not started yet is described here too, with
// nothing mapped and no limit in force.
type engineOnPod struct {
	claimName string
	// modelName is what the runtime knows this engine as, which is the name a
	// limit is written against.
	modelName string
	// snapshotKey identifies the engine in a runtime snapshot, and is empty
	// while no engine has been seen.
	snapshotKey string
	// perGPUBytes is what the claim declared one instance costs on a card.
	perGPUBytes
	// kvUsedBytes is the KV this engine has mapped: its pages in use and the
	// ones it holds in reserve. An engine with no KV segment has mapped
	// nothing, so this is zero rather than unknown.
	kvUsedBytes int64
	// kvCapacityBytes is the limit written in the engine's segment now, and is
	// negative when there is no segment to read it from.
	kvCapacityBytes int64
	// kvRecordedBytes is the limit the instance records now, and is zero when
	// it records none.
	kvRecordedBytes int64
	// inFlightRequests is the demand an engine's part of the spare KV is
	// weighed by: its running and waiting requests.
	inFlightRequests int64
	// requestsWaiting is how many of those requests wait for the engine to
	// take them.
	requestsWaiting int64
	// demandUnknown is whether the engine serves but its request metrics could
	// not be read, so its demand is not known.
	demandUnknown bool
	// asleep is whether the runtime reports the engine sleeping. A sleeping
	// engine serves nothing, so it is given no part of the spare KV.
	asleep bool
	// sleepingFootprintBytes is the memory the runtime measured the engine to
	// hold after it went to sleep, and zero when that is not known.
	sleepingFootprintBytes int64
	// withoutWakeReserve is whether the engine sleeps without a wake reserve:
	// its pool keeps none, its memory asleep is known, and no request asks to
	// wake it. It is then charged only what it still holds.
	withoutWakeReserve bool
	// wakeReserveAsked is whether the engine would sleep without a wake
	// reserve, but a request to wake it has put the reserve back.
	wakeReserveAsked bool
}

// minimumReserveBytes is what an instance is promised on its card: the
// footprint and floor its claim declared. An engine asleep without a wake
// reserve is promised only the memory it still holds.
func (e engineOnPod) minimumReserveBytes() int64 {
	if e.withoutWakeReserve {
		return e.sleepingFootprintBytes
	}
	return e.perGPUBytes.minimumReserveBytes()
}

// kvHeldBytes is the KV an engine keeps whatever else happens on the card: the
// floor its claim declared, or what it has already mapped when that is larger.
// Lowering a limit does not evict a mapped page, so the pages an engine holds
// are not room that can be offered to anyone else.
func (e engineOnPod) kvHeldBytes() int64 {
	if e.kvUsedBytes > e.kvFloorBytes {
		return e.kvUsedBytes
	}
	return e.kvFloorBytes
}

// heldBytes is what one instance occupies on a card now and will not give
// back: its maximum footprint and the KV it holds. An engine asleep without a
// wake reserve holds what its runtime measured, its mapped KV included.
func (e engineOnPod) heldBytes() int64 {
	if e.withoutWakeReserve {
		return e.sleepingFootprintBytes
	}
	return e.maximumFootprintBytes + e.kvHeldBytes()
}

// plannedKVHeldBytes is the KV an engine keeps in a division before its share
// of the spare. An engine asleep without a wake reserve keeps only what it has
// mapped. That is part of the memory it is charged, and the rest of its floor
// is not its own until it is asked to wake.
func (e engineOnPod) plannedKVHeldBytes() int64 {
	if e.withoutWakeReserve {
		return e.kvUsedBytes
	}
	return e.kvHeldBytes()
}

// podLedger is one card's account: how much it can hold, and how much of it the
// instances already recorded there were promised.
//
// judgeable is false when the account has a hole in it, and blocked says which
// one. An account with a hole never admits a placement: the memory it cannot
// see is memory it would otherwise hand out twice.
type podLedger struct {
	judgeable bool
	blocked   string
	// keepsNoWakeReserve is whether the pod's pool keeps no wake reserve for
	// its sleeping engines.
	keepsNoWakeReserve bool
	// unread is set when the pod's runtime did not answer, or the claims could
	// not be listed. Nothing is known about the pod then, not even whether it
	// has cards.
	unread                   bool
	hbmUsableBytes           int64
	totalMinimumReserveBytes int64
	totalHeldBytes           int64
	// reservedBytes is the room held on this card for another claim, while
	// engines are put to sleep to make room for it. It is taken from what
	// the card offers, and from what a division of the card hands out.
	reservedBytes int64
	// accelerators is how many cards the runtime reported, which is what
	// makes a pod that requests no nvidia.com/gpu still a pod with cards. It
	// is one where a reading missed the card, and an engine on the pod holds a
	// KV segment or an instance records a limit.
	accelerators int
	// observedAt is when the snapshot this account was built from was taken. A
	// limit written from it carries the same moment, which is what tells the
	// runtime one attempt from the next.
	observedAt time.Time
	// engines are the instances recorded on this card, each with what its claim
	// declared and what its engine currently holds.
	engines []engineOnPod
}

// maximumRoomBytes is the most memory this card could ever offer another
// instance: what is left once every instance on it is down to the KV floor its
// claim declared. Getting there means engines give back everything above their
// floor, so a model that needs more than this cannot be placed here by waiting.
// It is negative when the card is already promised more than it has.
func (l podLedger) maximumRoomBytes() int64 {
	return l.hbmUsableBytes - l.totalMinimumReserveBytes - l.reservedBytes
}

// plannableBytes is what a division of the card shares out: all of the card
// but the room held there for another claim.
func (l podLedger) plannableBytes() int64 {
	return l.hbmUsableBytes - l.reservedBytes
}

// heldRoomBytes is what this card can offer another instance now, without
// anything being given back: what is left once every engine keeps its footprint
// and the KV it already holds. Lowering a limit evicts nothing, so a model that
// needs more than this cannot be placed here today even though the card may be
// able to hold it later.
func (l podLedger) heldRoomBytes() int64 {
	return l.hbmUsableBytes - l.totalHeldBytes - l.reservedBytes
}

// withHole marks an account that cannot be trusted, keeping the first cause
// found: one reason an operator can act on beats a list that grows with the
// pool.
func (l podLedger) withHole(reason string) podLedger {
	if l.blocked == "" {
		l.blocked = reason
	}
	l.judgeable = false
	return l
}

// collectPodLedgers builds one account per candidate pod.
//
// The snapshots must be this pass's readings, never older ones. What an engine
// holds moves with traffic, and admitting a model against memory another engine
// has since mapped is how a card ends up oversubscribed.
//
// What a card owes comes from ModelClaim status, which only this controller
// writes, so the account charges an instance from the moment it is recorded
// rather than from the moment its engine gets around to allocating. The
// snapshot then says how much of that an engine has actually taken.
func (r *ModelClaimReconciler) collectPodLedgers(
	ctx context.Context,
	namespace string,
	candidates []corev1.Pod,
	snapshots map[string]*RuntimeSnapshot,
	forClaim string,
) map[string]podLedger {
	claims, err := r.listClaimsForAccount(ctx, namespace)
	ledgers := podLedgersFrom(claims, err, candidates, snapshots, r.podsWithoutWakeReserve(ctx, candidates))
	r.reservations().takeFrom(ledgers, namespace, forClaim, r.now())
	return ledgers
}

// listClaimsForAccount lists the claims in a namespace for the GPU memory
// account.
//
// Deliberately not the cached client. An instance recorded moments ago may not
// have reached the informer yet. An instance missing from the account is memory
// that a second claim would be told is free.
func (r *ModelClaimReconciler) listClaimsForAccount(
	ctx context.Context,
	namespace string,
) (*modelv1alpha1.ModelClaimList, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	claims := &modelv1alpha1.ModelClaimList{}
	if err := reader.List(ctx, claims, client.InNamespace(namespace)); err != nil {
		klog.ErrorS(err, "list model claims for the GPU memory account", "namespace", namespace)
		return nil, err
	}
	return claims, nil
}

// observe takes in what a runtime reading says of an instance's engine.
func (e *engineOnPod) observe(model *RuntimeSnapshotModel, instance modelv1alpha1.ModelClaimInstance) {
	e.snapshotKey = snapshotActivityKey(*model)
	e.kvCapacityBytes = model.KVCapacityBytes
	e.inFlightRequests = max(model.RequestsRunning, 0) + max(model.RequestsWaiting, 0)
	e.requestsWaiting = max(model.RequestsWaiting, 0)
	// A scrape that failed says nothing about load, and the engine may be too
	// busy to answer it in time. A serving engine whose metrics could not be
	// read is not taken for idle. Only an engine that serves can be busy,
	// though. The runtime reports an engine ready before it has finished
	// booting, after a start or a wake, and it reads no metrics until then. The
	// gateway routes only to an Active instance. An engine that is not both
	// active and routed has no load that could have gone unread.
	e.demandUnknown = model.Ready && !model.RequestMetricsObserved &&
		model.Phase == runtimePhaseActive && instance.Phase == modelv1alpha1.ModelClaimActive
	e.asleep = model.Phase == runtimePhaseSleeping
	if footprint, known := sleepingFootprintOf(model); e.asleep && known {
		e.sleepingFootprintBytes = footprint
	}
	// A negative figure means there is no KV segment to read, and an engine
	// without one has mapped nothing.
	e.kvUsedBytes = max(model.KVUsedBytes, 0)
}

// podLedgersFrom builds one account per candidate pod from a listing of the
// claims, or marks every account as a hole when the listing failed.
// withoutWakeReserve names the pods whose pool keeps no wake reserve for a
// sleeping engine.
func podLedgersFrom(
	claims *modelv1alpha1.ModelClaimList,
	listErr error,
	candidates []corev1.Pod,
	snapshots map[string]*RuntimeSnapshot,
	withoutWakeReserve map[string]bool,
) map[string]podLedger {
	ledgers := make(map[string]podLedger, len(candidates))
	pods := make(map[string]*corev1.Pod, len(candidates))
	for i := range candidates {
		pods[candidates[i].Name] = &candidates[i]
	}
	for i := range candidates {
		pod := &candidates[i]
		hbmUsableBytes, measured := snapshots[pod.Name].hbmUsableBytes()
		accelerators := reportedAccelerators(snapshots[pod.Name])
		switch {
		case snapshots[pod.Name] == nil:
			ledgers[pod.Name] = podLedger{blocked: "its runtime did not answer", unread: true,
				keepsNoWakeReserve: withoutWakeReserve[pod.Name]}
		case !measured:
			ledgers[pod.Name] = podLedger{blocked: unmeasuredCards(snapshots[pod.Name]), accelerators: accelerators,
				keepsNoWakeReserve: withoutWakeReserve[pod.Name]}
		default:
			ledgers[pod.Name] = podLedger{
				keepsNoWakeReserve: withoutWakeReserve[pod.Name],
				judgeable:          true,
				hbmUsableBytes:     hbmUsableBytes,
				observedAt:         snapshots[pod.Name].ObservedAt,
				accelerators:       accelerators,
			}
		}
	}

	if listErr != nil {
		// Without the claims, nothing says what is recorded on a pod, and so
		// nothing says whether it has a card. Every pod is turned away, and
		// the claims are the reason. A card that this reading did not show
		// is no reason of its own here: a claim may record one on the pod.
		// A runtime that did not answer stays the reason for its pod.
		for name, ledger := range ledgers {
			if !ledger.unread {
				ledger.blocked = ""
			}
			ledger.unread = true
			ledgers[name] = ledger.withHole("the claims on it could not be listed")
		}
		return ledgers
	}

	accounted := make(map[string]map[string]struct{}, len(ledgers))
	for i := range claims.Items {
		claim := &claims.Items[i]
		served := servedModelName(claim)
		for _, instance := range claim.Status.Instances {
			ledger, tracked := ledgers[instance.Pod]
			if !tracked {
				continue
			}
			// A limit is recorded only where a card was divided. So a pod on
			// which an instance records one has a card, whatever this reading
			// says of it.
			if instance.KVLimitBytes > 0 && ledger.accelerators == 0 {
				ledger.accelerators = 1
				ledgers[instance.Pod] = ledger
			}
			perGPU, perGPUErr := perGPUBytesOf(claim)
			engine := engineOnPod{
				claimName:       claim.Name,
				modelName:       served,
				perGPUBytes:     perGPU,
				kvCapacityBytes: kvLimitUnknown,
				kvRecordedBytes: instance.KVLimitBytes,
			}
			alive := false
			if model := snapshotModelForClaim(snapshots[instance.Pod], claim, served); model != nil {
				alive = model.Alive
				engine.observe(model, instance)
				if _, seen := accounted[instance.Pod]; !seen {
					accounted[instance.Pod] = map[string]struct{}{}
				}
				accounted[instance.Pod][engine.snapshotKey] = struct{}{}
			}
			// A failed instance has exhausted its restarts, so its memory is
			// back with the card unless its engine is somehow still there.
			// Charging for an engine that is gone would take a slice of GPU out
			// of circulation for as long as the claim exists, and nothing would
			// put it back. The runtime goes on listing an engine it has given
			// up on, not alive, so an engine listed but dead is gone as well.
			// An activating instance is charged, and deliberately: placement has
			// already committed those bytes, and waiting for readiness would
			// let a second claim be placed against the same memory.
			if instance.Phase == modelv1alpha1.ModelClaimFailed && (engine.snapshotKey == "" || !alive) {
				continue
			}
			// A sleeping engine whose pool keeps no wake reserve is charged
			// only what its runtime measured it to hold asleep, on its
			// heaviest card when it has several. A request to wake it puts
			// the reserve back at once, so the room is its own again from
			// then on. An engine whose memory asleep is not known keeps its
			// reserve, since nothing says how much of it is free.
			keepsNone := withoutWakeReserve[instance.Pod] && engine.sleepingFootprintBytes > 0
			asked := wakeAsked(pods[instance.Pod], claim.Name)
			engine.withoutWakeReserve = keepsNone && !asked
			engine.wakeReserveAsked = keepsNone && asked
			// A claim whose declaration cannot be used is charged nothing, so its
			// card cannot be judged either. A zero written by mistake would
			// otherwise read as an engine that takes up no room.
			if perGPUErr != nil {
				ledger = ledger.withHole(fmt.Sprintf("%s runs there, and %v", claim.Name, perGPUErr))
			}
			ledger.totalMinimumReserveBytes += engine.minimumReserveBytes()
			ledger.totalHeldBytes += engine.heldBytes()
			ledger.engines = append(ledger.engines, engine)
			ledgers[instance.Pod] = ledger
		}
	}

	// An engine nobody claimed is memory nobody can account for. It is charged
	// to no instance, so leaving the card judgeable would offer its memory to
	// the next model as though it were free.
	for name, ledger := range ledgers {
		for _, model := range snapshots[name].models() {
			if _, known := accounted[name][snapshotActivityKey(model)]; known {
				continue
			}
			// An engine that was told to stop holds its memory until its last
			// process has exited, and the runtime lists it as stopping until
			// then. The runtime reports it alive only while its first process
			// lives, which the processes that hold the card can outlive. So
			// the phase is asked here, and not whether it is alive.
			if model.Phase == runtimePhaseStopping {
				ledgers[name] = ledger.withHole(fmt.Sprintf(
					"the engine that served %s there is still exiting", model.ModelName))
				break
			}
			// The runtime stops an engine once its restarts run out, and goes
			// on listing it as failed. That engine has given its memory back,
			// and does not come back by itself. Any other engine counts, alive
			// or not: one whose first process died is started again by the
			// runtime, and takes its memory again.
			if model.Phase == runtimePhaseFailed && !model.Alive {
				continue
			}
			ledgers[name] = ledger.withHole(fmt.Sprintf(
				"the engine serving %s there answers to no claim", model.ModelName))
			break
		}
	}
	return ledgers
}

// unmeasuredCards says why a pod's cards could not be sized.
//
// A runtime that could not measure a card reports a negative figure for what
// it can hold. One from before that figure existed reports the card without
// it, which reads as zero. That runtime has to be replaced, which waiting does
// not do, so it is said apart.
func unmeasuredCards(snapshot *RuntimeSnapshot) string {
	if snapshot != nil {
		for _, accelerator := range snapshot.Accelerators {
			if accelerator.HBMTotalBytes > 0 && accelerator.HBMUsableBytes == 0 {
				return "its runtime does not report what its cards can hold, as one older than the controller does not"
			}
		}
	}
	return "its cards could not be measured"
}

// models is the engine list of a snapshot that may be missing.
func (s *RuntimeSnapshot) models() []RuntimeSnapshotModel {
	if s == nil {
		return nil
	}
	return s.Models
}

// wakeAsked reports whether a request asks to wake a claim's engine on a pod.
func wakeAsked(pod *corev1.Pod, claimName string) bool {
	if pod == nil {
		return false
	}
	_, asked := pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+claimName]
	return asked
}

// withoutAskedReserves returns a card's engines with the wake reserves that
// requests put back taken off again, and whether there were any.
func withoutAskedReserves(engines []engineOnPod) ([]engineOnPod, bool) {
	eased := make([]engineOnPod, len(engines))
	copy(eased, engines)
	found := false
	for i := range eased {
		if eased[i].wakeReserveAsked {
			eased[i].withoutWakeReserve = true
			eased[i].wakeReserveAsked = false
			found = true
		}
	}
	return eased, found
}
