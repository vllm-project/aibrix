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
	"math"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.opentelemetry.io/otel/attribute"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/errresp"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/inflight"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// errReplicaInflightExceeded is returned by selectTargetPod when every pod it considered is
// already at its config profile's requestsInflight cap. The considered set is the candidate set
// when the candidate stage narrowed it, and the ready set otherwise.
var errReplicaInflightExceeded = errors.New("all considered replicas have reached inflight limit")

func replicaInflightLimit(routingCtx *types.RoutingContext) int64 {
	if routingCtx == nil || routingCtx.ConfigProfile == nil {
		return 0
	}
	return routingCtx.ConfigProfile.RequestsInflight
}

// enforceReplicaInflight checks whether the already-selected target pod still has
// capacity for one more concurrent request under its config profile's requestsInflight cap,
// and if so reserves that capacity atomically when Redis is available.
//
// Uses AdmitPodRunningRequest, not a plain read (GetPodRunningRequests) followed by the
// normal request-tracking path's later increment: this request (and any other concurrent
// one to the same pod, on this gateway or another) would otherwise be able to observe the
// same pre-increment count and all get admitted regardless of how tight limit is -- see
// AdmitPodRunningRequest's doc comment. An admission whose +1 is, or may be, already in
// Redis (counted) sets routingCtx.ReplicaInflightAdmitted -- addPodStats (cache_trace.go)
// then skips its own increment of the same counter, or this request would be double-counted.
// Otherwise the increment is left to normal request tracking.
//
// err != nil means the pod dropped out of the cache between selection and this call (an
// AdmitPodRunningRequest contract, mirroring GetPodRunningRequests) -- fail open, same as
// the plain read this replaced did on error.
func (s *Server) enforceReplicaInflight(ctx context.Context, model string, routingCtx *types.RoutingContext) *extProcPb.ProcessingResponse {
	limit := replicaInflightLimit(routingCtx)
	if limit <= 0 {
		return nil
	}
	if routingCtx.ReplicaInflightChecked {
		// The router already admitted its target through AdmitTarget (routeWithEarlyAdmission).
		return nil
	}
	if !routingCtx.HasRouted() || routingCtx.TargetPod() == nil {
		return nil
	}
	pod := routingCtx.TargetPod()
	if !s.admitReplicaInflight(routingCtx, pod, limit) {
		klog.InfoS("replica_inflight_exceeded", "requestID", routingCtx.RequestID, "model", model,
			"targetPod", pod.Name, "limit", limit, "reason", "replica_at_capacity")
		return withRoutingResponseHeaders(replicaInflightExceededResponse(model, limit), routingCtx, routingCtx.RequestID)
	}
	return nil
}

// admitReplicaInflight runs the atomic admission for pod and records a counted admission on
// routingCtx. It reports false only when pod is at its cap; a pod missing from the cache
// fails open, as described on enforceReplicaInflight.
func (s *Server) admitReplicaInflight(routingCtx *types.RoutingContext, pod *v1.Pod, limit int64) bool {
	admitted, counted, err := s.cache.AdmitPodRunningRequest(pod.Name, pod.Namespace, limit)
	if err != nil {
		return true
	}
	if !admitted {
		return false
	}
	routingCtx.ReplicaInflightAdmitted = counted
	return true
}

// routeWithEarlyAdmission routes a request whose router may start work or record state on
// the request's behalf before it returns a target -- PD sends the prefill leg from inside
// Route, and the prefix-cache routers index the prompt on the chosen pod. Admitting that
// target afterwards, in enforceReplicaInflight, would let every request that arrives during
// one prefill pass the pre-filter for the same decode pod, run its own prefill, and only then
// be rejected, leaving the prefill's work and KV behind; a rejected prefix-cache request would
// leave an index entry for a prompt the pod never received, and draw the next requests with
// that prefix to it. So the router admits its target through routeCtx.AdmitTarget as soon as
// it has chosen it, and a full target fails Route with types.ErrTargetReplicaInflightExceeded
// before any of that happens. A router that never calls AdmitTarget is admitted afterwards by
// enforceReplicaInflight, as before.
//
// A counted admission whose Route then fails (the prefill leg failed) never reaches
// AddRequestCount, so request tracking's completion decrement will not release it; it is
// released here instead. A router that admits a second target releases the first one's
// slot, so only the last admitted target holds one.
func (s *Server) routeWithEarlyAdmission(routeCtx *types.RoutingContext, router types.Router, pods types.PodList, limit int64) (string, error) {
	var admittedPod *v1.Pod
	release := func() {
		if admittedPod != nil && routeCtx.ReplicaInflightAdmitted {
			s.cache.ReleasePodRunningRequest(admittedPod.Name, admittedPod.Namespace)
		}
		admittedPod = nil
		routeCtx.ReplicaInflightAdmitted = false
	}
	routeCtx.SetTargetAdmitter(func(pod *v1.Pod) error {
		routeCtx.ReplicaInflightChecked = true
		release()
		if !s.admitReplicaInflight(routeCtx, pod, limit) {
			return types.ErrTargetReplicaInflightExceeded
		}
		admittedPod = pod
		return nil
	})
	target, err := router.Route(routeCtx, pods)
	if err != nil {
		release()
	}
	return target, err
}

func replicaInflightExceededResponse(model string, limit int64) *extProcPb.ProcessingResponse {
	return errresp.BuildErrorResponseWithType(envoyTypePb.StatusCode_TooManyRequests,
		fmt.Sprintf("model: %v has exceeded replica inflight limit: %v", model, limit),
		errresp.ErrorTypeOverloaded, ErrorCodeReplicaInflightExceeded, "",
		HeaderErrorReplicaInflightExceeded, "true")
}

// filterSaturatedReplicaInflight drops pods whose current running-request count is
// already at the cap. A pod missing from the batch result (not yet scraped, or not
// found in the cache) is kept so enforceReplicaInflight can fail-open on it later.
// Reads the counts through routeCtx.RunningRequests (one Redis round trip for every
// candidate, shared with the rest of the routing decision) rather than looping a
// single-pod read.
//
// This is only a best-effort pre-filter to steer selection away from saturated
// replicas -- the actual hard cap is enforced later by enforceReplicaInflight's atomic
// AdmitPodRunningRequest. So on a GetPodsRunningRequests error (e.g. a Redis blip),
// failing open here (returning every candidate unfiltered) can at worst route to an
// already-saturated pod, which enforceReplicaInflight will then reject; it does not
// bypass the cap itself.
func (s *Server) filterSaturatedReplicaInflight(routeCtx *types.RoutingContext, pods []*v1.Pod, limit int64) []*v1.Pod {
	if limit <= 0 || len(pods) == 0 {
		return pods
	}
	running, err := routeCtx.RunningRequests(pods, s.cache.GetPodsRunningRequests)
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

// narrowToInflightCapacity drops the pods already at the per-replica cap and reports
// errReplicaInflightExceeded when none is left. scope names the set being filtered
// (inflight.InflightScopeFleet or inflight.InflightScopeCandidates).
//
// For non-exclusive strategies selectTargetPod runs it on the candidate set, so the read it
// performs is the bounded one; it is the first reader of the per-decision snapshot, so whatever
// the candidate stage kept is what the rest of the decision reuses. Filtering before
// SelectCandidates reads every ready pod and leaves the stage nothing to save (#3463).
func (s *Server) narrowToInflightCapacity(routeCtx *types.RoutingContext, pods []*v1.Pod, scope string) ([]*v1.Pod, error) {
	limit := replicaInflightLimit(routeCtx)
	if limit <= 0 {
		return pods, nil
	}
	kept := s.filterSaturatedReplicaInflight(routeCtx, pods, limit)
	if len(kept) > 0 {
		return kept, nil
	}
	// Every pod considered is at the cap. A full-fleet fallback would read every ready
	// pod at the load where Redis is hottest, and a second K-pod sample would miss this
	// decision's snapshot, so keep the existing 429 (#3463). The span records how many
	// pods were considered, and the counter records which set that was, so a sampled-set
	// rejection can be told from a fleet-wide one after the fact: ready_pods, set at the
	// top of selectTargetPod, is the fleet.
	inflight.RecordInflightRejection(routeCtx.Model, scope)
	if routeCtx.Span != nil {
		routeCtx.Span.SetAttributes(
			attribute.Int("inflight_cap", int(limit)),
			attribute.Int("inflight_considered_pods", len(pods)),
		)
	}
	return nil, errReplicaInflightExceeded
}

// narrowPDToInflightCapacity applies the per-replica inflight cap to the pods PD routing can
// actually admit and track in aibrix:rrq:* -- routable decode pods in rolesets that also have
// a routable prefill pod, plus routable combined pods when prompt-length bucketing is enabled.
//
// Prefill pods, encoders, non-HTTP multi-node TP workers (pod-group-index != "0"), and orphan
// decode pods without a prefill peer are never passed to AdmitTarget or AddRequestCount, so
// their running-request counts stay 0. Filtering them in Redis both wastes HGETALL commands
// and leaves non-empty leftovers when every routable decode pod is saturated, masking the 429
// behind a downstream PD "not ready" 503.
//
// When pods carry no PD role/roleset labels (e.g. legacy or synthetic fixtures), this falls
// back to narrowToInflightCapacity over the full slice. When pods are PD-labeled but have no
// admissible target candidates (e.g. all decode or all prefill pods are unready), this returns
// pods unchanged without a Redis read so pdRouter reports its not-ready error instead of a
// false 429.
func (s *Server) narrowPDToInflightCapacity(routeCtx *types.RoutingContext, pods []*v1.Pod, scope string) ([]*v1.Pod, error) {
	if replicaInflightLimit(routeCtx) <= 0 || len(pods) == 0 {
		return pods, nil
	}
	targets, passthrough, hasLabeledPD := partitionPDInflightTargets(routeCtx, pods)
	if !hasLabeledPD {
		return s.narrowToInflightCapacity(routeCtx, pods, scope)
	}
	if len(targets) == 0 {
		return pods, nil
	}
	keptTargets, err := s.narrowToInflightCapacity(routeCtx, targets, scope)
	if err != nil {
		return nil, err
	}
	if len(passthrough) == 0 {
		return keptTargets, nil
	}
	out := make([]*v1.Pod, 0, len(keptTargets)+len(passthrough))
	out = append(out, keptTargets...)
	out = append(out, passthrough...)
	return out, nil
}

// partitionPDInflightTargets splits PD readyPods into inflight-checked target candidates
// (routable decode pods in prefill-backed rolesets, plus combined spillover pods when
// prompt-length bucketing is enabled) and non-target passthrough pods (prefill, encode,
// non-HTTP TP workers, and pods in incomplete or non-matching-bucket rolesets).
// hasLabeledPD is false when no pod carries both PD roleset and role labels.
func partitionPDInflightTargets(routeCtx *types.RoutingContext, pods []*v1.Pod) (targets, passthrough []*v1.Pod, hasLabeledPD bool) {
	bucketing := routeCtx != nil && routeCtx.RoutingOverrides().PD.PromptLengthBucketing
	var promptLength int
	if bucketing {
		promptLength, _ = routeCtx.PromptLength()
	}
	var cfgCache map[string]*types.RoutingConfig

	var prefillRolesets map[string]struct{}
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		roleSetID, hasRoleset := pod.Labels[pd.RoleSetLabel]
		roleID, hasRole := pod.Labels[pd.RoleLabel]
		if !hasRoleset || !hasRole {
			continue
		}
		hasLabeledPD = true
		if roleID == pd.RolePrefill && pd.IsPodWithHTTPServer(pod) {
			if bucketing && !isPDPodSuitableForPromptLength(resolvePDRoutingConfigCached(routeCtx, pod, &cfgCache), promptLength) {
				continue
			}
			if prefillRolesets == nil {
				prefillRolesets = make(map[string]struct{})
			}
			prefillRolesets[roleSetID] = struct{}{}
		}
	}
	if !hasLabeledPD {
		return nil, nil, false
	}

	targets = make([]*v1.Pod, 0, len(pods)/2+1)
	passthrough = make([]*v1.Pod, 0, len(pods)/2+1)
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		roleSetID, hasRoleset := pod.Labels[pd.RoleSetLabel]
		roleID, hasRole := pod.Labels[pd.RoleLabel]
		if hasRoleset && hasRole && pd.IsPodWithHTTPServer(pod) {
			switch roleID {
			case pd.RoleDecode:
				if _, ok := prefillRolesets[roleSetID]; ok {
					if !bucketing || isPDPodSuitableForPromptLength(resolvePDRoutingConfigCached(routeCtx, pod, &cfgCache), promptLength) {
						targets = append(targets, pod)
						continue
					}
				}
			case pd.RolePrefill:
				// Never admitted via AdmitTarget / aibrix:rrq:*.
			default:
				// Covers combined spillover pods and any future non-prefill/decode roles
				// (excluding encoders, which never run through AdmitTarget).
				if bucketing && !encode.IsEncoder(pod) {
					cfg := resolvePDRoutingConfigCached(routeCtx, pod, &cfgCache)
					if cfg != nil && cfg.Combined != nil && *cfg.Combined && isPDPodSuitableForPromptLength(cfg, promptLength) {
						targets = append(targets, pod)
						continue
					}
				}
			}
		}
		passthrough = append(passthrough, pod)
	}
	return targets, passthrough, true
}

// resolvePDRoutingConfigCached resolves and parses pod's RoutingConfig once per
// distinct pod config annotation within one partitionPDInflightTargets pass,
// avoiding repeated JSON unmarshaling across large mixed/bucketed fleets.
func resolvePDRoutingConfigCached(routeCtx *types.RoutingContext, pod *v1.Pod, cache *map[string]*types.RoutingConfig) *types.RoutingConfig {
	if routeCtx == nil || pod == nil {
		return nil
	}
	anno := pod.Annotations[constants.ModelAnnoConfig]
	if *cache != nil {
		if cfg, ok := (*cache)[anno]; ok {
			return cfg
		}
	}
	var cfg *types.RoutingConfig
	if profile := configprofiles.ResolveProfileFrom(routeCtx.ModelConfig, pod, routeCtx.ReqConfigProfile); profile != nil {
		cfg = configprofiles.ParseRoutingConfig(profile.RoutingConfig)
	}
	if *cache == nil {
		*cache = make(map[string]*types.RoutingConfig, 4)
	}
	(*cache)[anno] = cfg
	return cfg
}

// isPDPodSuitableForPromptLength mirrors pdRouter.isPodSuitableForPromptLength /
// promptLenBucketBounds using only leaf config types so multi-bucket PD fleets
// evaluate inflight capacity only against pods eligible for promptLength.
func isPDPodSuitableForPromptLength(cfg *types.RoutingConfig, promptLength int) bool {
	minLength, maxLength := 0, math.MaxInt32
	if cfg != nil {
		if v := cfg.PromptLenBucketMinLength; v != nil && *v > 0 {
			minLength = *v
		}
		if v := cfg.PromptLenBucketMaxLength; v != nil && *v > 0 {
			maxLength = *v
		}
	}
	if minLength > maxLength {
		return false
	}
	if minLength == 0 && maxLength == math.MaxInt32 {
		return true
	}
	return promptLength >= minLength && promptLength <= maxLength
}
