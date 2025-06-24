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

package queue

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	fifoOnNonSLOViolation    bool = false
	queueOverallSLO          bool = false
	monogenousGPURouting     bool = true
	monogenousGPURoutingOnly bool = monogenousGPURouting && false
	initialTotalSubQueues    int  = 8  // Expect no more than 8 subqueues
	initialSubQueueSize      int  = 64 // Support maximum 128 pending request per sub-queue within one expansion.
)

var (
	errNoSLO = errors.New("no SLO available")
)

type candidateProfiles struct {
	Rank float64 // Rank of this profile
	Key  string  // Key of this profile
}

type candidateRouterRequest struct {
	*types.RoutingContext
	SubKey   string
	Profiles []*candidateProfiles
}

func (crr *candidateRouterRequest) resetProfiles() {
	crr.Profiles = crr.Profiles[:0]
}

func (crr *candidateRouterRequest) nextProfile() *candidateProfiles {
	if len(crr.Profiles) < cap(crr.Profiles) {
		crr.Profiles = crr.Profiles[:len(crr.Profiles)+1]
	} else {
		crr.Profiles = append(crr.Profiles, &candidateProfiles{})
	}

	ret := crr.Profiles[len(crr.Profiles)-1]
	if ret == nil {
		ret = &candidateProfiles{}
		crr.Profiles[len(crr.Profiles)-1] = ret
	}
	ret.Rank = 0.0
	ret.Key = ""
	return ret
}

type SLOQueue struct {
	routerProvider types.RouterProviderFunc
	cache          cache.Cache

	modelName string
	subs      utils.SyncMap[string, types.RouterQueue[*types.RoutingContext]]
	// features  utils.SyncMap[string, types.RequestFeatures]
	subpool sync.Pool

	dequeueCandidates   []*candidateRouterRequest
	lastCandidateSubKey string // Clear in Dequeue()
	lastCandidateError  error  // Clear in Dequeue()
}

func NewSLOQueue(provider types.RouterProviderFunc, modelName string) (router *SLOQueue, err error) {
	// Dedup deployments
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}

	router = &SLOQueue{
		routerProvider: provider,
		cache:          c,
		modelName:      modelName,
	}
	router.subpool.New = func() any { return NewSimpleQueue[*types.RoutingContext](initialSubQueueSize) }
	router.expandDequeueCandidatesLocked(initialTotalSubQueues)
	return router, nil
}

func (q *SLOQueue) Enqueue(ctx *types.RoutingContext, currentTime time.Time) error {
	// Set output predictor first
	if predictor, err := q.cache.GetOutputPredictor(ctx.Model); err != nil {
		return err
	} else {
		ctx.SetOutputPreditor(predictor)
	}

	newQueue := q.subpool.Get().(types.RouterQueue[*types.RoutingContext])
	features, err := ctx.Features()
	if err != nil {
		return err
	}

	key := q.featuresKey(features)
	sub, loaded := q.subs.LoadOrStore(key, newQueue)
	if loaded {
		q.subpool.Put(newQueue)
	}

	// SimpleQueue.Enqueue() returns nil always.
	sub.Enqueue(ctx, currentTime) // nolint: errcheck
	q.debugSub(fmt.Sprintf("%s request enqueued, request=%s", ctx.Model, ctx.RequestID))
	return nil
}

func (q *SLOQueue) Peek(currentTime time.Time, pods types.PodList) (*types.RoutingContext, error) {
	// Most implementation goes here.
	var err error

	deployments := pods.Indexes()
	deploymentProfiles := make([]*cache.ModelGPUProfile, len(deployments))
	availableProfiles := 0
	for i, deploymentName := range deployments {
		deploymentProfiles[i], err = q.cache.GetModelProfileByDeploymentName(deploymentName, q.modelName)
		if err != nil {
			klog.Warning(err)
			// Note that deployments[deploymentName] is set and will not try again.
			// We simply ignore this deployment.
			continue
		}
		availableProfiles++
	}
	if availableProfiles == 0 {
		klog.Warningf("SLOQueue found no profile available for model %s, fallback to FIFO queue", q.modelName)
	}
	// Clear error
	err = nil

	// Refill candidates
	q.dequeueCandidates = q.dequeueCandidates[:0]
	q.debugSub(fmt.Sprintf("peeking %s requests for %v", q.modelName, deployments))
	// Define fallback handler to handle cases like:
	// 1. No available profiles.
	// 2. Profile does not provide SLO info.
	// Fallback handler emulate a FIFO queue by comparing arrival time
	fallbackHandler := func(key string, subReq *types.RoutingContext) bool {
		if len(q.dequeueCandidates) == 0 {
			q.validateDequeueCandidatesLocked(1)
			q.dequeueCandidates = q.dequeueCandidates[:1]
		} else if q.dequeueCandidates[0].RoutingContext.RequestTime.Before(subReq.RequestTime) {
			// Skip this subqueue
			return true
		}
		// Update ealiest candidate
		q.dequeueCandidates[0].RoutingContext = subReq
		q.dequeueCandidates[0].SubKey = key
		return true
	}
	q.subs.Range(func(key string, sub types.RouterQueue[*types.RoutingContext]) bool {
		r, peekErr := sub.Peek(currentTime, pods)
		if peekErr == types.ErrQueueEmpty {
			return true
		} else if peekErr != nil {
			klog.Errorf("Failed to peek subqueue %s: %v.", key, peekErr)
			return true
		}

		// Keep fallback decision in case anything wrong.
		// Fallback decision occupies first element(0) of q.dequeueCandidates.
		fbRet := fallbackHandler(key, r)
		// Fallback case 1: No available profiles.
		if availableProfiles == 0 {
			// Warned, simply follows fallback decision.
			return fbRet
		}
		// Append new candidate to candidates
		idx := len(q.dequeueCandidates)
		q.validateDequeueCandidatesLocked(idx + 1)
		q.dequeueCandidates = q.dequeueCandidates[:idx+1]
		// Reset values
		candidate := q.dequeueCandidates[idx]
		candidate.RoutingContext = r
		candidate.SubKey = key
		candidate.resetProfiles()
		// Use a relaxer SLO
		var rank float64
		var rankErr error
		for _, profile := range deploymentProfiles {
			// Skip deployments with no profile
			if profile == nil {
				continue
			}
			// Calculate rank
			if queueOverallSLO {
				rank, rankErr = q.queueRank(currentTime, r, sub, profile)
			} else {
				rank, rankErr = q.rank(currentTime, r, profile)
			}
			if rankErr != nil {
				err = rankErr
				klog.Warningf("SLOQueue failed to get SLO info for request %s with profile %s: %v, skip.", r.RequestID, profile.Deployment, rankErr)
				continue
			}
			cp := candidate.nextProfile()
			cp.Rank = rank
			cp.Key = profile.Deployment
		}
		// Fallback case 2: Profile does not provide SLO info.
		if len(candidate.Profiles) == 0 {
			// No available profiles, skip this subqueue.
			klog.Warningf("SLOQueue failed to get SLO info for request %s in all profiles, fallback to FIFO queue.", r.RequestID)
			return fbRet
		}
		// Sort by rank ascendingly, so the first one contains lowest rank.
		sort.Slice(candidate.Profiles, func(i, j int) bool {
			return q.higherRank(candidate.Profiles[i].Rank, candidate.Profiles[j].Rank) < 0
		})
		return true
	})
	if err != nil {
		// Apply fallback decision by just keep the first one.
		q.dequeueCandidates = q.dequeueCandidates[:1]
	}

	if len(q.dequeueCandidates) == 0 {
		return nil, types.ErrQueueEmpty
	} else if len(q.dequeueCandidates) == 1 {
		// Only candidate
		q.lastCandidateSubKey = q.dequeueCandidates[0].SubKey
		return q.dequeueCandidates[0].RoutingContext, nil
	}

	// Exclude fallback candidate
	dequeueCandidates := q.dequeueCandidates[1:]

	// Sort by rank
	sort.Slice(dequeueCandidates, func(i, j int) bool {
		// Keep original order for no slo violation if fifoOnNonSLOViolation enabled.
		if fifoOnNonSLOViolation && dequeueCandidates[i].Profiles[0].Rank < 0 && dequeueCandidates[j].Profiles[0].Rank < 0 {
			return dequeueCandidates[i].RoutingContext.RequestTime.Before(dequeueCandidates[j].RoutingContext.RequestTime)
		} else {
			return q.higherRank(dequeueCandidates[i].Profiles[0].Rank, dequeueCandidates[j].Profiles[0].Rank) > 0
		}
	})

	// Start from ealiest
	q.debugCandidates(fmt.Sprintf("%s candidates", q.modelName), dequeueCandidates)
	for _, candidate := range dequeueCandidates {
		var lastErr error
		if monogenousGPURouting {
			//nolint:errcheck
			for i, profile := range candidate.Profiles {
				// Always route the most relaxing profile (the first) and stop if the profile can lead to SLO violation.
				if i > 0 && profile.Rank > 0 {
					break
				}
				// Try routing.
				_, lastErr = q.subRoute(candidate.RoutingContext, &utils.PodArray{Pods: pods.ListByIndex(profile.Key)})
				// If monogenousGPURoutingOnly is enabled, only the most relaxing profile is considered.
				if monogenousGPURoutingOnly || candidate.RoutingContext.HasRouted() {
					break
				}
			}
		} else {
			_, lastErr = q.subRoute(candidate.RoutingContext, pods)
		}
		if candidate.RoutingContext.HasRouted() {
			q.lastCandidateSubKey = candidate.SubKey
			return candidate.RoutingContext, nil
		} else if lastErr != cache.ErrorLoadCapacityReached {
			// We have route dicision concluded as SLO violation. Track the conclusion.
			q.lastCandidateSubKey = candidate.SubKey
			q.lastCandidateError = lastErr
			return candidate.RoutingContext, nil
		} // temporary errors are ignored
	}

	return nil, nil
}

func (q *SLOQueue) Dequeue(ts time.Time) (*types.RoutingContext, error) {
	if len(q.lastCandidateSubKey) == 0 {
		return nil, fmt.Errorf("call SLOQueue.Peek first")
	}
	subkey := q.lastCandidateSubKey
	sub, _ := q.subs.Load(subkey)
	q.lastCandidateSubKey = ""
	q.lastCandidateError = nil
	defer q.debugSub(fmt.Sprintf("%s request dequeued from sub %s,", q.modelName, subkey))
	return sub.Dequeue(ts)
}

func (q *SLOQueue) Len() (total int) {
	q.subs.Range(func(_ string, sub types.RouterQueue[*types.RoutingContext]) bool {
		total += sub.Len()
		return true
	})
	return
}

func (q *SLOQueue) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	// Ctx is not routed if no profiles is found during Peek.
	if !ctx.HasRouted() && q.lastCandidateError == nil {
		return q.subRoute(ctx, pods)
	}
	if q.lastCandidateError != nil {
		return "", q.lastCandidateError
	}
	return ctx.TargetAddress(), nil
}

func (q *SLOQueue) LastError() error {
	return q.lastCandidateError
}

func (q *SLOQueue) validateDequeueCandidatesLocked(size int) {
	if size <= cap(q.dequeueCandidates) {
		return
	}

	q.expandDequeueCandidatesLocked(0)
}

func (q *SLOQueue) expandDequeueCandidatesLocked(limit int) {
	if limit == 0 {
		limit = 2 * cap(q.dequeueCandidates)
	}
	if limit < initialTotalSubQueues {
		limit = initialTotalSubQueues
	}
	candidates := make([]*candidateRouterRequest, limit)
	var dequeueCandidates []*candidateRouterRequest
	if cap(q.dequeueCandidates) > 0 {
		dequeueCandidates = q.dequeueCandidates[:cap(q.dequeueCandidates)]
		copy(candidates, dequeueCandidates)
	}
	for i := len(dequeueCandidates); i < len(candidates); i++ {
		profiles := make([]*candidateProfiles, 10)
		for j := 0; j < len(profiles); j++ {
			profiles[j] = &candidateProfiles{}
		}
		candidates[i] = &candidateRouterRequest{Profiles: profiles} // Arbitrarily initialize capacity to 10.
	}
	q.dequeueCandidates = candidates[:len(q.dequeueCandidates)]
}

func (q *SLOQueue) featuresKey(features types.RequestFeatures) string {
	for i := range features {
		features[i] = math.Round(math.Log2(features[i]))
	}
	return fmt.Sprint(features)
}

func (q *SLOQueue) rank(currentTime time.Time, req *types.RoutingContext, profile *cache.ModelGPUProfile) (rank float64, err error) {
	_, expected, target, err := q.rankImpl(currentTime, req, profile)
	if err != nil {
		return 0.0, err
	}
	// Expecting SLO violation if waited + expected - target > 0.
	return req.Elapsed(currentTime).Seconds() + expected - target, nil
}

func (q *SLOQueue) queueRank(currentTime time.Time, headReq *types.RoutingContext, sub types.RouterQueue[*types.RoutingContext], profile *cache.ModelGPUProfile) (rank float64, err error) {
	signature, headServingTime, target, err := q.rankImpl(currentTime, headReq, profile)
	if err != nil {
		return 0.0, err
	}

	throughput, err := profile.ThroughputRPS(signature...)
	if err != nil {
		return 0.0, err
	}

	queueServiceTime := float64(sub.Len()-1) / throughput
	// Expecting SLO violation if waited + head serving time + queue serving time(excluding the head) - target > 0.
	return headReq.Elapsed(currentTime).Seconds() + headServingTime + queueServiceTime - target, nil
}

func (q *SLOQueue) rankImpl(currentTime time.Time, req *types.RoutingContext, profile *cache.ModelGPUProfile) (signature []int, expected float64, target float64, err error) {
	features, err := req.Features() // Since req are in the queue, Features() must be called before without an error.
	if err != nil {
		return nil, 0.0, 0.0, err
	}
	signature, err = profile.GetSignature(features...)
	if err != nil {
		return
	}

	if profile.SLOs.TPOT > 0.0 {
		expected, target, err = q.rankImplTPOT(currentTime, req, profile, signature)
	} else if profile.SLOs.TTFT > 0.0 {
		expected, target, err = q.rankImplTTFT(currentTime, req, profile, signature)
	} else if profile.SLOs.TPAT > 0.0 {
		expected, target, err = q.rankImplTPAT(currentTime, req, profile, signature)
	} else if profile.SLOs.E2E > 0.0 {
		expected, target, err = q.rankImplE2E(currentTime, req, profile, signature)
	} else {
		err = errNoSLO
	}
	return
}

func (q *SLOQueue) rankImplE2E(_ time.Time, _ *types.RoutingContext, profile *cache.ModelGPUProfile, signature []int) (expected float64, target float64, err error) {
	target = profile.SLOs.E2E // TODO: We should make SLO.E2E always available
	expected, err = profile.LatencySeconds(signature...)
	return
}

// TPAT is simply the normalized E2E latency with regard of the total number of tokens.
func (q *SLOQueue) rankImplTPAT(_ time.Time, req *types.RoutingContext, profile *cache.ModelGPUProfile, signature []int) (expected float64, target float64, err error) {
	prompts, err := req.PromptLength()
	if err != nil {
		// unlikely, we should not reach here.
		return 0.0, 0.0, err
	}
	tokens, err := req.TokenLength()
	if err != nil {
		// unlikely, we should not reach here.
		return 0.0, 0.0, err
	}
	target = profile.SLOs.TPAT * float64(prompts+tokens)
	expected, err = profile.LatencySeconds(signature...)
	return
}

func (q *SLOQueue) rankImplTTFT(_ time.Time, _ *types.RoutingContext, profile *cache.ModelGPUProfile, signature []int) (expected float64, target float64, err error) {
	target = profile.SLOs.TTFT
	expected, err = profile.TTFTSeconds(signature...)
	return
}

func (q *SLOQueue) rankImplTPOT(_ time.Time, req *types.RoutingContext, profile *cache.ModelGPUProfile, signature []int) (expected float64, target float64, err error) {
	tokens, err := req.TokenLength()
	if err != nil {
		// unlikely, we should not reach here.
		return 0.0, 0.0, err
	}
	target = profile.SLOs.TPOT*float64(tokens) + profile.SLOs.TTFT // TTFT is optional if the workload is decoding dominant.
	// Since profile metrics only include mean values, the assumption here is mean(e2e) = mean(ttft) + mean(tpot) * tokens
	// In the case that the workload is decoding dominant, mean(e2e) = mean(tpot) * tokens, while mean(ttft) is ignorable.
	expected, err = profile.LatencySeconds(signature...)
	return
}

func (q *SLOQueue) higherRank(rank1 float64, rank2 float64) float64 {
	return rank1 - rank2
}

func (q *SLOQueue) debugSub(msg string) {
	if !klog.V(5).Enabled() {
		return
	}

	var logMsg strings.Builder
	q.subs.Range(func(key string, sub types.RouterQueue[*types.RoutingContext]) bool {
		logMsg.WriteString(key)
		logMsg.WriteRune('(')
		logMsg.WriteString(strconv.Itoa(sub.Len()))
		logMsg.WriteString(") ")
		return true
	})
	klog.V(5).InfoS(msg, "sub_stats", logMsg.String())
}

func (q *SLOQueue) debugCandidates(msg string, candidates []*candidateRouterRequest) {
	if !klog.V(5).Enabled() {
		return
	}

	var logMsg strings.Builder
	for _, candidate := range candidates {
		logMsg.WriteString(candidate.RequestID)
		logMsg.WriteRune('(')
		for _, profile := range candidate.Profiles {
			logMsg.WriteString(profile.Key)
			logMsg.WriteRune(':')
			logMsg.WriteString(fmt.Sprintf("%.3f", profile.Rank))
			logMsg.WriteRune(' ')
		}
		logMsg.WriteString(") ")
	}
	klog.V(5).InfoS(msg, "candidates", logMsg.String())
}

func (q *SLOQueue) subRoute(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	router, err := q.routerProvider(ctx)
	if err != nil {
		return "", err
	}

	return router.Route(ctx, pods)
}
