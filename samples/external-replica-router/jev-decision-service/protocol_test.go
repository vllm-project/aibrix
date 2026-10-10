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
	"testing"
)

func validRequest() ReplicaSelectionRequest {
	return ReplicaSelectionRequest{
		APIVersion: apiVersion,
		Kind:       requestKind,
		Metadata:   RequestMetadata{RequestID: "req-1"},
		Spec: ReplicaSelectionSpec{
			Model:      "qwen2-7b",
			PolicyMode: "Advisory",
			Candidates: []Candidate{{
				ID:    "default/pod-a",
				Ports: []int{9000, 8000},
			}},
		},
	}
}

func TestValidateRequest(t *testing.T) {
	t.Run("accepts valid request", func(t *testing.T) {
		if err := validateRequest(validRequest()); err != nil {
			t.Fatalf("validateRequest() error = %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*ReplicaSelectionRequest)
	}{
		{"api version", func(r *ReplicaSelectionRequest) { r.APIVersion = "wrong" }},
		{"kind", func(r *ReplicaSelectionRequest) { r.Kind = "wrong" }},
		{"request id", func(r *ReplicaSelectionRequest) { r.Metadata.RequestID = "" }},
		{"model", func(r *ReplicaSelectionRequest) { r.Spec.Model = "" }},
		{"policy mode", func(r *ReplicaSelectionRequest) { r.Spec.PolicyMode = "Unknown" }},
		{"candidates", func(r *ReplicaSelectionRequest) { r.Spec.Candidates = nil }},
		{"candidate id", func(r *ReplicaSelectionRequest) { r.Spec.Candidates[0].ID = "" }},
		{"candidate ports", func(r *ReplicaSelectionRequest) { r.Spec.Candidates[0].Ports = nil }},
		{"zero port", func(r *ReplicaSelectionRequest) { r.Spec.Candidates[0].Ports = []int{0} }},
		{"large port", func(r *ReplicaSelectionRequest) { r.Spec.Candidates[0].Ports = []int{65536} }},
	}
	for _, tt := range tests {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			req := validRequest()
			tt.mutate(&req)
			if err := validateRequest(req); err == nil {
				t.Fatal("validateRequest() error = nil, want error")
			}
		})
	}
}

func TestSelectedResponseUsesSmallestCandidatePort(t *testing.T) {
	req := validRequest()
	got := selectedResponse(req, req.Spec.Candidates[0])
	if got.Metadata.RequestID != "req-1" {
		t.Fatalf("request ID = %q", got.Metadata.RequestID)
	}
	if got.Status.Decision != "Selected" {
		t.Fatalf("decision = %q", got.Status.Decision)
	}
	if got.Status.Target == nil || got.Status.Target.ID != "default/pod-a" || got.Status.Target.Port != 8000 {
		t.Fatalf("target = %#v", got.Status.Target)
	}
}
