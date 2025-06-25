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
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// leastLoadRouter prioritizes dispatching requests to pod of the most load. The definition of load depends on input CappedLoadProvider.
type packLoadRouter struct {
	provider cache.CappedLoadProvider
}

// NewPackLoadRouter creates packLoadRouter instance.
func NewPackLoadRouter(provider cache.CappedLoadProvider) (types.Router, error) {
	return &packLoadRouter{
		provider: provider,
	}, nil
}

func (r *packLoadRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	klog.V(4).InfoS("Routing using packLoadRouter", "candidates", pods.Len(), "requestID", ctx.RequestID)

	if pods.Len() == 0 {
		return "", ErrorNoAvailablePod
	}

	var targetPod *v1.Pod
	capUtil := r.provider.Cap()
	maxUtil := 0.0

	lastErr := ErrorNoAvailablePod // The default error keeps if all pods are not ready.
	for _, pod := range pods.All() {
		if pod.Status.PodIP == "" {
			// Pod not ready.
			continue
		}

		util, err := r.provider.GetUtilization(ctx, pod)
		if err != nil {
			lastErr = r.updateError(lastErr, err)
			klog.ErrorS(err, "Skipped pod due to fail to get utilization in packLoadRouter", "pod", pod.Name)
			continue
		}

		consumption, err := r.provider.GetConsumption(ctx, pod)
		if err == cache.ErrorSLOFailureRequest {
			lastErr = r.updateError(lastErr, err)
			klog.V(4).InfoS("Skipped pod due to SLOFailureRequest in packLoadRouter", "pod", pod.Name, "request", ctx.RequestID)
			continue
		} else if err != nil {
			lastErr = r.updateError(lastErr, err)
			klog.ErrorS(err, "Skipped pod due to fail to get consumption in packLoadRouter", "pod", pod.Name)
			continue
		}

		klog.V(5).Infof("pod: %v, podIP: %v, consumption: %.2f, util: %.2f", pod.Name, pod.Status.PodIP, consumption, util)

		util += consumption
		if util > capUtil {
			lastErr = r.updateError(lastErr, cache.ErrorLoadCapacityReached)
		} else if util > maxUtil {
			maxUtil = util
			targetPod = pod
		}
	}

	// Use fallback if no valid metrics
	if targetPod == nil {
		return "", lastErr
	}

	klog.V(4).Infof("targetPod: %s(%s)", targetPod.Name, targetPod.Status.PodIP)
	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (r *packLoadRouter) updateError(last error, err error) error {
	// ErrorLoadCapacityReached is a temporary error.
	if last == cache.ErrorLoadCapacityReached {
		return last
	}

	return err
}
