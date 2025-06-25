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

package routingalgorithms

import (
	"fmt"
	"math"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var (
	ErrorNoAvailablePod = fmt.Errorf("no pod available")
)

// leastLoadRouter prioritizes dispatching requests to pod of the least load, and supports both push(default) and pull mode.
// The definition of load depends on input LoadProvider (push mode) or CappedLoadProvider (pull mode).
//
// Below is a comparison of pull mode and default push mode:
//
//	Push mode: The router dispatches requests to the server, possibly overloading the server.
//	Pull mode: The server pulls requests from the router based on the server's capacity.
//
// With profile support, the gateway now has server capacity knowledge and can achieve pull mode within the gateway.
type leastLoadRouter struct {
	provider       cache.LoadProvider
	cappedProvider cache.CappedLoadProvider
	pulling        bool // Flag to indicate the leastLoadRouter will run in pull mode.
}

// NewLeastLoadRouter creates leastLoadRouter instance in the push mode.
func NewLeastLoadRouter(provider cache.LoadProvider) (types.Router, error) {
	return &leastLoadRouter{
		provider: provider,
	}, nil
}

// NewLeastLoadRouter creates leastLoadRouter instance in the pull mode.
func NewLeastLoadPullingRouter(provider cache.CappedLoadProvider) (types.Router, error) {
	return &leastLoadRouter{
		provider:       provider,
		cappedProvider: provider,
		pulling:        true,
	}, nil
}

func (r *leastLoadRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	if pods.Len() == 0 {
		return "", ErrorNoAvailablePod
	}

	var targetPod *v1.Pod
	minUtil := math.MaxFloat64
	if r.pulling {
		minUtil = r.cappedProvider.Cap()
	}

	lastErr := ErrorNoAvailablePod // The default error keeps if all pods are not ready.
	for _, pod := range pods.All() {
		if pod.Status.PodIP == "" {
			// Pod not ready.
			continue
		}

		util, err := r.provider.GetUtilization(ctx, pod)
		if err != nil {
			lastErr = r.updateError(lastErr, err)
			klog.ErrorS(err, "Skipped pod due to fail to get utilization in leastLoadRouter", "pod", pod.Name)
			continue
		}

		klog.V(5).Infof("pod: %v, podIP: %v, util: %.2f",
			pod.Name, pod.Status.PodIP, util)

		var consumption float64
		if r.pulling {
			consumption, err = r.provider.GetConsumption(ctx, pod)
			if err == cache.ErrorSLOFailureRequest {
				lastErr = r.updateError(lastErr, err)
				klog.V(4).InfoS("Skipped pod due to SLOFailureRequest in leastLoadRouter", "pod", pod.Name, "request", ctx.RequestID)
				continue
			} else if err != nil {
				lastErr = r.updateError(lastErr, err)
				klog.ErrorS(err, "Skipped pod due to fail to get consumption in leastLoadRouter", "pod", pod.Name)
				continue
			}
		}

		util += consumption // 0 if not in pulling mode
		if r.pulling && util > r.cappedProvider.Cap() {
			lastErr = r.updateError(lastErr, cache.ErrorLoadCapacityReached)
		} else if util <= minUtil {
			minUtil = util
			targetPod = pod
		}
	}

	// No fallback
	if targetPod == nil {
		return "", lastErr
	}

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *leastLoadRouter) updateError(last error, err error) error {
	// ErrorLoadCapacityReached is a temporary error.
	if last == cache.ErrorLoadCapacityReached {
		return last
	}

	return err
}
