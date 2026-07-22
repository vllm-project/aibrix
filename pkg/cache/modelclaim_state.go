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

package cache

import (
	"sort"
	"sync"

	v1 "k8s.io/api/core/v1"
)

type modelClaimBinding struct {
	podKey string
	port   int
	state  string
}

// modelClaimState tracks all ModelClaim advertisements, including port-0
// bindings. It never participates in normal model-to-pod routing.
type modelClaimState struct {
	mu       sync.RWMutex
	bindings map[string]map[string]modelClaimBinding
}

func (s *modelClaimState) set(podKey, model string, port int, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = make(map[string]map[string]modelClaimBinding)
	}
	byPod := s.bindings[model]
	if byPod == nil {
		byPod = make(map[string]modelClaimBinding)
		s.bindings[model] = byPod
	}
	byPod[podKey] = modelClaimBinding{podKey: podKey, port: port, state: state}
}

func (s *modelClaimState) clearPod(podKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for model, byPod := range s.bindings {
		delete(byPod, podKey)
		if len(byPod) == 0 {
			delete(s.bindings, model)
		}
	}
}

func (s *modelClaimState) get(model string) []modelClaimBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byPod := s.bindings[model]
	out := make([]modelClaimBinding, 0, len(byPod))
	for _, binding := range byPod {
		out = append(out, binding)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].podKey < out[j].podKey })
	return out
}

// ModelClaimBinding returns one deterministic advertisement for a served
// model. ModelClaim currently enforces one replica, while deterministic
// ordering keeps this safe if duplicate served names temporarily coexist.
func (c *Store) ModelClaimBinding(modelName string) (*v1.Pod, int, string, bool) {
	for _, binding := range c.modelClaims.get(modelName) {
		metaPod, found := c.metaPods.Load(binding.podKey)
		if !found || metaPod == nil || metaPod.Pod == nil {
			continue
		}
		return metaPod.Pod, binding.port, binding.state, true
	}
	return nil, 0, "", false
}

var _ ModelClaimBindingProvider = (*Store)(nil)
