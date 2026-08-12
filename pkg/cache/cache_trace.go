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
)

type podStatsRecord struct {
	pod         *Pod
	port        int
	pendingLoad float64
}

func podStatsKey(modelName, requestID string) string {
	return modelName + "\x00" + requestID
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

	metaPod, ok := c.metaPods.Load(utils.GeneratePodKey(pod.Namespace, pod.Name))
	if !ok {
		klog.Warningf("can't find routing pod: %s, requestID: %s", pod.Name, requestID)
		return
	}
	podStats := &podStatsRecord{pod: metaPod, port: port}

	// Update running requests
	requests := atomic.AddInt32(&metaPod.runningRequests, 1)
	metricName := metrics.RealtimeNumRequestsRunning
	if port > 0 {
		metricName = metricName + "/" + strconv.Itoa(port)
	}
	if err := c.updatePodRecord(metaPod, "", metricName, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: float64(requests)}); err != nil {
		klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s, err: %v", metrics.RealtimeNumRequestsRunning, metaPod.Name, requestID, err)
	}

	// Update pending load
	var utilization float64
	if c.pendingLoadProvider != nil {
		var err error
		ctx.PendingLoad, err = c.pendingLoadProvider.GetConsumption(ctx, pod)
		if err == nil {
			podStats.pendingLoad = ctx.PendingLoad
			utilization = metaPod.pendingLoadUtilization.Add(ctx.PendingLoad)
			if c.updatePodRecord(metaPod, "", metrics.RealtimeNormalizedPendings, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: utilization}) != nil {
				klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s, err: %v", metrics.RealtimeNormalizedPendings, metaPod.Name, requestID, err)
			}
		} else if !IsError(err, ErrorMissingProfile) { // ErrorMissingProfile is not considered as an error here and should be reported where the profile is essential.
			klog.Errorf("error on track request load consumption: %v", err)
		}
	}
	c.podStats.Store(podStatsKey(modelName, requestID), podStats)

	if metaPod.CanLogPodTrace(5) {
		klog.V(4).InfoS("pod stats updated (addPodStats).", "pod", metaPod.Name, "requestID", ctx.RequestID, "running_requests", requests, "pending_util", utilization, "pending_load", ctx.PendingLoad)
	}
}

func (c *Store) donePodStats(ctx *types.RoutingContext, requestID string, modelName string) {
	podStats, ok := c.podStats.LoadAndDelete(podStatsKey(modelName, requestID))
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

	// Update running requests
	requests := atomic.AddInt32(&metaPod.runningRequests, -1)
	atomic.AddInt64(&metaPod.completedRequests, 1)
	metricName := metrics.RealtimeNumRequestsRunning
	if port > 0 {
		metricName = metricName + "/" + strconv.Itoa(port)
	}
	if err := c.updatePodRecord(metaPod, modelName, metricName, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: float64(requests)}); err != nil {
		klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s", metrics.RealtimeNumRequestsRunning, metaPod.Name, requestID)
	}

	// Update pending load
	var utilization float64
	if podStats.pendingLoad != 0.0 && c.pendingLoadProvider != nil {
		utilization = metaPod.pendingLoadUtilization.Add(-podStats.pendingLoad)
		if c.updatePodRecord(metaPod, modelName, metrics.RealtimeNormalizedPendings, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: utilization}) != nil {
			klog.Warningf("can't update realtime metric: %s, pod: %s, requestID: %s", metrics.RealtimeNormalizedPendings, metaPod.Name, requestID)
		}
		if utilization < c.pendingLoadProvider.Cap() {
			// Notify queue router to try route with pending requests.
			if metaModel, ok := c.metaModels.Load(modelName); ok && metaModel.QueueRouter != nil {
				// nolint: errcheck
				metaModel.QueueRouter.Route(nil, metaModel.Pods.Array())
			}
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
