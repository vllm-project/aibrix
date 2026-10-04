/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"errors"
	"fmt"
	"sort"
)

const (
	apiVersion   = "routing.aibrix.ai/v1alpha1"
	requestKind  = "ReplicaSelectionRequest"
	responseKind = "ReplicaSelectionResponse"
	mediaType    = "application/vnd.aibrix.external-routing+json;version=v1alpha1"
)

type RequestMetadata struct {
	RequestID string `json:"requestId"`
}

type PolicyContext struct {
	Attributes map[string]string `json:"attributes,omitempty"`
}

type CandidateMetrics struct {
	RunningRequests   *int64   `json:"runningRequests,omitempty"`
	EngineUtilization *float64 `json:"engineUtilization,omitempty"`
	KVCacheUsage      *float64 `json:"kvCacheUsage,omitempty"`
}

type Candidate struct {
	ID         string            `json:"id"`
	Ports      []int             `json:"ports"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Metrics    *CandidateMetrics `json:"metrics,omitempty"`
}

type ReplicaSelectionSpec struct {
	Model         string         `json:"model"`
	PolicyMode    string         `json:"policyMode"`
	PolicyContext *PolicyContext `json:"policyContext,omitempty"`
	Candidates    []Candidate    `json:"candidates"`
}

type ReplicaSelectionRequest struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Metadata   RequestMetadata      `json:"metadata"`
	Spec       ReplicaSelectionSpec `json:"spec"`
}

type ResponseMetadata struct {
	RequestID  string `json:"requestId"`
	DecisionID string `json:"decisionId,omitempty"`
}

type SelectionTarget struct {
	ID   string `json:"id"`
	Port int    `json:"port"`
}

type SelectionStatus struct {
	Decision string           `json:"decision"`
	Target   *SelectionTarget `json:"target,omitempty"`
	Reason   string           `json:"reason,omitempty"`
}

type ReplicaSelectionResponse struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   ResponseMetadata `json:"metadata"`
	Status     SelectionStatus  `json:"status"`
}

func validateRequest(req ReplicaSelectionRequest) error {
	if req.APIVersion != apiVersion || req.Kind != requestKind {
		return errors.New("unsupported request envelope")
	}
	if req.Metadata.RequestID == "" {
		return errors.New("requestId is required")
	}
	if req.Spec.Model == "" {
		return errors.New("model is required")
	}
	if req.Spec.PolicyMode != "Advisory" && req.Spec.PolicyMode != "Authoritative" {
		return errors.New("policyMode must be Advisory or Authoritative")
	}
	if len(req.Spec.Candidates) == 0 {
		return errors.New("candidates are required")
	}
	seen := make(map[string]struct{}, len(req.Spec.Candidates))
	for _, candidate := range req.Spec.Candidates {
		if candidate.ID == "" {
			return errors.New("candidate id is required")
		}
		if _, ok := seen[candidate.ID]; ok {
			return fmt.Errorf("duplicate candidate id %q", candidate.ID)
		}
		seen[candidate.ID] = struct{}{}
		if len(candidate.Ports) == 0 {
			return fmt.Errorf("candidate %q requires ports", candidate.ID)
		}
		for _, port := range candidate.Ports {
			if port < 1 || port > 65535 {
				return fmt.Errorf("candidate %q has invalid port %d", candidate.ID, port)
			}
		}
	}
	return nil
}

func selectedResponse(req ReplicaSelectionRequest, candidate Candidate) ReplicaSelectionResponse {
	ports := append([]int(nil), candidate.Ports...)
	sort.Ints(ports)
	return ReplicaSelectionResponse{
		APIVersion: apiVersion,
		Kind:       responseKind,
		Metadata:   ResponseMetadata{RequestID: req.Metadata.RequestID},
		Status: SelectionStatus{
			Decision: "Selected",
			Target:   &SelectionTarget{ID: candidate.ID, Port: ports[0]},
		},
	}
}
