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

package podautoscaler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// An elastic expert parallel (EP) deployment adds and removes vLLM data
// parallel ranks at runtime through the engine endpoints and the DP
// coordinator request-wave protocol (issue #2288). This file implements the
// observe-only phase: it reads the engine scaling state and records it in the
// PodAutoscaler status without changing any replica decision.
//
// vLLM does not expose the coordinator request-wave state over HTTP today, so
// only /is_scaling_elastic_ep is observed here.

const (
	// elasticEPEnableFlag enables elastic expert parallelism in vLLM.
	elasticEPEnableFlag = "--enable-elastic-ep"
	// elasticEPMaxDPFlag caps the elastic EP data parallel size and implies
	// elastic EP is enabled even when the boolean flag is omitted.
	elasticEPMaxDPFlag = "--elastic-ep-max-dp-size"
	// elasticEPProbePath is the vLLM endpoint that reports the scaling state.
	elasticEPProbePath = "/is_scaling_elastic_ep"
	// elasticEPBusyMarker appears in the 503 response vLLM returns for every
	// request, this probe included, while a scaling commit is in flight.
	elasticEPBusyMarker = "currently scaling"

	// elasticEPProbeTimeout bounds a single engine probe.
	elasticEPProbeTimeout = 2 * time.Second
	// elasticEPObservationTimeout bounds the whole observation so that slow
	// engines cannot consume the reconcile budget.
	elasticEPObservationTimeout = 3 * time.Second
	// elasticEPMaxConcurrentProbes bounds how many probes run in parallel.
	elasticEPMaxConcurrentProbes = 8
	// elasticEPProbeMaxBodySize caps how much of a probe response is read.
	elasticEPProbeMaxBodySize = 4 << 10
	// elasticEPErrorBodyMaxLen caps how much response body is echoed in logs.
	elasticEPErrorBodyMaxLen = 120
)

// elasticEPProbeOutcome is the result of one scaling state probe.
type elasticEPProbeOutcome int

const (
	// elasticEPProbeUnavailable means the engine did not report a usable state.
	elasticEPProbeUnavailable elasticEPProbeOutcome = iota
	// elasticEPProbeIdle means the engine reported a scaling state of false.
	elasticEPProbeIdle
	// elasticEPProbeScaling means the engine is scaling, or reported a scaling
	// commit in flight.
	elasticEPProbeScaling
)

// elasticEPProber probes engine pods for the elastic EP scaling state.
type elasticEPProber struct {
	client *http.Client
}

// newElasticEPProber builds a prober with the default request timeout.
func newElasticEPProber() *elasticEPProber {
	return &elasticEPProber{
		client: &http.Client{Timeout: elasticEPProbeTimeout},
	}
}

// probe asks one engine for its scaling state. The vLLM endpoint answers 200
// with {"is_scaling_elastic_ep": bool} while idle. A scaling commit blocks all
// HTTP requests with a 503, so that response also maps to scaling. Anything
// else is unavailable and must not be read as "not scaling".
func (p *elasticEPProber) probe(ctx context.Context, podIP string, port int32) (elasticEPProbeOutcome, error) {
	url := fmt.Sprintf("http://%s%s", net.JoinHostPort(podIP, strconv.Itoa(int(port))), elasticEPProbePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return elasticEPProbeUnavailable, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return elasticEPProbeUnavailable, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, elasticEPProbeMaxBodySize))
	if err != nil {
		return elasticEPProbeUnavailable, fmt.Errorf("read scaling state response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var payload struct {
			IsScalingElasticEP bool `json:"is_scaling_elastic_ep"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return elasticEPProbeUnavailable, fmt.Errorf("decode scaling state: %w", err)
		}
		if payload.IsScalingElasticEP {
			return elasticEPProbeScaling, nil
		}
		return elasticEPProbeIdle, nil
	case http.StatusServiceUnavailable:
		if strings.Contains(strings.ToLower(string(body)), elasticEPBusyMarker) {
			return elasticEPProbeScaling, nil
		}
		return elasticEPProbeUnavailable, fmt.Errorf("service unavailable: %s", truncateElasticEPBody(body))
	default:
		return elasticEPProbeUnavailable, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, truncateElasticEPBody(body))
	}
}

// elasticEPEngineContainer returns the engine container that enables elastic
// EP on the pod. Enablement is read from the container command line;
// enablement through a vLLM --config file is not detected today.
func elasticEPEngineContainer(pod *corev1.Pod) (*corev1.Container, bool) {
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		for _, token := range containerCommandLine(container) {
			if token == elasticEPEnableFlag ||
				token == elasticEPMaxDPFlag ||
				strings.HasPrefix(token, elasticEPMaxDPFlag+"=") {
				return container, true
			}
		}
	}
	return nil, false
}

// containerCommandLine returns the container command and arguments as one list.
func containerCommandLine(container *corev1.Container) []string {
	tokens := make([]string, 0, len(container.Command)+len(container.Args))
	tokens = append(tokens, container.Command...)
	tokens = append(tokens, container.Args...)
	return tokens
}

// elasticEPEnginePort returns the engine HTTP port to probe for the pod. It
// follows the Aibrix port convention first (the model.aibrix.ai/port label),
// then falls back to the container command line and the declared container
// ports. The data parallel ranks share the engine API server, so only the
// first port of the convention answers the scaling state probe.
func elasticEPEnginePort(pod *corev1.Pod, container *corev1.Container) (int32, bool) {
	if value, ok := pod.Labels[constants.ModelLabelPort]; ok {
		if port, ok := parseElasticEPPort(value); ok {
			return port, true
		}
	}
	return elasticEPPortFromContainer(container)
}

// elasticEPPortFromContainer resolves the engine HTTP port from the --port
// argument, the VLLM_PORT env, and the declared container ports.
func elasticEPPortFromContainer(container *corev1.Container) (int32, bool) {
	tokens := containerCommandLine(container)
	for i, token := range tokens {
		if token == "--port" && i+1 < len(tokens) {
			if port, ok := parseElasticEPPort(tokens[i+1]); ok {
				return port, true
			}
		}
		if value, found := strings.CutPrefix(token, "--port="); found {
			if port, ok := parseElasticEPPort(value); ok {
				return port, true
			}
		}
	}
	for _, env := range container.Env {
		if env.Name == "VLLM_PORT" {
			if port, ok := parseElasticEPPort(env.Value); ok {
				return port, true
			}
		}
	}
	for _, port := range container.Ports {
		if port.Protocol == corev1.ProtocolUDP || port.ContainerPort < 1 || port.ContainerPort > 65535 {
			continue
		}
		return port.ContainerPort, true
	}
	return 0, false
}

// parseElasticEPPort parses a TCP port in the valid range.
func parseElasticEPPort(value string) (int32, bool) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return int32(port), true
}

// elasticEPProbeTarget is one engine pod selected for probing.
type elasticEPProbeTarget struct {
	podName string
	ip      string
	port    int32
}

// elasticEPScalingState classifies one observation pass over the engine pods.
type elasticEPScalingState int

const (
	// elasticEPScalingIncomplete means at least one engine pod did not answer or
	// could not be probed, so the previous status is kept: a missing answer must
	// not be read as "not scaling".
	elasticEPScalingIncomplete elasticEPScalingState = iota
	// elasticEPScalingAbsent means the scale target runs no engine pod with
	// elastic EP enabled, so the recorded status no longer describes it.
	elasticEPScalingAbsent
	// elasticEPScalingComplete means every engine pod answered and the status
	// below is a fresh observation.
	elasticEPScalingComplete
)

// elasticEPScalingObservation is the outcome of one observation pass.
type elasticEPScalingObservation struct {
	state  elasticEPScalingState
	status *autoscalingv1alpha1.ElasticEPScalingStatus
}

// observeElasticEPScaling probes the elastic-EP-enabled engine pods among the
// pods backing the scale target. The engines publish the scaling state on
// their API server, so readiness is not part of the selection: a scaling
// commit answers 503 to every request, including the kubelet readiness probe,
// and those are exactly the pods to observe. The observation is complete only
// when every engine pod answered.
func (r *PodAutoscalerReconciler) observeElasticEPScaling(
	ctx context.Context,
	pods []corev1.Pod,
) elasticEPScalingObservation {
	engines := 0
	targets := make([]elasticEPProbeTarget, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		container, enabled := elasticEPEngineContainer(pod)
		if !enabled {
			continue
		}
		engines++
		port, resolved := elasticEPEnginePort(pod, container)
		if !resolved || pod.Status.PodIP == "" {
			klog.V(4).InfoS("Elastic EP engine pod cannot be probed",
				"pod", klog.KObj(pod), "podIP", pod.Status.PodIP, "portResolved", resolved)
			continue
		}
		targets = append(targets, elasticEPProbeTarget{podName: pod.Name, ip: pod.Status.PodIP, port: port})
	}
	if engines == 0 {
		return elasticEPScalingObservation{state: elasticEPScalingAbsent}
	}

	prober := r.elasticEPProber
	if prober == nil {
		klog.V(4).InfoS("Skipping elastic EP scaling observation, the reconciler has no prober configured")
		return elasticEPScalingObservation{state: elasticEPScalingIncomplete}
	}

	probeCtx, cancel := context.WithTimeout(ctx, elasticEPObservationTimeout)
	defer cancel()

	observed := make([]bool, len(targets))
	scaling := make([]bool, len(targets))
	sem := make(chan struct{}, elasticEPMaxConcurrentProbes)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-probeCtx.Done():
				return
			}

			outcome, probeErr := prober.probe(probeCtx, targets[index].ip, targets[index].port)
			if outcome == elasticEPProbeUnavailable {
				klog.V(4).InfoS("Elastic EP scaling probe did not return a state",
					"pod", targets[index].podName, "port", targets[index].port, "err", probeErr)
				return
			}
			observed[index] = true
			scaling[index] = outcome == elasticEPProbeScaling
		}(i)
	}
	wg.Wait()

	observedEngines := 0
	scalingEngines := 0
	for i := range targets {
		if observed[i] {
			observedEngines++
		}
		if scaling[i] {
			scalingEngines++
		}
	}
	if observedEngines < engines {
		return elasticEPScalingObservation{state: elasticEPScalingIncomplete}
	}

	return elasticEPScalingObservation{
		state: elasticEPScalingComplete,
		status: &autoscalingv1alpha1.ElasticEPScalingStatus{
			InProgress:      scalingEngines > 0,
			ObservedEngines: int32(observedEngines),
			ScalingEngines:  int32(scalingEngines),
		},
	}
}

// mergeElasticEPScalingObservation folds one observation pass into the
// previously recorded status. An absent observation clears the field, an
// incomplete pass keeps it, and a complete pass merges the fresh counts.
func mergeElasticEPScalingObservation(
	previous *autoscalingv1alpha1.ElasticEPScalingStatus,
	observation elasticEPScalingObservation,
	now metav1.Time,
) *autoscalingv1alpha1.ElasticEPScalingStatus {
	switch observation.state {
	case elasticEPScalingAbsent:
		return nil
	case elasticEPScalingComplete:
		return mergeElasticEPScalingStatus(previous, observation.status, now)
	default:
		return previous
	}
}

// mergeElasticEPScalingStatus folds a fresh observation into the previously
// recorded status. LastTransitionTime only moves when InProgress changes, so
// steady state observations do not rewrite the status on every reconcile.
func mergeElasticEPScalingStatus(
	previous *autoscalingv1alpha1.ElasticEPScalingStatus,
	observed *autoscalingv1alpha1.ElasticEPScalingStatus,
	now metav1.Time,
) *autoscalingv1alpha1.ElasticEPScalingStatus {
	merged := observed.DeepCopy()
	if previous != nil && previous.InProgress == observed.InProgress && previous.LastTransitionTime != nil {
		merged.LastTransitionTime = previous.LastTransitionTime
		return merged
	}
	merged.LastTransitionTime = &now
	return merged
}

// truncateElasticEPBody keeps probe error messages small.
func truncateElasticEPBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > elasticEPErrorBodyMaxLen {
		return text[:elasticEPErrorBodyMaxLen] + "..."
	}
	return text
}
