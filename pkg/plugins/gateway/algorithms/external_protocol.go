/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package routingalgorithms

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

const (
	externalAPIVersion   = "routing.aibrix.ai/v1alpha1"
	externalRequestKind  = "ReplicaSelectionRequest"
	externalResponseKind = "ReplicaSelectionResponse"
	externalMediaType    = "application/vnd.aibrix.external-routing+json;version=v1alpha1"

	externalDecisionSelected   = "Selected"
	externalDecisionNoDecision = "NoDecision"
	externalDecisionDenied     = "Denied"
)

type externalMetadata struct {
	RequestID  string `json:"requestId"`
	DecisionID string `json:"decisionId,omitempty"`
}

type externalPolicyContext struct {
	Attributes map[string]string `json:"attributes,omitempty"`
}

type externalCandidateMetrics struct {
	RunningRequests   *int64   `json:"runningRequests,omitempty"`
	EngineUtilization *float64 `json:"engineUtilization,omitempty"`
	KVCacheUsage      *float64 `json:"kvCacheUsage,omitempty"`
}

type externalCandidate struct {
	ID         string                    `json:"id"`
	Ports      []int                     `json:"ports"`
	Attributes map[string]string         `json:"attributes,omitempty"`
	Metrics    *externalCandidateMetrics `json:"metrics,omitempty"`
}

type externalRequestSpec struct {
	Model         string                 `json:"model"`
	PolicyMode    externalPolicyMode     `json:"policyMode"`
	PolicyContext *externalPolicyContext `json:"policyContext,omitempty"`
	Candidates    []externalCandidate    `json:"candidates"`
}

type externalDecisionRequest struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   externalMetadata    `json:"metadata"`
	Spec       externalRequestSpec `json:"spec"`
}

type externalTarget struct {
	ID   *string `json:"id"`
	Port *int    `json:"port,omitempty"`
}

type externalResponseMetadata struct {
	RequestID  *string `json:"requestId"`
	DecisionID *string `json:"decisionId,omitempty"`
}

type externalResponseStatus struct {
	Decision *string         `json:"decision"`
	Target   *externalTarget `json:"target,omitempty"`
	Reason   *string         `json:"reason,omitempty"`
}

type externalDecisionResponse struct {
	APIVersion *string                   `json:"apiVersion"`
	Kind       *string                   `json:"kind"`
	Metadata   *externalResponseMetadata `json:"metadata"`
	Status     *externalResponseStatus   `json:"status"`
}

type externalCandidateSnapshot struct {
	pod   *v1.Pod
	ports map[int]struct{}
}

type externalValidatedDecision struct {
	decision   string
	targetPod  *v1.Pod
	targetPort int
	decisionID string
	reason     string
}

func buildExternalDecisionRequest(cfg externalRouterConfig, metricCache cache.Cache, ctx *types.RoutingContext, pods types.PodList) (externalDecisionRequest, map[string]externalCandidateSnapshot, error) {
	if ctx == nil || ctx.RequestID == "" || len(ctx.RequestID) > 256 || !utf8.ValidString(ctx.RequestID) {
		return externalDecisionRequest{}, nil, errors.New("external request ID must be non-empty valid UTF-8 up to 256 bytes")
	}
	if ctx.Model == "" {
		return externalDecisionRequest{}, nil, errors.New("external request model is empty")
	}

	readyPods := append([]*v1.Pod(nil), pods.All()...)
	sort.Slice(readyPods, func(i, j int) bool {
		return externalPodID(readyPods[i]) < externalPodID(readyPods[j])
	})

	var running map[string]int64
	if _, enabled := cfg.candidateMetrics[externalMetricRunningRequests]; enabled && metricCache != nil {
		counts, err := metricCache.GetPodsRunningRequests(readyPods)
		if err == nil {
			running = counts
		}
	}

	candidates := make([]externalCandidate, 0, len(readyPods))
	snapshots := make(map[string]externalCandidateSnapshot, len(readyPods))
	for _, pod := range readyPods {
		id := externalPodID(pod)
		if pod == nil || pod.Namespace == "" || pod.Name == "" || len(id) > 256 || !utf8.ValidString(id) {
			return externalDecisionRequest{}, nil, fmt.Errorf("invalid external candidate identity %q", id)
		}
		if _, duplicate := snapshots[id]; duplicate {
			return externalDecisionRequest{}, nil, fmt.Errorf("duplicate external candidate identity %q", id)
		}
		ports := normalizeExternalPorts(utils.GetPortsForPod(pod))
		if len(ports) == 0 {
			return externalDecisionRequest{}, nil, fmt.Errorf("candidate %s has no routable port", id)
		}
		portSet := make(map[int]struct{}, len(ports))
		for _, port := range ports {
			portSet[port] = struct{}{}
		}
		candidate := externalCandidate{
			ID:         id,
			Ports:      ports,
			Attributes: externalCandidateAttributes(cfg.candidateAttributes, pod.Labels),
		}
		candidate.Metrics = externalMetricsForCandidate(cfg, metricCache, ctx.Model, pod, running)
		candidates = append(candidates, candidate)
		snapshots[id] = externalCandidateSnapshot{pod: pod, ports: portSet}
	}
	if len(candidates) == 0 {
		return externalDecisionRequest{}, nil, errors.New("external candidate list is empty")
	}

	trusted := ctx.TrustedPolicyAttributes(cfg.policyAttributes)
	trusted = filterExternalAttributes(trusted)
	var policyContext *externalPolicyContext
	if len(trusted) > 0 {
		policyContext = &externalPolicyContext{Attributes: trusted}
	}
	request := externalDecisionRequest{
		APIVersion: externalAPIVersion,
		Kind:       externalRequestKind,
		Metadata:   externalMetadata{RequestID: ctx.RequestID},
		Spec: externalRequestSpec{
			Model:         ctx.Model,
			PolicyMode:    cfg.policyMode,
			PolicyContext: policyContext,
			Candidates:    candidates,
		},
	}
	return request, snapshots, nil
}

func externalPodID(pod *v1.Pod) string {
	if pod == nil {
		return ""
	}
	return pod.Namespace + "/" + pod.Name
}

func normalizeExternalPorts(raw []int) []int {
	set := make(map[int]struct{}, len(raw))
	for _, port := range raw {
		if port >= 1 && port <= 65535 {
			set[port] = struct{}{}
		}
	}
	ports := make([]int, 0, len(set))
	for port := range set {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func externalCandidateAttributes(allowlist []string, labels map[string]string) map[string]string {
	attributes := make(map[string]string)
	for _, key := range allowlist {
		if value, ok := labels[key]; ok && validExternalAttributeValue(value) {
			attributes[key] = value
		}
	}
	if len(attributes) == 0 {
		return nil
	}
	return attributes
}

func filterExternalAttributes(input map[string]string) map[string]string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	output := make(map[string]string)
	for _, key := range keys {
		value := input[key]
		if externalAttributeKeyRE.MatchString(key) && validExternalAttributeValue(value) && len(output) < 32 {
			output[key] = value
		}
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

func validExternalAttributeValue(value string) bool {
	return utf8.ValidString(value) && len(value) <= 256
}

func externalMetricsForCandidate(cfg externalRouterConfig, metricCache cache.Cache, model string, pod *v1.Pod, running map[string]int64) *externalCandidateMetrics {
	result := &externalCandidateMetrics{}
	populated := false
	if _, enabled := cfg.candidateMetrics[externalMetricRunningRequests]; enabled {
		if value, ok := running[utils.GeneratePodKey(pod.Namespace, pod.Name)]; ok && value >= 0 {
			valueCopy := value
			result.RunningRequests = &valueCopy
			populated = true
		}
	}
	readUtilization := func(configName, metricName string) *float64 {
		if _, enabled := cfg.candidateMetrics[configName]; !enabled || metricCache == nil {
			return nil
		}
		value, err := metricCache.GetMetricValueByPodModel(pod.Name, pod.Namespace, model, metricName)
		if err != nil || value == nil {
			return nil
		}
		simple := value.GetSimpleValue()
		if math.IsNaN(simple) || math.IsInf(simple, 0) || simple < 0 || simple > 1 {
			return nil
		}
		return &simple
	}
	if value := readUtilization(externalMetricEngineUtilization, metrics.EngineUtilization); value != nil {
		result.EngineUtilization = value
		populated = true
	}
	if value := readUtilization(externalMetricKVCacheUsage, metrics.KVCacheUsagePerc); value != nil {
		result.KVCacheUsage = value
		populated = true
	}
	if !populated {
		return nil
	}
	return result
}

//nolint:gocyclo // Decision-dependent required/forbidden fields are validated together as one protocol boundary.
func validateExternalDecision(data []byte, requestID string, policyMode externalPolicyMode, snapshots map[string]externalCandidateSnapshot) (externalValidatedDecision, error) {
	if len(data) == 0 {
		return externalValidatedDecision{}, errors.New("empty external routing response")
	}
	if err := rejectDuplicateJSONMembers(data); err != nil {
		return externalValidatedDecision{}, err
	}
	if err := rejectExplicitNullExternalFields(data); err != nil {
		return externalValidatedDecision{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var response externalDecisionResponse
	if err := decoder.Decode(&response); err != nil {
		return externalValidatedDecision{}, fmt.Errorf("decode external routing response: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return externalValidatedDecision{}, err
	}
	if response.APIVersion == nil || *response.APIVersion != externalAPIVersion ||
		response.Kind == nil || *response.Kind != externalResponseKind ||
		response.Metadata == nil || response.Metadata.RequestID == nil || *response.Metadata.RequestID != requestID ||
		response.Status == nil || response.Status.Decision == nil {
		return externalValidatedDecision{}, errors.New("invalid external routing response envelope")
	}
	if response.Metadata.DecisionID != nil && (!utf8.ValidString(*response.Metadata.DecisionID) || len(*response.Metadata.DecisionID) > 256) {
		return externalValidatedDecision{}, errors.New("invalid external decision ID")
	}
	if response.Status.Reason != nil && (!utf8.ValidString(*response.Status.Reason) || len(*response.Status.Reason) > 256) {
		return externalValidatedDecision{}, errors.New("invalid external decision reason")
	}
	decision := externalValidatedDecision{decision: *response.Status.Decision}
	if response.Metadata.DecisionID != nil {
		decision.decisionID = *response.Metadata.DecisionID
	}
	if response.Status.Reason != nil {
		decision.reason = *response.Status.Reason
	}

	switch decision.decision {
	case externalDecisionSelected:
		if response.Status.Target == nil || response.Status.Target.ID == nil || *response.Status.Target.ID == "" {
			return externalValidatedDecision{}, errors.New("selected response is missing target")
		}
		snapshot, ok := snapshots[*response.Status.Target.ID]
		if !ok {
			return externalValidatedDecision{}, errors.New("external target is outside the candidate snapshot")
		}
		if response.Status.Target.Port == nil {
			if len(snapshot.ports) != 1 {
				return externalValidatedDecision{}, errors.New("external target port is required")
			}
			for port := range snapshot.ports {
				decision.targetPort = port
			}
		} else {
			if _, ok := snapshot.ports[*response.Status.Target.Port]; !ok {
				return externalValidatedDecision{}, errors.New("external target port is outside the candidate snapshot")
			}
			decision.targetPort = *response.Status.Target.Port
		}
		decision.targetPod = snapshot.pod
	case externalDecisionNoDecision:
		if policyMode != PolicyAdvisory || response.Status.Target != nil {
			return externalValidatedDecision{}, errors.New("NoDecision is valid only for Advisory without a target")
		}
	case externalDecisionDenied:
		if policyMode != PolicyAuthoritative || response.Status.Target != nil {
			return externalValidatedDecision{}, errors.New("denied is valid only for Authoritative without a target")
		}
	default:
		return externalValidatedDecision{}, fmt.Errorf("unknown external decision %q", decision.decision)
	}
	return decision, nil
}

func rejectExplicitNullExternalFields(data []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	checkObject := func(raw json.RawMessage, fields ...string) error {
		if len(raw) == 0 {
			return nil
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		for _, field := range fields {
			if value, exists := object[field]; exists && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("external response field %s must be omitted instead of null", field)
			}
		}
		return nil
	}
	if err := checkObject(envelope["metadata"], "decisionId"); err != nil {
		return err
	}
	if err := checkObject(envelope["status"], "target", "reason"); err != nil {
		return err
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(envelope["status"], &status); err == nil {
		if target, exists := status["target"]; exists && !bytes.Equal(bytes.TrimSpace(target), []byte("null")) {
			if err := checkObject(target, "port"); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("external routing response contains trailing JSON")
		}
		return fmt.Errorf("decode trailing external response: %w", err)
	}
	return nil
}

func rejectDuplicateJSONMembers(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode external response token: %w", err)
	}
	if err := walkExternalJSONValue(decoder, token); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("external response contains trailing JSON")
	}
	return nil
}

func walkExternalJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("external JSON object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("external response contains duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := walkExternalJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated external JSON object")
		}
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := walkExternalJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated external JSON array")
		}
	default:
		return errors.New("unexpected external JSON delimiter")
	}
	return nil
}
