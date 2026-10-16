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

package router

import (
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// calculatePodScoreBasedOffRequestRate returns the pod's queue-drain ratio:
//
//	(waitingReqs + prefillPreallocQueue + decodePreallocQueue) / drainRate1m
//
// A lower score means the pod is draining its queue faster relative to its
// backlog. Missing metrics are treated as 0; a zero drainRate1m yields +Inf,
// which naturally sorts last.
func calculatePodScoreBasedOffRequestRate(routingCtx *types.RoutingContext, cache cache.Cache, pod *v1.Pod) float64 {
	modelName, _ := constants.ModelNameFromMetadata(pod.Labels, pod.Annotations)
	waitingReqs := routeutil.GetPodModelMetricsSimpleValue(cache, pod.Name, pod.Namespace, modelName, metrics.NumRequestsWaiting)
	prefillPreallocQueue := routeutil.GetPodModelMetricsSimpleValue(cache, pod.Name, pod.Namespace, modelName, metrics.NumPrefillPreallocQueueReqs)
	decodePreallocQueue := routeutil.GetPodModelMetricsSimpleValue(cache, pod.Name, pod.Namespace, modelName, metrics.NumDecodePreallocQueueReqs)
	drainRate1m := routeutil.GetPodModelMetricsSimplePrometheusValue(cache, pod.Name, pod.Namespace, modelName, metrics.DrainRate1m)

	if klog.V(4).Enabled() {
		klog.V(4).InfoS("pod_metrics", "requestId",
			routingCtx.RequestID, "podName", pod.Name, "namespace", pod.Namespace, "modelName", modelName,
			"waitingReqs", waitingReqs, "prefillPreallocQueue", prefillPreallocQueue, "decodePreallocQueue", decodePreallocQueue, "drainRate1m", drainRate1m)
	}

	return (waitingReqs + prefillPreallocQueue + decodePreallocQueue) / drainRate1m
}
