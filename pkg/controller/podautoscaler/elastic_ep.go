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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	podutils "github.com/vllm-project/aibrix/pkg/utils"
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

// defaultElasticEPProber is shared by all reconcilers; http.Client is safe for
// concurrent use.
var defaultElasticEPProber = newElasticEPProber()

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

// elasticEPEnginePorts returns the engine HTTP ports to probe for the pod. It
// follows the Aibrix port convention first (the model.aibrix.ai/port label
// with the data-parallel-size env), then falls back to the container command
// line and the declared container ports.
func elasticEPEnginePorts(pod *corev1.Pod, container *corev1.Container) []int32 {
	if ports := podutils.GetPortsForPod(pod); len(ports) > 0 {
		probePorts := make([]int32, 0, len(ports))
		for _, port := range ports {
			probePorts = append(probePorts, int32(port))
		}
		return probePorts
	}

	if port, ok := elasticEPPortFromContainer(container); ok {
		return []int32{port}
	}
	return nil
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
	ports   []int32
}

// observeElasticEPScaling probes the elastic-EP-enabled engine pods of the
// scale target and returns the observed scaling state. It returns nil when no
// engine reported a usable state or when the target has no elastic EP pods, so
// the previously recorded state is preserved instead of being replaced by a
// "not scaling" guess.
func (r *PodAutoscalerReconciler) observeElasticEPScaling(
	ctx context.Context,
	pa *autoscalingv1alpha1.PodAutoscaler,
	scaleObject *unstructured.Unstructured,
) *autoscalingv1alpha1.ElasticEPScalingStatus {
	pods, err := r.getPodsForScale(ctx, pa, scaleObject)
	if err != nil {
		klog.V(4).InfoS("Skipping elastic EP scaling observation, failed to list target pods",
			"PodAutoscaler", klog.KObj(pa), "err", err)
		return nil
	}

	targets := make([]elasticEPProbeTarget, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if pod.Status.PodIP == "" || !podutils.IsPodReady(pod) {
			continue
		}
		container, enabled := elasticEPEngineContainer(pod)
		if !enabled {
			continue
		}
		ports := elasticEPEnginePorts(pod, container)
		if len(ports) == 0 {
			klog.V(4).InfoS("Skipping elastic EP scaling observation for pod, unable to resolve the engine port",
				"pod", klog.KObj(pod))
			continue
		}
		targets = append(targets, elasticEPProbeTarget{podName: pod.Name, ip: pod.Status.PodIP, ports: ports})
	}
	if len(targets) == 0 {
		return nil
	}

	prober := r.elasticEPProber
	if prober == nil {
		prober = defaultElasticEPProber
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
			sem <- struct{}{}
			defer func() { <-sem }()
			if probeCtx.Err() != nil {
				return
			}

			observed[index], scaling[index] = probeElasticEPTarget(probeCtx, prober, targets[index])
		}(i)
	}
	wg.Wait()

	observedEngines := int32(0)
	scalingEngines := int32(0)
	for i := range targets {
		if !observed[i] {
			continue
		}
		observedEngines++
		if scaling[i] {
			scalingEngines++
		}
	}
	if observedEngines == 0 {
		return nil
	}

	return &autoscalingv1alpha1.ElasticEPScalingStatus{
		InProgress:      scalingEngines > 0,
		ObservedEngines: observedEngines,
		ScalingEngines:  scalingEngines,
	}
}

// probeElasticEPTarget probes the resolved ports of one pod. The pod counts as
// observed when at least one port answers, and counts as scaling when any
// answer reports scaling.
func probeElasticEPTarget(ctx context.Context, prober *elasticEPProber, target elasticEPProbeTarget) (observed bool, scaling bool) {
	for _, port := range target.ports {
		outcome, probeErr := prober.probe(ctx, target.ip, port)
		if outcome == elasticEPProbeUnavailable {
			klog.V(4).InfoS("Elastic EP scaling probe did not return a state",
				"pod", target.podName, "port", port, "err", probeErr)
			if ctx.Err() != nil {
				return observed, scaling
			}
			continue
		}
		observed = true
		if outcome == elasticEPProbeScaling {
			return observed, true
		}
	}
	return observed, scaling
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
