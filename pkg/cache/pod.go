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
	"sync"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
	atomic_ext "go.uber.org/atomic"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type Pod struct {
	*v1.Pod
	Models       *utils.Registry[string]             // Model/adapter names that the pod is running
	Metrics      utils.SyncMap[string, MetricRecord] // Pod metrics (metric_name -> record)
	ModelMetrics utils.SyncMap[string, MetricRecord] // Pod-model metrics (model_name/metric_name -> record)

	// claimMetrics is immutable after publication. A non-nil map marks a warm Pod,
	// including when it has no active engines. Unchanged targets survive Pod updates.
	claimMetrics map[string]*modelClaimMetricsTarget

	// Shared gateway labels are immutable for this Pod object and rebuilt on update.
	gatewayMetricLabelsOnce sync.Once
	gatewayMetricLabels     []string

	// Realtime statstistic
	runningRequests        int32 // Realtime running requests counter.
	completedRequests      int64 // Monotonically increasing count of finished requests (gateway-tracked).
	completedOutputTokens  int64 // Monotonically increasing count of output tokens of finished requests (gateway-tracked; 0 added when usage is unavailable).
	pendingLoadUtilization atomic_ext.Float64

	// completedOutputBusyNanos is the monotonically increasing sum of the time, from addPodStats
	// to donePodStats, that finished requests with known output tokens spent on this pod. It covers
	// the same requests as completedOutputTokens' non-zero additions, so their ratio is a per-request
	// decode rate (see RealtimeOutputTokensPerRequestSecondEWMA).
	completedOutputBusyNanos int64
	// completedOutputRequests counts the finished requests behind the two counters above: those
	// with known output tokens. A rate window updates the estimate only if it holds at least
	// minOutputRateSamples of them (see updateRealtimeOutputTokensPerRequestSecondEWMA).
	completedOutputRequests int64

	// statsGeneration identifies this cache object's request-tracking counters.
	// Copied on a same-IP resume (see addPodLocked) so in-flight Add/Done can
	// follow the live object across a flap; incremented on a fresh add so a
	// stale completion cannot bleed into a new generation after grace expiry.
	statsGeneration int64

	// Log frenquency control
	lastTraceLogTimestamp int64

	// readySince is the UnixNano time this gateway first saw the pod routable
	// (utils.FilterReadyPod), not when it was added, so time spent Pending or NotReady doesn't
	// count against the ramp. 0 means never routable yet. It is kept across same-IP re-adds and
	// NotReady flaps (see addPodLocked) and restarts on a new IP. Read by
	// LeastRequestTopKRouter's ramp and by the load-balance capacity warm-up.
	readySince atomic.Int64
}

func (pod *Pod) CanLogPodTrace(level klog.Level) bool {
	// Skip log throttling for greater log level.
	if klog.V(level).Enabled() {
		return true
	}

	lastTs := atomic.LoadInt64(&pod.lastTraceLogTimestamp)
	ts := time.Now().UnixNano()
	if ts-lastTs < int64(traceLogInterval) {
		return false
	}

	// Ensure only one attempt pass.
	return atomic.CompareAndSwapInt64(&pod.lastTraceLogTimestamp, lastTs, ts)
}
