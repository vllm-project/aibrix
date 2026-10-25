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

package roleset

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	controllerconst "github.com/vllm-project/aibrix/pkg/controller/constants"
	controllerdrain "github.com/vllm-project/aibrix/pkg/controller/drain"
	"github.com/vllm-project/aibrix/pkg/metrics"
	podutil "github.com/vllm-project/aibrix/pkg/utils"
)

const trafficDrainPoll = time.Second

// This client reads one fresh response, including both request gauges. The normal
// Gateway metrics loop skips draining Pods and its routing counters are best-effort.
var trafficDrainMetrics = metrics.NewEngineMetricsFetcherWithConfig(metrics.EngineMetricsFetcherConfig{
	Timeout: time.Second,
})

func trafficScaleInTimeout(role *orchestrationv1alpha1.RoleSpec) (time.Duration, error) {
	for _, key := range []string{aibrixconst.TrafficDrainProtocolAnnotationKey, aibrixconst.TrafficDrainEpochAnnotationKey, aibrixconst.TrafficScaleInRetryAtAnnotationKey} {
		if _, present := role.Template.Annotations[key]; present {
			return 0, fmt.Errorf("role %s: %s is controller-owned", role.Name, key)
		}
	}
	raw, enabled := role.Template.Annotations[aibrixconst.TrafficScaleInMaxWaitAnnotationKey]
	if !enabled {
		return 0, nil
	}
	if role.Stateful || (role.PodGroupSize != nil && *role.PodGroupSize > 1) {
		return 0, fmt.Errorf("role %s: traffic-aware scale-in requires a stateless, single-Pod role", role.Name)
	}
	if role.UpdateStrategy.Type == orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType {
		return 0, fmt.Errorf("role %s: traffic-aware drain requires Recreate updates", role.Name)
	}
	maxWait, err := time.ParseDuration(raw)
	if err != nil || maxWait <= 0 || role.Drain == nil || role.Drain.TimeoutSeconds == nil || *role.Drain.TimeoutSeconds <= 0 || maxWait <= time.Duration(*role.Drain.TimeoutSeconds)*time.Second {
		return 0, fmt.Errorf("role %s: %s must be a positive duration greater than a positive drain.timeoutSeconds", role.Name, aibrixconst.TrafficScaleInMaxWaitAnnotationKey)
	}
	return maxWait, nil
}

func readTrafficLoad(ctx context.Context, pod *v1.Pod) (float64, error) {
	if pod.UID == "" || pod.Status.PodIP == "" {
		return 0, fmt.Errorf("pod %s has no UID or metrics address", pod.Name)
	}
	port := "8000"
	if value, ok := pod.Labels[aibrixconst.ModelLabelMetricPort]; ok {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 || n > 65535 {
			return 0, fmt.Errorf("pod %s has an invalid metrics port", pod.Name)
		}
		port = strconv.Itoa(n)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return trafficDrainMetrics.FetchSingleModelRequestLoad(ctx, net.JoinHostPort(pod.Status.PodIP, port), metrics.GetEngineType(*pod))
}

// orderTrafficScaleIn only reorders equally eligible, ready Pods. Template and
// readiness priorities still protect rollout surge Pods and maxUnavailable.
func (s *StatelessRoleSyncer) orderTrafficScaleIn(ctx context.Context, pods []*v1.Pod, targetHash string, now time.Time) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	loads := make(map[string]float64, len(pods))
	eligible := make(map[string]bool, len(pods))
	for _, pod := range pods {
		if podutil.IsPodDraining(pod) || !podutil.IsPodReady(pod) {
			continue
		}
		if retry, err := time.Parse(time.RFC3339Nano, pod.Annotations[aibrixconst.TrafficScaleInRetryAtAnnotationKey]); err == nil && now.Before(retry) {
			continue
		}
		var load float64
		var err error
		if protectedTrafficPod(pod) {
			state, readErr := s.trafficGate().Read(ctx, string(pod.UID), pod.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey])
			err = readErr
			load = float64(state.Active)
		} else {
			load, err = readTrafficLoad(ctx, pod)
		}
		if err != nil {
			klog.V(4).InfoS("Cannot observe traffic for scale-in", "pod", pod.Name, "error", err)
			continue
		}
		loads[pod.Name], eligible[pod.Name] = load, true
	}
	cohort := func(pod *v1.Pod) int {
		priority := 0
		if pod.Labels[controllerconst.RoleTemplateHashLabelKey] == targetHash {
			priority += 2
		}
		if podutil.IsPodReady(pod) {
			priority++
		}
		return priority
	}
	for start := 0; start < len(pods); {
		end := start + 1
		for end < len(pods) && cohort(pods[end]) == cohort(pods[start]) {
			end++
		}
		group := pods[start:end]
		sort.SliceStable(group, func(i, j int) bool {
			iOK, jOK := eligible[group[i].Name], eligible[group[j].Name]
			if iOK != jOK {
				return iOK
			}
			return iOK && loads[group[i].Name] < loads[group[j].Name]
		})
		start = end
	}
	return eligible
}

func (s *StatelessRoleSyncer) trafficScaleIn(ctx context.Context, owner *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, pods []*v1.Pod, maxWait time.Duration) (controllerdrain.Result, error) {
	var result controllerdrain.Result
	for _, pod := range pods {
		var r controllerdrain.Result
		var err error
		if protectedTrafficPod(pod) && pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed {
			r, err = s.coordinatedTrafficDelete(ctx, owner, pod, role.Drain, aibrixconst.PodDrainReasonScaleIn)
		} else {
			r, err = controllerdrain.DeletePods(ctx, s.cli, s.recorder, owner, []*v1.Pod{pod}, role.Drain, aibrixconst.PodDrainReasonScaleIn, time.Now())
		}
		result.Merge(r)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func nextTrafficScaleInAttempt(pods []*v1.Pod, cooldown time.Duration, now time.Time) time.Duration {
	delay := cooldown
	for _, pod := range pods {
		retry, err := time.Parse(time.RFC3339Nano, pod.Annotations[aibrixconst.TrafficScaleInRetryAtAnnotationKey])
		if err != nil || !now.Before(retry) {
			return trafficDrainPoll
		}
		delay = min(delay, retry.Sub(now))
	}
	return delay
}
