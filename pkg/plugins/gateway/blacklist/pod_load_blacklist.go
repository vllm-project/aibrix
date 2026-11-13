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

package blacklist

import (
	"math"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// loadOutlierCheckInterval is how often each model's replica set is
	// re-scored for load outliers (see StartLoadOutlierSweep).
	loadOutlierCheckInterval = 30 * time.Second
	// loadOutlierStdDevThreshold is how many standard deviations above the
	// mean a pod's running+waiting engine request count must be, among its
	// model's replicas, to be excluded from routing as an outlier.
	loadOutlierStdDevThreshold = 2.0
	// loadOutlierMinPods is the fewest ready replicas a model must have for
	// outlier detection to run at all -- mean/stddev over 1-2 pods isn't a
	// meaningful signal.
	loadOutlierMinPods = 3
)

// loadOutlierMinGap is the minimum absolute running+waiting request gap a
// pod must have over its peers' mean before it can be excluded, regardless
// of what the stddev-scaled threshold alone would say. Two failure modes
// this guards against:
//
//   - At small replica counts, the stddev-scaled threshold alone is far too
//     sensitive: scoring against n-1 peers via leave-one-out, the
//     population stddev of just 2 peers is |a-b|/2, so loadOutlierStdDevThreshold
//     (2 sigma) is only ever ~|a-b| away from their mean -- ordinary
//     variance between two otherwise-healthy pods (e.g. [20, 25, 28]) can
//     cross it.
//   - When every peer reports the exact same load, their stddev is 0, so
//     the stddev-scaled threshold is 0 too and *any* positive gap would
//     otherwise qualify -- including a trivial blip off a near-idle
//     baseline (e.g. [0, 0, 1]). This absolute floor is what keeps that
//     case from firing while still letting a genuine outlier against tied
//     peers through (e.g. [20, 20, 100] has a gap of 80, well past the
//     floor).
//
// This mirrors the reasoning behind AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP in
// load_balance.go, which guards the same class of small-n false positive for
// routing-time load-imbalance scoring.
var loadOutlierMinGap = float64(utils.LoadEnvInt("AIBRIX_LOAD_OUTLIER_MIN_GAP", 16))

// QueueDepthFunc returns pod's real backend load for model: the engine's own
// running+waiting request count. A genuinely missing metric should read as 0:
// the load sweep only ever excludes a pod for being too high, never for being
// unmeasured.
type QueueDepthFunc func(pod *v1.Pod, model string) float64

// StartLoadOutlierSweep starts a background goroutine that every
// loadOutlierCheckInterval scores each model's ready replicas by their
// current vLLM-reported running+waiting request count (num_requests_running
// + num_requests_waiting -- the real engine queue depth) and excludes any
// pod more than loadOutlierStdDevThreshold standard deviations above the
// mean of its *peers* (leave-one-out, not the whole group including
// itself -- see sweepLoadOutliers for why). src supplies the models and their
// pods, depth the per-pod load, and onRecovered (optional) is told when a pod
// leaves load exclusion, so the caller can attach its own context to that log.
//
// This is deliberately based on the engine's own metrics rather than
// GetPodRunningRequests (the gateway-local in-flight counter used elsewhere
// for routing/MODEL_REPLICA_REQUESTS_INFLIGHT): that counter only spans the
// lifetime of the gateway's own HTTP/ext_proc call to the pod, so it can
// badly undercount true backend load. A pod's real queue can be hundreds
// deep from repeated preemption or GPU contention while the gateway still
// sees only a handful of concurrently open calls to it.
//
// A pod leaves exclusion the moment a later tick finds it back within
// range: unlike the tracked-failure circuit breaker in
// pod_blacklist.go, there is no backoff or half-open probing here
// -- the periodic recheck IS the recovery signal.
func (b *PodBlacklist) StartLoadOutlierSweep(src PodSource, depth QueueDepthFunc, onRecovered func(*v1.Pod), stopCh <-chan struct{}) {
	ticker := time.NewTicker(loadOutlierCheckInterval)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				klog.Errorf("load outlier sweep goroutine panic recovered: %v", r)
			}
		}()
		for {
			select {
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							klog.Errorf("panic recovered during load outlier sweep: %v", r)
						}
					}()
					b.sweepLoadOutliers(src, depth, onRecovered)
				}()
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

// sweepLoadOutliers scores every known model's ready pods and updates each
// pod's load-exclusion state via PodBlacklist.SetLoadExcluded.
//
// A pod can belong to more than one model's replica set at once (e.g. a base
// model and its LoRA adapters share the same underlying pods -- see
// utils.PodArray / ModelAdapter). Each model is scored independently below,
// but a pod's exclusion verdict is collected across every model it belongs
// to and applied once, after all models have been scored, as the OR of
// those verdicts: SetLoadExcluded has no model of its own (a pod is either
// excluded from routing or it isn't), so applying it once per model as each
// model is processed would make the *last* model iterated -- order
// depending on cache.ListModels(), which is not guaranteed stable -- silently
// overwrite an outlier verdict an earlier model just set for the same pod.
func (b *PodBlacklist) sweepLoadOutliers(src PodSource, depth QueueDepthFunc, onRecovered func(*v1.Pod)) {
	type podVerdict struct {
		pod     *v1.Pod
		outlier bool
	}
	verdicts := make(map[string]*podVerdict)
	mark := func(pod *v1.Pod, outlier bool) {
		key := PodKey(pod)
		if existing, ok := verdicts[key]; ok {
			existing.outlier = existing.outlier || outlier
			return
		}
		verdicts[key] = &podVerdict{pod: pod, outlier: outlier}
	}

	for _, model := range src.ListModels() {
		podsArr, err := src.ListPodsByModel(model)
		if err != nil || podsArr == nil {
			continue
		}
		pods := b.withoutQuarantined(utils.FilterRoutablePods(podsArr.All()))

		if len(pods) < loadOutlierMinPods {
			// Too few replicas for mean/stddev to mean anything -- from this
			// model's point of view these pods have nothing demanding
			// exclusion (they may still end up excluded via another model
			// they also belong to, per the OR above).
			for _, pod := range pods {
				mark(pod, false)
			}
			continue
		}

		loads := make([]float64, len(pods))
		for i, pod := range pods {
			loads[i] = depth(pod, model)
		}

		// Each pod is scored against the mean/stddev of its *peers*, not the
		// whole group including itself: folding the candidate into its own
		// baseline lets it inflate the very stddev it's being compared
		// against ("masking"), which for small replica counts makes 2
		// standard deviations mathematically unreachable regardless of how
		// extreme the outlier is (with n pods total, a point can never be
		// more than sqrt(n-1) population-stddev from a mean that includes
		// it -- for n=3 that cap is ~1.41, always below this threshold).
		others := make([]float64, 0, len(loads)-1)
		for i := range pods {
			others = others[:0]
			for j, l := range loads {
				if j != i {
					others = append(others, l)
				}
			}
			mean, stddev := meanStdDev(others)
			gap := loads[i] - mean
			// Both conditions must hold -- see loadOutlierMinGap for why
			// the stddev-scaled threshold alone is unreliable at small
			// replica counts and against tied peers.
			outlier := gap > loadOutlierStdDevThreshold*stddev && gap >= loadOutlierMinGap
			mark(pods[i], outlier)
			if outlier {
				klog.InfoS("pod flagged as a load outlier: running+waiting request count is a statistical outlier among its replicas",
					"pod", pods[i].Name, "model", model, "load", loads[i], "peer_mean", mean, "peer_stddev", stddev, "gap", gap, "min_gap", loadOutlierMinGap)
			}
		}
	}

	for _, v := range verdicts {
		if changed := b.SetLoadExcluded(v.pod, v.outlier); changed && !v.outlier && onRecovered != nil {
			onRecovered(v.pod)
		}
	}
}

// withoutQuarantined drops the pods IsQuarantined reports. The cache stops
// scraping a quarantined pod, so the load it last reported is as old as the
// quarantine; as a peer it would skew the mean and stddev the live pods are
// scored against, and count toward loadOutlierMinPods. Such a pod gets no
// verdict from the sweep: its load exclusion is left as it was until the
// quarantine ends and its metrics are fresh again.
//
// Only quarantine is dropped. A load-excluded pod or one in an ordinary backoff
// is still scraped, so its numbers are current, and a load-excluded pod has to
// stay scored for the sweep to ever lift its exclusion.
//
// The result shares pods' backing array; pods must not be used afterwards.
func (b *PodBlacklist) withoutQuarantined(pods []*v1.Pod) []*v1.Pod {
	if b.tracked.Load() == 0 {
		return pods
	}
	kept := pods[:0]
	for _, pod := range pods {
		if !b.IsQuarantined(pod) {
			kept = append(kept, pod)
		}
	}
	return kept
}

// meanStdDev returns the population mean and standard deviation of values.
func meanStdDev(values []float64) (mean, stddev float64) {
	n := float64(len(values))
	if n == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean = sum / n

	var variance float64
	for _, v := range values {
		d := v - mean
		variance += d * d
	}
	variance /= n
	return mean, math.Sqrt(variance)
}
