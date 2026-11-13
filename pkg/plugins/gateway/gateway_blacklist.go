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

package gateway

import (
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/blacklist"
	"github.com/vllm-project/aibrix/pkg/types"
)

// Server-side glue for the pod blacklist. The breaker and its background
// sweeps live in the blacklist package; what stays here is what needs the
// Server's own state: the cache, the running-request totals and the
// PodLifecycleListener registration.

// startBlacklistSweeps starts the blacklist's background goroutines: pruning
// of untouched entries, the load-outlier sweep and the proactive probe sweep.
// When AIBRIX_POD_BLACKLIST_ENABLED is off (the default) it returns without
// starting any work, so a healthy fleet pays nothing for the feature.
func (s *Server) startBlacklistSweeps(stopCh <-chan struct{}) {
	if !blacklist.Enabled {
		klog.InfoS("pod blacklist disabled; set AIBRIX_POD_BLACKLIST_ENABLED=true to enable",
			"env", blacklist.EnvPodBlacklistEnabled)
		return
	}
	s.podBlacklist.StartSweep(stopCh)
	s.podBlacklist.StartLoadOutlierSweep(s.cache, s.podEngineQueueDepth, s.logPodUnBlacklisted, stopCh)
	s.podBlacklist.StartProbeSweep(s.cache, stopCh)
}

// registerPodLifecycle lets the cache tell the blacklist when a pod flaps back in.
// A cache that reports no pod lifecycle events simply does not implement
// cache.PodLifecycleRegistry. Skipped while the master switch is off.
func (s *Server) registerPodLifecycle() {
	if !blacklist.Enabled {
		return
	}
	if registry, ok := s.cache.(cache.PodLifecycleRegistry); ok {
		registry.RegisterPodLifecycleListener(s)
	}
}

// registerPodQuarantine lets the cache pause its metrics scrape and per-pod
// series for a pod the blacklist has quarantined (found dead by the probe
// sweep and blacklisted for an hour). A cache that does not do per-pod
// scraping simply does not implement cache.PodQuarantineRegistry. Skipped
// while the master switch is off.
func (s *Server) registerPodQuarantine() {
	if !blacklist.Enabled {
		return
	}
	if registry, ok := s.cache.(cache.PodQuarantineRegistry); ok {
		registry.SetPodQuarantineChecker(&s.podBlacklist)
	}
}

// unregisterFromCache undoes registerPodLifecycle and registerPodQuarantine when the
// server shuts down, so a cache that outlives it neither keeps it alive nor keeps
// asking its blacklist about pods. The quarantine slot is shared, so it is cleared
// only if it still holds this server's blacklist.
func (s *Server) unregisterFromCache() {
	if registry, ok := s.cache.(cache.PodLifecycleRegistry); ok {
		registry.UnregisterPodLifecycleListener(s)
	}
	if registry, ok := s.cache.(cache.PodQuarantineRegistry); ok {
		registry.ClearPodQuarantineChecker(&s.podBlacklist)
	}
}

// OnPodFlakyReAdd implements cache.PodLifecycleListener on *Server (registered via
// cache.PodLifecycleRegistry in NewServer), delegating to the pod blacklist. See
// blacklist.PodBlacklist.OnPodFlakyReAdd.
func (s *Server) OnPodFlakyReAdd(pod *v1.Pod) {
	s.podBlacklist.OnPodFlakyReAdd(pod)
}

// claimBlacklistProbe makes the request just routed the half-open probe of the pod it
// was routed to, if that pod is waiting for one (see blacklist.PodBlacklist.ClaimProbe).
// HandleRequestBody calls it once the request is certain to reach the picked pod. The
// routing filter only admits a pod as a candidate; claiming there would spend the probe
// on every candidate the router then passed over.
func (s *Server) claimBlacklistProbe(routingCtx *types.RoutingContext) {
	if routingCtx.HasRouted() {
		s.podBlacklist.ClaimProbe(routingCtx.TargetPod())
	}
}

// getGlobalRunningRequestsByPod is the live cross-gateway running-request count of
// pod (a Redis read when Redis is configured), 0 when the cache does not know the
// pod. It is for background surfaces such as the un-blacklist log line, not the
// request path.
func (s *Server) getGlobalRunningRequestsByPod(pod *v1.Pod) float64 {
	if pod == nil {
		return 0
	}
	count, err := s.cache.GetPodRunningRequests(pod.Name, pod.Namespace)
	if err != nil {
		return 0
	}
	return float64(count)
}

// podEngineTotal is the engine's own reported queue depth (num_requests_running +
// num_requests_waiting) for pod, summed over every model the pod serves. A missing
// metric reads as 0.
func (s *Server) podEngineTotal(pod *v1.Pod) float64 {
	if pod == nil {
		return 0
	}
	models, err := s.cache.ListModelsByPod(pod.Name, pod.Namespace)
	if err != nil {
		return 0
	}
	var total float64
	for _, model := range models {
		total += s.podEngineQueueDepth(pod, model)
	}
	return total
}

// logPodUnBlacklisted logs a pod's load-outlier un-blacklist transition together with
// the live cross-gateway running-request count and the engine's running+waiting queue
// depth, so an operator investigating why a pod tripped and recovered doesn't have to
// separately correlate them.
func (s *Server) logPodUnBlacklisted(pod *v1.Pod) {
	if pod == nil {
		return
	}
	const msg = "pod un-blacklisted: running+waiting request count has converged back to normal range"
	// Same pod/addr/model fields as the matching "blacklisted" line, so the two can be joined.
	fields := blacklist.PodLogFields(pod)
	klog.InfoS(msg, append(fields,
		"gateway_total", s.getGlobalRunningRequestsByPod(pod),
		"engine_total", s.podEngineTotal(pod))...)
}

// allBlacklistedLogInterval is the minimum gap between "every ready pod is blacklisted"
// lines for one model. selectTargetPod takes that branch on every request while a model is
// fully blacklisted, so an unthrottled line would flood the log during the very outage it
// reports.
const allBlacklistedLogInterval = 10 * time.Second

// allBlacklistedLogThrottle rate-limits that line per model and counts what it dropped.
// The zero value is ready to use.
type allBlacklistedLogThrottle struct {
	mu      sync.Mutex
	entries map[string]allBlacklistedLogEntry
}

type allBlacklistedLogEntry struct {
	last       time.Time
	suppressed int
}

// allow reports whether a line for model may be logged at now. When it may, suppressed is
// how many were dropped since the previous one.
func (t *allBlacklistedLogThrottle) allow(model string, now time.Time) (suppressed int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]allBlacklistedLogEntry)
	}
	e := t.entries[model]
	if !e.last.IsZero() && now.Sub(e.last) < allBlacklistedLogInterval {
		e.suppressed++
		t.entries[model] = e
		return 0, false
	}
	t.entries[model] = allBlacklistedLogEntry{last: now}
	return e.suppressed, true
}

// logAllPodsBlacklisted records that routing fell back to blacklisted pods because every
// ready pod of the request's model is excluded -- the one case where the blacklist is
// deliberately ignored. It names the model and each pod's exclusion reason, throttled per
// model (see allBlacklistedLogInterval).
func (s *Server) logAllPodsBlacklisted(routeCtx *types.RoutingContext, readyPods []*v1.Pod) {
	suppressed, ok := s.allBlacklistedLog.allow(routeCtx.Model, time.Now())
	if !ok {
		return
	}
	klog.InfoS("all ready pods are blacklisted; routing anyway to avoid a full outage",
		"requestID", routeCtx.RequestID, "model", routeCtx.Model, "readyPods", len(readyPods),
		"reasons", s.podBlacklist.BlacklistReasons(readyPods), "suppressedSinceLast", suppressed)
}

// podEngineQueueDepth returns pod's real backend load for model: the engine's own
// num_requests_running + num_requests_waiting, i.e. requests the engine has
// actually admitted into a batch plus those still queued behind it. A genuinely
// missing metric (e.g. before its first scrape lands) is treated the same as a pod
// truly reporting 0: both count as "not overloaded" here, since the load sweep only
// ever excludes a pod for being too high, never for being unmeasured.
func (s *Server) podEngineQueueDepth(pod *v1.Pod, model string) float64 {
	return routeutil.GetPodModelMetricsSimpleValue(s.cache, pod.Name, pod.Namespace, model, metrics.NumRequestsRunning) +
		routeutil.GetPodModelMetricsSimpleValue(s.cache, pod.Name, pod.Namespace, model, metrics.NumRequestsWaiting)
}

// downstreamCanceled is true when the ext_proc stream context is already done --
// the usual signal that the downstream client (or Envoy tearing the request down
// because the client left) cancelled. When it is false while the stream is closing,
// the client is still there and Envoy closed the processor because of an
// upstream or reset problem.
func downstreamCanceled(st *processState) bool {
	return st != nil && st.ctx != nil && st.ctx.Err() != nil
}

// recordUncompletedRecv reports to the blacklist an ext_proc stream that ended
// before the response completed, for a request that was routed to a pod:
//
//   - the client is gone (the stream context is done): a client abort or timeout.
//     It says nothing about the pod, so nothing is recorded. If headers had
//     arrived they already recorded the status;
//   - no response headers ever arrived: blacklist.StatusStreamClosed;
//   - headers arrived and the client is still connected: the upstream hung up
//     mid-response, blacklist.StatusIncompleteStream.
func (s *Server) recordUncompletedRecv(st *processState) {
	if st == nil || st.routerCtx == nil || !st.routerCtx.HasRouted() || downstreamCanceled(st) {
		return
	}
	code := blacklist.StatusIncompleteStream
	if !st.respHeadersSeen {
		code = blacklist.StatusStreamClosed
	}
	s.podBlacklist.RecordResult(st.routerCtx.TargetPod(), st.routerCtx.TargetAddress(), code, st.model)
}
