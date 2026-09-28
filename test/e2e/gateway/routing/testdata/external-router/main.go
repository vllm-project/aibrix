/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

const mediaType = "application/vnd.aibrix.external-routing+json;version=v1alpha1"

type modeConfig struct {
	Mode        string `json:"mode"`
	TargetID    string `json:"targetId,omitempty"`
	DelayMillis int    `json:"delayMillis,omitempty"`
}

type state struct {
	mu   sync.Mutex
	mode modeConfig
	last json.RawMessage
}

func main() {
	s := &state{mode: modeConfig{Mode: "selected"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/select", s.selectReplica)
	mux.HandleFunc("/__test/mode", s.setMode)
	mux.HandleFunc("/__test/last-request", s.lastRequest)
	mux.HandleFunc("/__test/state", s.reset)
	if err := http.ListenAndServe(":8080", mux); err != nil {
		panic(err)
	}
}

func (s *state) selectReplica(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Metadata struct {
			RequestID string `json:"requestId"`
		} `json:"metadata"`
		Spec struct {
			Candidates []struct {
				ID    string `json:"id"`
				Ports []int  `json:"ports"`
			} `json:"candidates"`
		} `json:"spec"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 262144))
	if err != nil || json.Unmarshal(raw, &request) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.last = append(s.last[:0], raw...)
	mode := s.mode
	s.mu.Unlock()
	if mode.DelayMillis > 0 {
		time.Sleep(time.Duration(mode.DelayMillis) * time.Millisecond)
	}

	response := map[string]any{
		"apiVersion": "routing.aibrix.ai/v1alpha1",
		"kind":       "ReplicaSelectionResponse",
		"metadata":   map[string]any{"requestId": request.Metadata.RequestID},
	}
	status := map[string]any{}
	switch mode.Mode {
	case "denied":
		status["decision"] = "Denied"
		status["reason"] = "E2EPolicy"
	case "invalid-target":
		status["decision"] = "Selected"
		status["target"] = map[string]any{"id": "outside/not-a-candidate", "port": 8000}
	default:
		ids := make([]string, 0, len(request.Spec.Candidates))
		ports := map[string][]int{}
		for _, candidate := range request.Spec.Candidates {
			ids = append(ids, candidate.ID)
			ports[candidate.ID] = candidate.Ports
		}
		sort.Strings(ids)
		target := mode.TargetID
		if target == "" && len(ids) > 0 {
			target = ids[0]
		}
		port := 8000
		if len(ports[target]) > 0 {
			port = ports[target][0]
		}
		status["decision"] = "Selected"
		status["target"] = map[string]any{"id": target, "port": port}
	}
	response["status"] = status
	w.Header().Set("Content-Type", mediaType)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *state) setMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var mode modeConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&mode); err != nil {
		http.Error(w, "invalid mode", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *state) lastRequest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	raw := append([]byte(nil), s.last...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	_, _ = w.Write(raw)
}

func (s *state) reset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.mode = modeConfig{Mode: "selected"}
	s.last = nil
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
