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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildSystemOneRequestPreservesRoutingContext(t *testing.T) {
	running := int64(3)
	utilization := 0.42
	kv := 0.27
	req := validRequest()
	req.Spec.PolicyContext = &PolicyContext{Attributes: map[string]string{"tenantTier": "gold"}}
	req.Spec.Candidates = []Candidate{
		{
			ID:         "default/pod-b",
			Ports:      []int{8000},
			Attributes: map[string]string{"role": "decode", "topology.kubernetes.io/zone": "zone-b"},
			Metrics:    &CandidateMetrics{RunningRequests: &running, EngineUtilization: &utilization, KVCacheUsage: &kv},
		},
		{ID: "default/pod-a", Ports: []int{9000}, Attributes: map[string]string{"role": "prefill"}},
	}

	got := buildSystemOneRequest(req, "Choose using topology, role, and load.")
	if got.State.Model != "qwen2-7b" || got.State.PolicyMode != "Advisory" {
		t.Fatalf("state = %#v", got.State)
	}
	if got.State.PolicyContext == nil || got.State.PolicyContext.Attributes["tenantTier"] != "gold" {
		t.Fatalf("policy context = %#v", got.State.PolicyContext)
	}
	if len(got.State.Candidates) != 2 || got.State.Candidates[0].ID != "default/pod-a" {
		t.Fatalf("candidates are not sorted: %#v", got.State.Candidates)
	}
	question := got.Questions["replica"]
	if question.Type != "choice" || question.Instructions != "Choose using topology, role, and load." {
		t.Fatalf("question = %#v", question)
	}
	description := question.Criteria["default/pod-b"]
	for _, want := range []string{
		"role=decode",
		"zone-b",
		"runningRequests=3",
		"engineUtilization=0.42",
		"kvCacheUsage=0.27",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("description %q does not contain %q", description, want)
		}
	}
}

func TestJevClientSelectReplica(t *testing.T) {
	var received systemOneRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"answers":{"replica":{"type":"choice","choice":"default/pod-a",` +
				`"confidence":0.72,"probabilities":{"default/pod-a":0.72,"default/pod-b":0.28}}}}`,
		))
	}))
	defer server.Close()

	req := validRequest()
	req.Spec.Candidates = append(req.Spec.Candidates, Candidate{ID: "default/pod-b", Ports: []int{8001}})
	client := jevClient{endpoint: server.URL, apiKey: "secret", instructions: "pick", httpClient: server.Client()}
	got, err := client.selectReplica(context.Background(), req)
	if err != nil {
		t.Fatalf("selectReplica() error = %v", err)
	}
	if got.Candidate.ID != "default/pod-a" || got.Confidence != 0.72 || got.Probabilities["default/pod-b"] != 0.28 {
		t.Fatalf("selection = %#v", got)
	}
	if received.Questions["replica"].Instructions != "pick" {
		t.Fatalf("received request = %#v", received)
	}
}

func TestJevClientRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"non success", http.StatusServiceUnavailable, `{"error":"down"}`, errUpstreamUnavailable},
		{"malformed json", http.StatusOK, `{`, errUpstreamInvalid},
		{"missing answer", http.StatusOK, `{"answers":{}}`, errUpstreamInvalid},
		{"outside snapshot", http.StatusOK, `{"answers":{"replica":{"choice":"default/other"}}}`, errUpstreamInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client := jevClient{endpoint: server.URL, instructions: "pick", httpClient: server.Client()}
			_, err := client.selectReplica(context.Background(), validRequest())
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestJevClientTimeoutIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	defer server.Close()
	client := jevClient{
		endpoint:     server.URL,
		instructions: "pick",
		httpClient:   &http.Client{Timeout: 5 * time.Millisecond},
	}
	_, err := client.selectReplica(context.Background(), validRequest())
	if !errors.Is(err, errUpstreamUnavailable) {
		t.Fatalf("error = %v, want unavailable", err)
	}
}
