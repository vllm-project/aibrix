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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const defaultRoutingInstructions = "Select the best replica by jointly considering node topology, prefill/decode " +
	"role, current request load, engine utilization, KV cache pressure, and trusted policy context. " +
	"Prefer a healthy, suitable, less-loaded candidate."

var (
	errUpstreamUnavailable = errors.New("jev-compatible service unavailable")
	errUpstreamInvalid     = errors.New("invalid Jev-compatible response")
)

type systemOneState struct {
	Model         string         `json:"model"`
	PolicyMode    string         `json:"policyMode"`
	PolicyContext *PolicyContext `json:"policyContext,omitempty"`
	Candidates    []Candidate    `json:"candidates"`
}

type systemOneQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type systemOneRequest struct {
	State     systemOneState               `json:"state"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

type systemOneAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type systemOneResponse struct {
	Answers map[string]systemOneAnswer `json:"answers"`
}

type replicaSelection struct {
	Candidate     Candidate
	Confidence    float64
	Probabilities map[string]float64
}

type jevClient struct {
	endpoint     string
	apiKey       string
	instructions string
	httpClient   *http.Client
}

func buildSystemOneRequest(req ReplicaSelectionRequest, instructions string) systemOneRequest {
	candidates := append([]Candidate(nil), req.Spec.Candidates...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	criteria := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		criteria[candidate.ID] = describeCandidate(candidate)
	}
	return systemOneRequest{
		State: systemOneState{
			Model:         req.Spec.Model,
			PolicyMode:    req.Spec.PolicyMode,
			PolicyContext: req.Spec.PolicyContext,
			Candidates:    candidates,
		},
		Questions: map[string]systemOneQuestion{
			"replica": {Type: "choice", Instructions: instructions, Criteria: criteria},
		},
	}
}

func describeCandidate(candidate Candidate) string {
	parts := []string{fmt.Sprintf("ports=%v", candidate.Ports)}
	keys := make([]string, 0, len(candidate.Attributes))
	for key := range candidate.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, key+"="+candidate.Attributes[key])
	}
	if candidate.Metrics != nil {
		if candidate.Metrics.RunningRequests != nil {
			parts = append(parts, fmt.Sprintf("runningRequests=%d", *candidate.Metrics.RunningRequests))
		}
		if candidate.Metrics.EngineUtilization != nil {
			parts = append(parts, fmt.Sprintf("engineUtilization=%g", *candidate.Metrics.EngineUtilization))
		}
		if candidate.Metrics.KVCacheUsage != nil {
			parts = append(parts, fmt.Sprintf("kvCacheUsage=%g", *candidate.Metrics.KVCacheUsage))
		}
	}
	return strings.Join(parts, ", ")
}

func (c jevClient) selectReplica(ctx context.Context, req ReplicaSelectionRequest) (replicaSelection, error) {
	payload, err := json.Marshal(buildSystemOneRequest(req, c.instructions))
	if err != nil {
		return replicaSelection{}, fmt.Errorf("%w: encode request: %v", errUpstreamInvalid, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return replicaSelection{}, fmt.Errorf("%w: create request: %v", errUpstreamInvalid, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return replicaSelection{}, fmt.Errorf("%w: %v", errUpstreamUnavailable, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return replicaSelection{}, fmt.Errorf("%w: HTTP %d", errUpstreamUnavailable, resp.StatusCode)
	}
	const maxResponseBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return replicaSelection{}, fmt.Errorf("%w: read response: %v", errUpstreamInvalid, err)
	}
	if len(body) > maxResponseBytes {
		return replicaSelection{}, fmt.Errorf("%w: response exceeds %d bytes", errUpstreamInvalid, maxResponseBytes)
	}
	var decoded systemOneResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return replicaSelection{}, fmt.Errorf("%w: decode response: %v", errUpstreamInvalid, err)
	}
	answer, ok := decoded.Answers["replica"]
	if !ok || answer.Choice == "" {
		return replicaSelection{}, fmt.Errorf("%w: replica choice is missing", errUpstreamInvalid)
	}
	for _, candidate := range req.Spec.Candidates {
		if candidate.ID == answer.Choice {
			return replicaSelection{
				Candidate:     candidate,
				Confidence:    answer.Confidence,
				Probabilities: answer.Probabilities,
			}, nil
		}
	}
	return replicaSelection{}, fmt.Errorf(
		"%w: choice %q is outside the candidate snapshot",
		errUpstreamInvalid,
		answer.Choice,
	)
}
