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
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// GetPod retrieves a Pod object by name from the cache
// Parameters:
//
//	podName: Name of the pod to retrieve
//	podNamespace: Namespace of the pod to retrieve
//
// Returns:
//
//	*v1.Pod: The found Pod object
//	error: Error if pod doesn't exist
func (c *Store) GetPod(podName, podNamespace string) (*v1.Pod, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return nil, fmt.Errorf("key does not exist in the cache: %s", key)
	}

	return metaPod.Pod, nil
}

// ListPods returns all cached Pod objects
// Do not call this directly, for debug purpose and less efficient.
// Returns:
//
//	[]*v1.Pod: Slice of Pod objects
func (c *Store) ListPods() []*v1.Pod {
	pods := make([]*v1.Pod, 0, c.metaPods.Len())
	c.metaPods.Range(func(_ string, metaPod *Pod) bool {
		pods = append(pods, metaPod.Pod)
		return true
	})
	return pods
}

// ListPodsByModel gets Pods associated with a specific model
// Parameters:
//
//	modelName: Name of the model to query
//
// Returns:
//
//	types.PodList: PodArray wrapper for a slice of Pod objects
//	error: Error if model doesn't exist
func (c *Store) ListPodsByModel(modelName string) (types.PodList, error) {
	meta, ok := c.metaModels.Load(modelName)
	if !ok {
		return nil, fmt.Errorf("model does not exist in the cache: %s", modelName)
	}

	return meta.Pods.Array(), nil
}

// ListModels returns all cached model names
// Returns:
//
//	[]string: Slice of model names
func (c *Store) ListModels() []string {
	return c.metaModels.Keys()
}

// ListModelsWithReadyPods returns names with at least one ready, addressable,
// non-terminating, non-draining Pod. The cache lock keeps model membership and
// Pod updates in the same snapshot while the list is built.
func (c *Store) ListModelsWithReadyPods() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	models := make([]string, 0, c.metaModels.Len())
	c.metaModels.Range(func(name string, meta *Model) bool {
		for _, pod := range meta.Pods.Array().All() {
			if pod != nil && utils.FilterReadyPod(pod) {
				models = append(models, name)
				break
			}
		}
		return true
	})
	sort.Strings(models)
	return models
}

// HasModel checks if a model exists in the cache
// Parameters:
//
//	modelName: Name of the model to check
//
// Returns:
//
//	bool: True if model exists
func (c *Store) HasModel(modelName string) bool {
	_, ok := c.metaModels.Load(modelName)

	return ok
}

// ListModelsByPod gets models associated with a specific Pod
// Parameters:
//
//	podName: Name of the Pod to query
//	podNamespace: Namespace of the Pod to query
//
// Returns:
//
//	[]string: Slice of model names
//	error: Error if Pod doesn't exist
func (c *Store) ListModelsByPod(podName, podNamespace string) ([]string, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return nil, fmt.Errorf("key does not exist in the cache: %s", key)
	}

	return metaPod.Models.Array(), nil
}

// ModelBaseModel returns the base-model name a LoRA adapter is attached to.
func (c *Store) ModelBaseModel(modelName string) (string, bool) {
	meta, ok := c.metaModels.Load(modelName)
	if !ok || meta.BaseModel == "" {
		return "", false
	}
	return meta.BaseModel, true
}

// GetMetricValueByPod retrieves metric value for a Pod
// Parameters:
//
//	podName: Name of the Pod
//	podNamespace: Namespace of the Pod
//	metricName: Name of the metric
//
// Returns:
//
//	metrics.MetricValue: The metric value
//	error: Error if Pod or metric doesn't exist
func (c *Store) GetMetricValueByPod(podName, podNamespace, metricName string) (metrics.MetricValue, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return nil, fmt.Errorf("key does not exist in the cache: %s", key)
	}

	return c.getPodMetricImpl(podName, &metaPod.Metrics, metricName)
}

// GetMetricValueByPodModel retrieves metric value for Pod-Model combination
// Parameters:
//
//	podName: Name of the Pod
//	podNamespace: Namespace of the Pod
//	modelName: Name of the model
//	metricName: Name of the metric
//
// Returns:
//
//	metrics.MetricValue: The metric value
//	error: Error if Pod, model or metric doesn't exist
func (c *Store) GetMetricValueByPodModel(podName, podNamespace, modelName string, metricName string) (metrics.MetricValue, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return nil, fmt.Errorf("key does not exist in the cache: %s", key)
	}

	return c.getPodMetricImpl(podName, &metaPod.ModelMetrics, c.getPodModelMetricName(modelName, metricName))
}

// AdmitPodRunningRequest is the hard-cap counterpart of GetPodRunningRequests: instead of
// just reading the live cross-gateway running-request count, it uses Redis to atomically
// check that count against limit and, only if still under it, includes this request's own
// contribution in the count from this call onward (see admitRunningRequest). A plain read
// followed by a later, separate increment cannot enforce a hard cap -- concurrent
// requests can all observe the same pre-increment count and all be admitted regardless of
// how tight limit is.
//
// Before the Redis round trip, this gateway reserves one slot on the pod's local
// running-requests counter (reserveLocalRunningSlot). The claim is the admission decision
// under a Redis outage -- where admitRunningRequest falls back to localRunning alone and
// cannot see the script's increment (if any) that a concurrent caller committed -- and it
// also raises a pending admission above everything else the fallback could miss: until
// request tracking (addPodStats) runs for this request, the Redis sum and the local
// counter both lag the slots this gateway has committed, and the claim is what closes
// that window. Concurrency-safe by construction: the CAS either claims the free slot or
// reports the pod at cap. An admission the script itself later rejects (another gateway
// took the last slot between claim and script) rolls the claim back.
//
// On success, ReplicaInflightReserved and ReplicaInflightReservedGen are stamped on
// routingCtx (the gateway additionally records counted as ReplicaInflightAdmitted): they
// tell addPodStats to skip its local increment, which reserveLocalRunningSlot's claim on
// the same counter makes redundant, so completion's decrement still pairs exactly once --
// same shape as ReplicaInflightAdmitted's Redis-increment skip. Passing a nil routingCtx
// (direct-cache callers) skips the stamping and the reservation alike: only the stamped
// context drives an unclaim (donePodStats's pairing, and ReleasePodRunningRequest is for
// a request that reached routing), so a claim on behalf of an untracked request would
// strand one slot on the pod's counter per admit. The nil-ctx path keeps the
// pre-reservation behavior: it reads the live local count as the script's floor and
// decides from the script, or from that count alone when the round trip fails.
//
// admitted=false, err=nil means the pod is over its cap right now; err is non-nil only
// when the pod itself isn't in the cache (mirrors GetPodRunningRequests), which the
// caller should treat as fail-open, same as GetPodRunningRequests's error case. For an
// admitted request, counted=true means request tracking skips its Redis increment and
// keeps the completion decrement: the admit script's +1 was confirmed or may have been
// applied (see admitRunningRequest). counted=false leaves the increment to normal request
// tracking.
func (c *Store) AdmitPodRunningRequest(podName, podNamespace string, limit int64, routingCtx *types.RoutingContext) (admitted, counted bool, err error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return false, false, fmt.Errorf("key does not exist in the cache: %s", key)
	}
	// localRunning doubles as the script's local floor (ARGV[5], pre-reservation value --
	// this request's own claim must not turn up twice: the script applies its own +1 for
	// it), and the claim itself is the fallback's admission decision, so its outcome gates
	// whether the Redis round trip is even worth starting. A nil routingCtx (direct-cache
	// callers) or a request that has already added its stats (queue router / SLO path)
	// skips the claim: their admission decision reads the live local count and nothing will
	// unclaim a second slot, so they keep the pre-reservation behavior.
	localRunning := int64(atomic.LoadInt32(&metaPod.runningRequests))
	claimed := false
	if routingCtx != nil && !routingCtx.StatsAdded() {
		mu := c.podStatsLockFor(key)
		mu.Lock()
		curMeta, ok := c.metaPods.Load(key)
		if !ok {
			mu.Unlock()
			return false, false, fmt.Errorf("key does not exist in the cache: %s", key)
		}
		metaPod = curMeta
		localRunning, claimed = reserveLocalRunningSlot(metaPod, limit)
		mu.Unlock()
		if !claimed {
			return false, false, nil
		}
	}
	admitted, counted, _ = c.admitRunningRequest(podNamespace, podName, limit, localRunning)
	if !admitted {
		// The sum was already at (or over) the limit when the script ran: no slot for this
		// request, undo the claim. counted is false here by construction (a rejection
		// never applied the script's +1), so no Redis-side unwind pairs with it.
		if claimed {
			mu := c.podStatsLockFor(key)
			mu.Lock()
			if curMeta, ok := c.metaPods.Load(key); ok && sameStatsGeneration(metaPod.statsGeneration, curMeta.statsGeneration) {
				releaseLocalRunningSlot(curMeta)
			} else if snap, ok := c.recentlyDeletedPods.Load(key); ok &&
				sameStatsGeneration(metaPod.statsGeneration, snap.statsGeneration) {
				releaseLocalRunningCounter(&snap.runningRequests)
			}
			mu.Unlock()
		}
		return false, false, nil
	}
	if claimed {
		routingCtx.ReplicaInflightReserved = true
		routingCtx.ReplicaInflightReservedGen = metaPod.statsGeneration
	}
	return admitted, counted, nil
}

// ReleasePodRunningRequest removes the +1 a counted AdmitPodRunningRequest applied, for a
// request that never reached request tracking, whose completion decrement would have.
// If a local admission slot was claimed on the pod's running-requests counter (reservedGen != 0),
// that reservation is unclaimed as well.
func (c *Store) ReleasePodRunningRequest(podName, podNamespace string, counted bool, reservedGen int64) {
	if reservedGen != 0 {
		key := utils.GeneratePodKey(podNamespace, podName)
		mu := c.podStatsLockFor(key)
		mu.Lock()
		if metaPod, ok := c.metaPods.Load(key); ok {
			if sameStatsGeneration(reservedGen, metaPod.statsGeneration) {
				releaseLocalRunningSlot(metaPod)
			}
		} else if snap, ok := c.recentlyDeletedPods.Load(key); ok {
			if sameStatsGeneration(reservedGen, snap.statsGeneration) {
				releaseLocalRunningCounter(&snap.runningRequests)
			}
		}
		mu.Unlock()
	}
	if counted {
		c.decrPodRunningRequests(podNamespace, podName)
	}
}

// realtimeRunningRequests is the live cross-gateway running-request total for one pod.
// Prefer GetPodRunningRequests / GetPodsRunningRequests at call sites; this is the inner
// Redis-or-local read they share (see readPodRunningRequests in cache_running_requests.go).
//
// Fallback to pod.runningRequests when Redis is nil, the read fails, or the hash is
// missing (never routed, or idle past runningRequestsTTL). A hash that exists and sums
// to 0 (every other contributor is a now-dead gateway) is a real 0, not a fallback --
// readPodRunningRequests overlays this gateway's own hash field with this same
// pod.runningRequests atomic before summing, so that self-contribution is never stale.
func (c *Store) realtimeRunningRequests(pod *Pod) int64 {
	if count, ok := c.readPodRunningRequests(pod.Namespace, pod.Name); ok {
		return count
	}
	return int64(atomic.LoadInt32(&pod.runningRequests))
}

// GetPodRunningRequests is the single-pod load / inflight API.
//
// Use for: MODEL_REPLICA_REQUESTS_INFLIGHT on one pod, logging, any path that already
// has one pod. For a ready-pod list, call GetPodsRunningRequests -- looping this is N
// Redis round trips.
//
// Do not use GetMetricValueByPod(..., RealtimeNumRequestsRunning) for routing or inflight.
// That slot is a periodically synced cache of this same count and is overwritten by this
// gateway's local atomic on request start/end (cache_trace.go). Between scrape ticks it
// is this gateway's local view, not the live cross-gateway total.
//
// Source: Redis hash summed over currently-live gateways (cache_running_requests.go).
// Crashed-gateway leftovers age out with runningRequestsLivenessWindow; they do not leak.
// Fallback: this gateway's pod.runningRequests atomic. Error if the pod is not in metaPods.
func (c *Store) GetPodRunningRequests(podName, podNamespace string) (int64, error) {
	key := utils.GeneratePodKey(podNamespace, podName)
	metaPod, ok := c.metaPods.Load(key)
	if !ok {
		return 0, fmt.Errorf("key does not exist in the cache: %s", key)
	}
	return c.realtimeRunningRequests(metaPod), nil
}

// GetPodsRunningRequests is GetPodRunningRequests for a pod list in one Redis pipeline.
//
// Use from routers that score every ready pod (least-request, load-balance, prefix-cache
// tie-break) and from the MODEL_REPLICA_REQUESTS_INFLIGHT saturation filter.
// Do not range pods calling GetPodRunningRequests.
//
// Return map is keyed by utils.GeneratePodKey(namespace, name). The local-atomic fallback
// is already applied. A missing key means the pod was nil or not in metaPods -- treat as 0.
// Do not treat a missing key as "read the local counter again."
func (c *Store) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	return c.runningRequestsWithLocalFallback(pods, c.readPodsRunningRequests(pods)), nil
}

// runningRequestsWithLocalFallback keys live by pod and fills in this gateway's
// local atomic for every pod live has no count for.
func (c *Store) runningRequestsWithLocalFallback(pods []*v1.Pod, live map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		if count, ok := live[podKey]; ok {
			result[podKey] = count
			continue
		}
		metaPod, ok := c.metaPods.Load(podKey)
		if !ok {
			continue
		}
		result[podKey] = int64(atomic.LoadInt32(&metaPod.runningRequests))
	}
	return result
}

// GetPodsReadySince implements PodReadySinceProvider. It is a local read with no Redis: ramp
// state is per gateway, since each gateway's informer sees the same Ready event at about the
// same time.
func (c *Store) GetPodsReadySince(pods []*v1.Pod) (map[string]int64, error) {
	result := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		metaPod, ok := c.metaPods.Load(podKey)
		if !ok {
			continue
		}
		result[podKey] = metaPod.readySince.Load()
	}
	return result, nil
}

// LatestPodReadySince implements PodReadySinceProvider: one atomic load of the running max kept
// by addPodLocked, deliberately not a scan of c.metaPods.
func (c *Store) LatestPodReadySince() int64 {
	return c.lastPodReadySince.Load()
}

var _ PodReadySinceProvider = (*Store)(nil)

// AddRequestCount tracks new request initiation.
// If ctx is provided,  AddRequestCount can be called multiple times for the same request.
//
// Parameters:
//
//	ctx: Routing context
//	requestID: Unique request identifier
//	modelName: Model handling the request
//
// Returns:
//
//	int64: Trace term identifier
func (c *Store) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) (traceTerm int64) {
	for _, tracker := range c.requestTrackers {
		// ignore traceTerm
		tracker.AddRequestCount(ctx, requestID, modelName)
	}
	// Current implementation assumes AddRequestCount() will not be called concurrently.
	// TODO: Implment "wait for trace term" logic if AddRequestCount() is called concurrently.
	if ctx == nil || ctx.CanAddTrace() {
		if c.enableTracing {
			success := false
			for {
				trace := c.getRequestTrace(modelName)
				// TODO: use non-empty key if we have output prediction to decide buckets early.
				if traceTerm, success = trace.AddRequest(requestID, ""); success {
					if ctx != nil {
						atomic.StoreInt64(&ctx.TraceTerm, traceTerm)
					}
					break
				}
				// In case AddRequest return false, it has been recycled and we want to retry.
			}
		}

		meta, ok := c.metaModels.Load(modelName)
		if ok {
			atomic.AddInt32(&meta.pendingRequests, 1)
		}
	}

	if ctx == nil {
		return traceTerm
	}
	// The queue router calls AddRequestCount concurrently for the same request. Pod stats
	// are added once, and a caller that loses CanAddStats waits in WithStatsUpdate until
	// the winner's addPodStats has returned.
	ctx.WithStatsUpdate(func() {
		if ctx.HasRouted() && ctx.CanAddStats() {
			c.addPodStats(ctx, requestID, modelName)
		}
	})
	// The trace term is not synchronized that way yet: a concurrent caller may read it
	// before the winner of CanAddTrace stores it.
	// TODO: Implment "wait for trace term" logic if AddRequestCount() is called concurrently.
	return atomic.LoadInt64(&ctx.TraceTerm)
}

// DoneRequestCount completes request tracking
// Parameters:
//
//	 ctx: Routing context (can be nil for early-cancelled requests)
//		requestID: Unique request identifier
//		modelName: Model handling the request
//		traceTerm: Trace term identifier
func (c *Store) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	// Defensive check: ctx can be nil when request is cancelled before routing completes
	// In this case, we still need to clean up model-level state but skip pod-level tracking
	for _, tracker := range c.requestTrackers {
		// ignore traceTerm
		tracker.DoneRequestCount(ctx, requestID, modelName, traceTerm)
	}
	if ctx == nil || ctx.CanDoneStats() {
		c.donePodStats(ctx, requestID, modelName, 0)
	}

	meta, ok := c.metaModels.Load(modelName)
	if ok && (ctx == nil || ctx.CanDoneTrace()) {
		atomic.AddInt32(&meta.pendingRequests, -1)
	}

	// DoneRequest only works for current term, no need to retry.
	if c.enableTracing {
		c.getRequestTrace(modelName).DoneRequest(requestID, traceTerm)
	}
}

// DoneRequestTrace completes request tracing
// Parameters:
//
//	ctx: Routing context
//	requestID: Unique request identifier
//	modelName: Model handling the request
//	inputTokens: Input tokens count
//	outputTokens: Output tokens count
//	traceTerm: Trace term identifier
func (c *Store) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens, outputTokens, traceTerm int64) {
	for _, tracker := range c.requestTrackers {
		// ignore traceTerm
		tracker.DoneRequestTrace(ctx, requestID, modelName, inputTokens, outputTokens, traceTerm)
	}

	if ctx == nil || ctx.CanDoneStats() {
		c.donePodStats(ctx, requestID, modelName, outputTokens)
	}

	meta, ok := c.metaModels.Load(modelName)
	if ok && (ctx == nil || ctx.CanDoneTrace()) {
		atomic.AddInt32(&meta.pendingRequests, -1)
	}

	if c.enableTracing {
		var traceKey string
		for {
			trace := c.getRequestTrace(modelName)
			if traceKey, ok = trace.DoneRequestTrace(requestID, inputTokens, outputTokens, traceKey, traceTerm); ok {
				break
			}
			// In case DoneRequest return false, it has been recycled and we want to retry.
		}
		klog.V(5).Infof("inputTokens: %v, outputTokens: %v, trace key: %s", inputTokens, outputTokens, traceKey)
	}

	// Only update OutputPredictor if model metadata and predictor exist
	if meta != nil && meta.OutputPredictor != nil {
		meta.OutputPredictor.AddTrace(int(inputTokens), int(outputTokens), 1)
	}
}

// AddSubscriber registers new metric subscriber
// Parameters:
//
//	subscriber: Metric subscriber implementation
func (c *Store) AddSubscriber(subscriber metrics.MetricSubscriber) {
	c.subscribers = append(c.subscribers, subscriber)
	c.aggregateMetrics()
}

func (c *Store) UpdateModelProfile(key string, profile *ModelGPUProfile, force bool) {
	existed, ok := c.deploymentProfiles.Load(key)
	if profile == nil && ok {
		c.deploymentProfiles.Delete(key)
	} else if force || !ok || existed.Created < profile.Created {
		c.deploymentProfiles.Store(key, profile)
	}
}

func (c *Store) GetModelProfileByPod(pod *v1.Pod, modelName string) (*ModelGPUProfile, error) {
	deploymentName := utils.DeploymentNameFromPod(pod)
	if len(deploymentName) == 0 {
		// Handle the case where the deployment name is not found (e.g., log an error)
		return nil, fmt.Errorf("deployment name not found on pod %s", pod.Name)
	}

	key := ModelGPUProfileKey(modelName, deploymentName)
	profile, ok := c.deploymentProfiles.Load(key)
	if !ok {
		return nil, MissingProfileError{
			CacheError: ErrorMissingProfile,
			ProfileKey: key,
		}
	}

	return profile, nil
}

func (c *Store) GetModelProfileByDeploymentName(deploymentName string, modelName string) (*ModelGPUProfile, error) {
	key := ModelGPUProfileKey(modelName, deploymentName)
	profile, ok := c.deploymentProfiles.Load(key)
	if !ok {
		return nil, fmt.Errorf("profile not available for %s", key)
	}

	return profile, nil
}

func (c *Store) GetOutputPredictor(modelName string) (types.OutputPredictor, error) {
	if model, ok := c.metaModels.Load(modelName); ok {
		return model.OutputPredictor, nil
	}
	return nil, fmt.Errorf("model does not exist in the cache: %s", modelName)
}

func (c *Store) GetRouter(ctx *types.RoutingContext) (types.Router, error) {
	if model, ok := c.metaModels.Load(ctx.Model); !ok {
		return nil, fmt.Errorf("model does not exist in the cache: %s", ctx.Model)
	} else if model.QueueRouter == nil {
		return nil, fmt.Errorf("queue router not available for model: %s", ctx.Model)
	} else {
		return model.QueueRouter, nil
	}
}

func (c *Store) RegisterRequestTracker(tracker RequestTracker) {
	c.requestTrackers = append(c.requestTrackers, tracker)
}
