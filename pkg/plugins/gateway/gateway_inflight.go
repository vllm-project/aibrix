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

package gateway

import (
	"context"
	"errors"
	"fmt"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// errReplicaInflightExceeded is returned by selectTargetPod when every routable
// replica is already at its config profile's requestsInflight cap.
var errReplicaInflightExceeded = errors.New("all replicas have reached inflight limit")

func replicaInflightLimit(routingCtx *types.RoutingContext) int64 {
	if routingCtx == nil || routingCtx.ConfigProfile == nil {
		return 0
	}
	return routingCtx.ConfigProfile.RequestsInflight
}

// enforceReplicaInflight checks whether the already-selected target pod still has
// capacity for one more concurrent request under its config profile's requestsInflight cap,
// and if so atomically reserves that capacity for this request.
//
// Uses AdmitPodRunningRequest, not a plain read (GetPodRunningRequests) followed by the
// normal request-tracking path's later increment: this request (and any other concurrent
// one to the same pod, on this gateway or another) would otherwise be able to observe the
// same pre-increment count and all get admitted regardless of how tight limit is -- see
// AdmitPodRunningRequest's doc comment. A successful admission here means this gateway's
// own contribution to the pod's live count has already been applied, so it sets
// routingCtx.ReplicaInflightAdmitted -- addPodStats (cache_trace.go) must see that and skip
// its own increment of the same counter, or this request would be double-counted.
//
// err != nil means the pod dropped out of the cache between selection and this call (an
// AdmitPodRunningRequest contract, mirroring GetPodRunningRequests) -- fail open, same as
// the plain read this replaced did on error.
func (s *Server) enforceReplicaInflight(ctx context.Context, model string, routingCtx *types.RoutingContext) *extProcPb.ProcessingResponse {
	limit := replicaInflightLimit(routingCtx)
	if limit <= 0 {
		return nil
	}
	if !routingCtx.HasRouted() || routingCtx.TargetPod() == nil {
		return nil
	}
	pod := routingCtx.TargetPod()
	admitted, err := s.cache.AdmitPodRunningRequest(pod.Name, pod.Namespace, limit)
	if err != nil {
		return nil
	}
	if !admitted {
		klog.InfoS("replica_inflight_exceeded", "requestID", routingCtx.RequestID, "model", model,
			"targetPod", pod.Name, "limit", limit, "reason", "replica_at_capacity")
		return replicaInflightExceededResponse(model, limit)
	}
	routingCtx.ReplicaInflightAdmitted = true
	return nil
}

func replicaInflightExceededResponse(model string, limit int64) *extProcPb.ProcessingResponse {
	return buildErrorResponseWithType(envoyTypePb.StatusCode_TooManyRequests,
		fmt.Sprintf("model: %v has exceeded replica inflight limit: %v", model, limit),
		ErrorTypeOverloaded, ErrorCodeReplicaInflightExceeded, "",
		HeaderErrorReplicaInflightExceeded, "true")
}

// filterSaturatedReplicaInflight drops pods whose current running-request count is
// already at the cap. A pod missing from the batch result (not yet scraped, or not
// found in the cache) is kept so enforceReplicaInflight can fail-open on it later.
// Uses GetPodsRunningRequests (one Redis round trip for every candidate) rather than
// looping a single-pod read.
//
// This is only a best-effort pre-filter to steer selection away from saturated
// replicas -- the actual hard cap is enforced later by enforceReplicaInflight's atomic
// AdmitPodRunningRequest. So on a GetPodsRunningRequests error (e.g. a Redis blip),
// failing open here (returning every candidate unfiltered) can at worst route to an
// already-saturated pod, which enforceReplicaInflight will then reject; it does not
// bypass the cap itself.
func (s *Server) filterSaturatedReplicaInflight(pods []*v1.Pod, limit int64) []*v1.Pod {
	if limit <= 0 || len(pods) == 0 {
		return pods
	}
	running, err := s.cache.GetPodsRunningRequests(pods)
	if err != nil {
		return pods
	}
	kept := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		count, ok := running[utils.GeneratePodKey(pod.Namespace, pod.Name)]
		if !ok || count < limit {
			kept = append(kept, pod)
		}
	}
	return kept
}
