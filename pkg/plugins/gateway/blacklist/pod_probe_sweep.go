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
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// StartProbeSweep starts a background goroutine that every
// podProbeSweepInterval probes every routable pod, except prefill and decode
// pods, with the same check the failure breaker uses to confirm a trip
// (PodBlacklist.probe): GET /health must be 2xx and, for vllm/sglang pods,
// GET /v1/models must return a JSON list naming the model.
//
// The request-driven breaker only reacts to pods that are being sent traffic
// and failing on it. This sweep also catches a pod that has gone bad while
// idle, or a stale registration whose address is gone or now belongs to
// another process, and blacklists it proactively:
//
//   - a pod found dead (GET /health failing, or GET /v1/models answering 2xx
//     with a body that is not a JSON model list) for podProbeFailureIterations
//     sweeps in a row is blacklisted for podProbeDeadBlacklistDuration (one
//     hour), renewed while it stays dead;
//   - a pod that is only degraded for that many sweeps gets the ordinary
//     breaker backoff.
//
// A pod that fails is probed again on the next sweeps, so one blip does not
// exclude it. Pods the breaker already excludes are probed too: that is how a
// stale entry stuck in open/half-open backoff is moved to the long blacklist
// instead of being handed a real half-open request every few minutes. See
// PodBlacklist.RecordProbeResult.
func (b *PodBlacklist) StartProbeSweep(src PodSource, stopCh <-chan struct{}) {
	if !Enabled {
		return
	}
	ticker := time.NewTicker(podProbeSweepInterval)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				klog.Errorf("pod probe sweep goroutine panic recovered: %v", r)
			}
		}()
		for {
			select {
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							klog.Errorf("panic recovered during pod probe sweep: %v", r)
						}
					}()
					b.sweepPodProbes(src)
				}()
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

type podProbeTarget struct {
	pod    *v1.Pod
	model  string
	engine string
	addr   string
}

// sweepPodProbes probes each target with at most podProbeConcurrency probes in
// flight and feeds every outcome to the pod blacklist.
func (b *PodBlacklist) sweepPodProbes(src PodSource) {
	targets := b.podProbeTargets(src)
	if len(targets) == 0 {
		return
	}

	sem := make(chan struct{}, podProbeConcurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		sem <- struct{}{}
		wg.Add(1)
		go func(t podProbeTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					klog.Errorf("panic recovered while probing pod %s: %v", podDesc(t.pod, t.addr, t.model), r)
				}
			}()
			result, failure := b.probe(t.addr, t.model, t.engine, podProbeTimeout)
			b.RecordProbeResult(t.pod, result, failure)
		}(t)
	}
	wg.Wait()
}

// isPDRolePod reports whether pod is the prefill or decode leg of a disaggregated
// (PD) deployment, by the same role label the PD router groups pods by.
func isPDRolePod(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}
	switch pod.Labels[pd.RoleLabel] {
	case pd.RolePrefill, pd.RoleDecode:
		return true
	}
	return false
}

// podProbeTargets lists each routable pod once, however many models it serves
// (a base model and its LoRA adapters share pods), with the address a request
// would reach it on. Prefill and decode pods are not probed.
func (b *PodBlacklist) podProbeTargets(src PodSource) []podProbeTarget {
	seen := make(map[string]struct{})
	var targets []podProbeTarget
	for _, model := range src.ListModels() {
		podsArr, err := src.ListPodsByModel(model)
		if err != nil || podsArr == nil {
			continue
		}
		for _, pod := range utils.FilterRoutablePods(podsArr.All()) {
			if isPDRolePod(pod) {
				continue
			}
			key := PodKey(pod)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			// The pod's own served model, not a LoRA adapter name it also answers to.
			served := pod.Labels[constants.ModelLabelName]
			if served == "" {
				served = model
			}
			addr := types.PodTargetAddress(pod, served, "")
			if addr == "" {
				continue
			}
			targets = append(targets, podProbeTarget{
				pod:    pod,
				model:  served,
				engine: pod.Labels[constants.ModelLabelEngine],
				addr:   addr,
			})
		}
	}
	return targets
}
