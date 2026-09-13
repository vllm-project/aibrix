/*
Copyright 2024 The Aibrix Team.

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
package cache

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	expireWriteRequestTraceIntervalInMins = 10
	traceLogInterval                      = 1 * time.Second

	// podStatsLockStripes sizes Store.podStatsMu -- see podStatsLockFor.
	podStatsLockStripes = 256
)

// podStatsLockFor returns the stripe lock guarding podKey's running-request counter
// (and its deletedPodSnapshot equivalent) against concurrent mutation. addPodStats and
// donePodStats hold it while resolving which *Pod object (or deletedPodSnapshot)
// currently represents podKey and mutating its counter; deletePodLocked and
// addPodLocked (informers.go) hold it around the delete/re-add snapshot handoff for
// the same key. Without a shared lock, those two sides can interleave: a completing
// request can resolve "the live pod is object O1" and then, after a concurrent
// delete+re-add swaps in O2, apply its decrement to the now-orphaned O1 -- silently
// losing it (or, symmetrically, applying it to the wrong generation and undercounting
// O2). A fixed-size stripe (indexed by a hash of podKey) is used instead of a lock per
// key so the lock set doesn't grow unbounded as distinct pod keys accumulate over the
// process lifetime; the tradeoff is harmless false-sharing between unrelated pod keys
// that happen to hash to the same stripe.
func (c *Store) podStatsLockFor(podKey string) *sync.Mutex {
	return &c.podStatsMu[fnv32a(podKey)%podStatsLockStripes]
}

// fnv32a is a small, non-allocating FNV-1a string hash used only to pick a lock
// stripe in podStatsLockFor -- not for anything requiring collision resistance.
func fnv32a(s string) uint32 {
	const offset32 = 2166136261
	const prime32 = 16777619
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}

// clampedRunningRequestDecrements counts how many times decrementClamped actually hit
// its floor -- i.e. an attempted decrement past zero. This is test/observability-only:
// production code never reads it. It exists because the clamp itself makes the
// counter's own value an unreliable signal in a test: an extra decrement that gets
// floored to 0 is indistinguishable from a counter that correctly settled at 0, so
// asserting "the counter is 0" cannot catch the failure mode the clamp guards
// against. Tests should instead snapshot this before/after a concurrent run and
// assert the delta is 0.
var clampedRunningRequestDecrements int64

// decrementClamped atomically decrements *counter by 1, clamping at 0, and returns the
// value after the decrement. A defense-in-depth floor: even if some other bug ever
// slips an extra decrement past podStatsLockFor's mutual exclusion, a real running-
// request count should never be reported as negative. When the floor actually fires
// (the counter was already <= 0), a warning is logged with podName/requestID so the
// extra decrement is visible rather than silent.
//
// This is a genuine lock-free floor, not merely a floor under the caller's lock: the
// old <= 0 case never blindly Stores 0 (observing 0 and unconditionally writing 0 back
// could stomp a concurrent AddInt32(+1) landing in between the two), it either does
// nothing (old == 0: already at the floor) or CASes up from a negative old value,
// which -- like the normal decrement path -- only succeeds if the value hasn't changed
// since the Load, so a concurrent increment always wins the race rather than being
// silently overwritten.
func decrementClamped(counter *int32, podName, requestID string) int32 {
	for {
		old := atomic.LoadInt32(counter)
		if old <= 0 {
			if old < 0 && !atomic.CompareAndSwapInt32(counter, old, 0) {
				continue
			}
			atomic.AddInt64(&clampedRunningRequestDecrements, 1)
			klog.Warningf("running-request count would go negative, clamped to 0, pod: %s, requestID: %s, observed: %d",
				podName, requestID, old)
			return 0
		}
		if atomic.CompareAndSwapInt32(counter, old, old-1) {
			return old - 1
		}
	}
}

// sameStatsGeneration reports whether two cache-pod generations are the same
// live identity. 0 is never issued (see nextStatsGeneration) and means
// uninitialized, so it must not match.
func sameStatsGeneration(a, b int64) bool {
	return a != 0 && a == b
}

// resolvePodOrSnapshotLocked resolves whichever object -- a live *Pod (returned as
// pod, with snap nil), or a pending deletedPodSnapshot (returned as snap, with pod
// nil) -- currently represents podKey, given staleMetaPod: a possibly-outdated *Pod
// reference captured before this lock was (re-)acquired (e.g. before a slow call like
// pendingLoadProvider.GetConsumption ran unlocked, or before donePodStats re-resolves
// the object addPodStats originally saw).
//
// If podKey currently maps to a *different*, live object with the same
// statsGeneration, that live object is returned -- the common case: the pod flapped
// via a delete/re-add cycle (deletePodLocked/addPodLocked in informers.go, which hold
// the same podStatsLockFor(podKey) lock) and addPodLocked resumed this generation
// onto a new *Pod. Matching on generation (not IP) is required so a same-IP re-add
// after recentlyDeletedPodGracePeriod -- which deliberately starts a new generation
// at zero -- cannot inherit stale Add/Done from the previous one.
// If generations don't match, or podKey is absent with no matching snapshot either,
// pod is staleMetaPod itself: the pod was genuinely recreated, or the grace period
// expired, and stale bookkeeping must not bleed into that new backend's fresh
// counters. Applying a decrement/delta to that returned stale object is a
// deliberate, harmless no-op: it's orphaned (unreachable via metaPods), so nothing
// else ever reads it again.
//
// Caller must hold podStatsLockFor(podKey).
func (c *Store) resolvePodOrSnapshotLocked(podKey string, staleMetaPod *Pod) (pod *Pod, snap *deletedPodSnapshot) {
	if current, ok := c.metaPods.Load(podKey); ok {
		if current != staleMetaPod && sameStatsGeneration(current.statsGeneration, staleMetaPod.statsGeneration) {
			return current, nil
		}
		return staleMetaPod, nil
	}
	if s, ok := c.recentlyDeletedPods.Load(podKey); ok && sameStatsGeneration(s.statsGeneration, staleMetaPod.statsGeneration) {
		return nil, s
	}
	return staleMetaPod, nil
}

type podStatsRecord struct {
	pod         *Pod
	port        int
	pendingLoad float64
	// ctx is this attempt's owner. Used to delete the attempt-scoped primary
	// key from a nil-ctx donePodStats (which only has the requestID alias).
	// nil is a valid value and never matches a real ctx.
	ctx *types.RoutingContext
	// consumed is CAS'd from 0 to 1 when the record is taken for decrement,
	// so a concurrent ctx-Done and nil-ctx Done of the same attempt cannot
	// both apply the increment.
	consumed int32
}

func (r *podStatsRecord) take() bool {
	return atomic.CompareAndSwapInt32(&r.consumed, 0, 1)
}

// podStatsKey is a nil-ctx alias keyed by (modelName, requestID) alone. The
// primary record lives at podStatsAttemptKey; this alias lets DoneRequestCount
// with ctx=nil still find one in-flight attempt for the requestID (the first
// one that stored). LoadOrStore so a retry cannot clobber the original.
func podStatsKey(modelName, requestID string) string {
	return modelName + "\x00" + requestID
}

// podStatsAttemptKey identifies one specific in-flight routing attempt via its
// RoutingContext's pointer identity, which is stable for exactly that attempt's
// lifetime (one RoutingContext per Process() call, returned to the pool only
// after donePodStats has run for it -- see the ordering in Process()'s
// deferred cleanup). This is the primary podStats map key whenever ctx != nil.
func podStatsAttemptKey(ctx *types.RoutingContext, modelName, requestID string) string {
	return fmt.Sprintf("%s\x00%s\x00%p", modelName, requestID, ctx)
}

// storePodStats writes the attempt-scoped primary key and, if vacant, the
// requestID alias used by nil-ctx donePodStats. Both keys point at the same
// record so CompareAndDelete can drop the alias without stealing another
// attempt.
func (c *Store) storePodStats(ctx *types.RoutingContext, modelName, requestID string, podStats *podStatsRecord) {
	if ctx != nil {
		c.podStats.Store(podStatsAttemptKey(ctx, modelName, requestID), podStats)
	}
	c.podStats.LoadOrStore(podStatsKey(modelName, requestID), podStats)
}

// takePodStats removes this attempt's podStats record. ctx != nil deletes the
// attempt key first (no put-back of someone else's entry). ctx == nil deletes
// the requestID alias and the attempt key of whichever record it held. take()
// ensures only one of a concurrent ctx-Done and nil-ctx Done actually
// decrements.
func (c *Store) takePodStats(ctx *types.RoutingContext, modelName, requestID string) (*podStatsRecord, bool) {
	var (
		podStats *podStatsRecord
		ok       bool
	)
	if ctx != nil {
		podStats, ok = c.podStats.LoadAndDelete(podStatsAttemptKey(ctx, modelName, requestID))
		if ok {
			c.podStats.CompareAndDelete(podStatsKey(modelName, requestID), podStats)
		}
	} else {
		podStats, ok = c.podStats.LoadAndDelete(podStatsKey(modelName, requestID))
		if ok && podStats.ctx != nil {
			c.podStats.CompareAndDelete(podStatsAttemptKey(podStats.ctx, modelName, requestID), podStats)
		}
	}
	if !ok || !podStats.take() {
		return nil, false
	}
	return podStats, true
}

func (c *Store) getRequestTrace(modelName string) *RequestTrace {
	trace := NewRequestTrace(time.Now().UnixNano())
	newer, loaded := c.requestTrace.LoadOrStore(modelName, trace)
	if loaded {
		trace.Recycle()
	} else {
		atomic.AddInt32(&c.numRequestsTraces, 1)
	}
	return newer
}

func (c *Store) addPodStats(ctx *types.RoutingContext, requestID string, modelName string) {
	if !ctx.HasRouted() {
		return
	}
	pod := ctx.TargetPod()
	port := ctx.TargetPort()
	podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)

	// See podStatsLockFor: held across resolving metaPod and mutating its running-
	// request counter so this can't interleave with a concurrent delete/re-add of the
	// same pod key.
	mu := c.podStatsLockFor(podKey)
	mu.Lock()
	metaPod, ok := c.metaPods.Load(podKey)
	var snap *deletedPodSnapshot
	if !ok {
		// Symmetric with donePodStats: a request can start inside the
		// delete/re-add window (updatePod releases the stripe lock between
		// deletePodLocked and addPodLocked). Increment the matching-IP
		// snapshot in place so the eventual resume includes this request
		// rather than dropping it for its whole lifetime.
		if pod.Status.PodIP != "" {
			if s, loaded := c.recentlyDeletedPods.Load(podKey); loaded && s.podIP == pod.Status.PodIP && s.pod != nil {
				snap = s
				metaPod = s.pod
				ok = true
			}
		}
		if !ok {
			mu.Unlock()
			klog.Warningf("can't find routing pod: %s, requestID: %s", pod.Name, requestID)
			return
		}
	}
	podStats := &podStatsRecord{pod: metaPod, port: port, ctx: ctx}

	// Update running requests
	var requests int32
	if snap != nil {
		requests = atomic.AddInt32(&snap.runningRequests, 1)
	} else {
		requests = atomic.AddInt32(&metaPod.runningRequests, 1)
		metricName := metrics.RealtimeNumRequestsRunning
		if port > 0 {
			metricName = metricName + "/" + strconv.Itoa(port)
		}
		if err := c.updatePodRecord(metaPod, "", metricName, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: float64(requests)}); err != nil {
			klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s, err: %v", metrics.RealtimeNumRequestsRunning, metaPod.Name, requestID, err)
		}
	}
	mu.Unlock()

	// Update pending load. GetConsumption runs unlocked -- it can be slow (e.g. a GPU
	// profile lookup) or take other locks, and stripe locks should stay short -- so
	// only the resulting delta is computed here. The lock is then re-acquired and
	// resolvePodOrSnapshotLocked re-resolves the current target from scratch rather
	// than trusting metaPod, which may have gone stale (a delete/re-add, or even a
	// genuine recreation, could have landed while GetConsumption was running): see
	// resolvePodOrSnapshotLocked's doc comment for why applying to a captured-before-
	// unlock pointer would risk landing on an orphaned object.
	var utilization float64
	if c.pendingLoadProvider != nil {
		var err error
		ctx.PendingLoad, err = c.pendingLoadProvider.GetConsumption(ctx, pod)
		if err == nil {
			podStats.pendingLoad = ctx.PendingLoad
			mu.Lock()
			target, snap := c.resolvePodOrSnapshotLocked(podKey, metaPod)
			if snap != nil {
				snap.pendingLoadUtilization.Add(ctx.PendingLoad)
				mu.Unlock()
			} else {
				utilization = target.pendingLoadUtilization.Add(ctx.PendingLoad)
				mu.Unlock()
				if c.updatePodRecord(target, "", metrics.RealtimeNormalizedPendings, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: utilization}) != nil {
					klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s, err: %v", metrics.RealtimeNormalizedPendings, metaPod.Name, requestID, err)
				}
			}
		} else if !IsError(err, ErrorMissingProfile) { // ErrorMissingProfile is not considered as an error here and should be reported where the profile is essential.
			klog.V(4).Infof("error on track request load consumption: %v", err)
		}
	}
	// Always store under the attempt key so two in-flight attempts that share
	// a requestID cannot clobber each other, and so donePodStats never has to
	// LoadAndDelete someone else's entry and put it back (a race that leaks
	// the original increment). The requestID key is only a nil-ctx alias.
	c.storePodStats(ctx, modelName, requestID, podStats)

	if metaPod.CanLogPodTrace(5) {
		klog.V(4).InfoS("pod stats updated (addPodStats).", "pod", metaPod.Name, "requestID", ctx.RequestID, "running_requests", requests, "pending_util", utilization, "pending_load", ctx.PendingLoad)
	}
}

func (c *Store) donePodStats(ctx *types.RoutingContext, requestID string, modelName string) {
	podStats, ok := c.takePodStats(ctx, modelName, requestID)
	if !ok {
		if ctx != nil {
			pod := ctx.TargetPod()
			if pod != nil {
				klog.Warningf("can't find routing pod stats: %s, requestID: %s", pod.Name, requestID)
			}
		}
		return
	}
	metaPod := podStats.pod
	port := podStats.port

	// A pod key that was deleted and quickly re-added (a transient health-check
	// flap -- see deletedPodSnapshot/addPodLocked in informers.go) gets a
	// brand-new *Pod object with counters seeded from the old one's snapshot.
	// addPodStats above captured the OLD object's pointer for this request
	// before that flap happened, so without a redirect, this decrement would
	// land on the now-orphaned old object and silently vanish -- permanently
	// inflating the new object's count by one for every request that was in
	// flight at flap time.
	//
	// Two cases, both gated on matching statsGeneration (a same-named pod
	// genuinely recreated, or re-added after recentlyDeletedPodGracePeriod,
	// starts a new generation and must not have this stale completion bleed
	// into it):
	//
	//   - The pod key is live again: redirect straight to the current entry.
	//   - The pod key is still absent (this request is completing *inside*
	//     the delete/re-add gap itself, before any re-add has happened):
	//     decrement the pending deletedPodSnapshot in place instead, so the
	//     eventual re-add resumes from a count that already reflects this
	//     completion rather than one that still includes it.
	//
	// podStatsLockFor(podKey) is held across resolving which object/snapshot is
	// current AND mutating BOTH its running-request counter and its pending-load
	// utilization, so a concurrent delete/re-add (deletePodLocked/addPodLocked,
	// which take the same lock) can't land mid-resolve and misdirect or lose either
	// -- see podStatsLockFor's and resolvePodOrSnapshotLocked's doc comments.
	// Nothing in this critical section is slow or reentrant (podStats.pendingLoad
	// was already computed back in addPodStats; only QueueRouter.Route below might
	// be either, so that alone stays outside).
	podKey := utils.GeneratePodKey(metaPod.Namespace, metaPod.Name)
	mu := c.podStatsLockFor(podKey)
	mu.Lock()
	target, snap := c.resolvePodOrSnapshotLocked(podKey, metaPod)
	if snap != nil {
		decrementClamped(&snap.runningRequests, metaPod.Name, requestID)
		atomic.AddInt64(&snap.completedRequests, 1)
		if podStats.pendingLoad != 0.0 && c.pendingLoadProvider != nil {
			snap.pendingLoadUtilization.Add(-podStats.pendingLoad)
		}
		mu.Unlock()
		if metaPod.CanLogPodTrace(5) {
			klog.V(4).InfoS("pod stats updated on a snapshot pending re-add (donePodStats).",
				"pod", metaPod.Name, "requestID", requestID, "pending_load", podStats.pendingLoad)
		}
		return
	}
	metaPod = target

	// Update running requests. Clamped at 0 (see decrementClamped) as a
	// defense-in-depth floor so a real count is never reported negative even if
	// some other bug slips an extra decrement past the lock above.
	requests := decrementClamped(&metaPod.runningRequests, metaPod.Name, requestID)
	atomic.AddInt64(&metaPod.completedRequests, 1)
	metricName := metrics.RealtimeNumRequestsRunning
	if port > 0 {
		metricName = metricName + "/" + strconv.Itoa(port)
	}
	if err := c.updatePodRecord(metaPod, modelName, metricName, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: float64(requests)}); err != nil {
		klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s", metrics.RealtimeNumRequestsRunning, metaPod.Name, requestID)
	}

	// Update pending load, still under the same lock as the resolve above (this is
	// just a cheap atomic float add, unlike GetConsumption in addPodStats, so there's
	// no cost to keeping it in the critical section).
	var utilization float64
	var notifyQueueRouter bool
	if podStats.pendingLoad != 0.0 && c.pendingLoadProvider != nil {
		utilization = metaPod.pendingLoadUtilization.Add(-podStats.pendingLoad)
		if err := c.updatePodRecord(metaPod, modelName, metrics.RealtimeNormalizedPendings, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: utilization}); err != nil {
			klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s", metrics.RealtimeNormalizedPendings, metaPod.Name, requestID)
		}
		notifyQueueRouter = utilization < c.pendingLoadProvider.Cap()
	}
	mu.Unlock()

	if notifyQueueRouter {
		// Notify queue router to try route with pending requests. Kept outside the
		// lock: Route may be slow or (via the router's own logic) reentrant.
		if metaModel, ok := c.metaModels.Load(modelName); ok && metaModel.QueueRouter != nil {
			// nolint: errcheck
			metaModel.QueueRouter.Route(nil, metaModel.Pods.Array())
		}
	}

	if metaPod.CanLogPodTrace(5) {
		klog.V(4).InfoS("pod stats updated (donePodStats).", "pod", metaPod.Name, "requestID", requestID, "running_requests", requests, "pending_util", utilization, "pending_load", podStats.pendingLoad)
	}
}

func (c *Store) writeRequestTraceToStorage(roundT int64) {
	// Save and reset trace context, atomicity is guaranteed.
	var requestTrace *utils.SyncMap[string, *RequestTrace]
	numTraces := atomic.LoadInt32(&c.numRequestsTraces)
	requestTrace, c.requestTrace = c.requestTrace, &utils.SyncMap[string, *RequestTrace]{}
	numResetTo := int32(0)
	// TODO: Adding a unit test here.
	for !atomic.CompareAndSwapInt32(&c.numRequestsTraces, numTraces, numResetTo) {
		// If new traces added to reset map, assert updatedNumTraces >= numTraces regardless duplication.
		updatedNumTraces := atomic.LoadInt32(&c.numRequestsTraces)
		numTraces, numResetTo = updatedNumTraces, updatedNumTraces-numTraces
	}

	requestTrace.Range(func(modelName string, trace *RequestTrace) bool {
		requestTrace.Store(modelName, nil) // Simply assign nil instead of delete

		trace.Lock()
		pending := 0
		queueing := 0
		if meta, loaded := c.metaModels.Load(modelName); loaded {
			pending = int(atomic.LoadInt32(&meta.pendingRequests))
			if meta.QueueRouter != nil {
				queueing = meta.QueueRouter.Len()
			}
		}
		traceMap := trace.ToMapLocked(pending, queueing)
		trace.RecycleLocked()
		trace.Unlock()

		value, err := sonic.Marshal(traceMap)
		if err != nil {
			klog.ErrorS(err, "error to marshall request trace for redis set")
			return true
		}

		key := fmt.Sprintf("aibrix:%v_request_trace_%v", modelName, roundT)
		if _, err = c.redisClient.Set(context.Background(), key, value, expireWriteRequestTraceIntervalInMins*time.Minute).Result(); err != nil {
			klog.Error(err)
		}
		return true
	})

	klog.V(5).Infof("writeRequestTraceWithKey: %v", roundT)
}

func (c *Store) DumpRequestTrace(modelName string) map[string]int {
	trace, ok := c.requestTrace.Load(modelName)
	if !ok {
		return nil
	} else {
		return trace.ToMap(0, 0)
	}
}
