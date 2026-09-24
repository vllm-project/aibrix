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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

type modelClaimBinding struct {
	podKey string
	port   int
	state  string
}

// modelClaimRecord is what the gateway knows of one ModelClaim object: its
// phase, and why it does not serve yet.
type modelClaimRecord struct {
	phase  string
	reason string
}

// modelClaimState tracks all ModelClaim advertisements, including port-0
// bindings. It never participates in normal model-to-pod routing.
//
// It also tracks the ModelClaim objects themselves. A claim that is not placed
// yet has no pod to advertise it, and is known only there.
type modelClaimState struct {
	mu       sync.RWMutex
	bindings map[string]map[string]modelClaimBinding
	// claims holds each claim by the name it serves, keyed by namespace/name,
	// and served maps each claim back to that name.
	claims map[string]map[string]modelClaimRecord
	served map[string]string
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

// setClaim records a claim under the name it serves, and forgets the name it
// served before.
func (s *modelClaimState) setClaim(key, model string, record modelClaimRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearClaimLocked(key)
	if s.claims == nil {
		s.claims = make(map[string]map[string]modelClaimRecord)
		s.served = make(map[string]string)
	}
	byKey := s.claims[model]
	if byKey == nil {
		byKey = make(map[string]modelClaimRecord)
		s.claims[model] = byKey
	}
	byKey[key] = record
	s.served[key] = model
}

func (s *modelClaimState) clearClaim(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearClaimLocked(key)
}

func (s *modelClaimState) clearClaimLocked(key string) {
	model, found := s.served[key]
	if !found {
		return
	}
	delete(s.served, key)
	delete(s.claims[model], key)
	if len(s.claims[model]) == 0 {
		delete(s.claims, model)
	}
}

// claim returns the record of the first claim, by namespace and name, that
// serves a model.
func (s *modelClaimState) claim(model string) (modelClaimRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byKey := s.claims[model]
	if len(byKey) == 0 {
		return modelClaimRecord{}, false
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return byKey[keys[0]], true
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

// setModelClaim records a ModelClaim object as the gateway sees it. A claim
// being deleted is forgotten at once, since the model it serves is going away.
func (c *Store) setModelClaim(claim *modelv1alpha1.ModelClaim) {
	key := claim.Namespace + "/" + claim.Name
	if !claim.DeletionTimestamp.IsZero() {
		c.modelClaims.clearClaim(key)
		return
	}
	c.modelClaims.setClaim(key, modelClaimServedName(claim), modelClaimRecord{
		phase:  string(claim.Status.Phase),
		reason: modelClaimReason(claim),
	})
}

func (c *Store) deleteModelClaim(claim *modelv1alpha1.ModelClaim) {
	c.modelClaims.clearClaim(claim.Namespace + "/" + claim.Name)
}

// ModelClaimStatus reports the phase of the ModelClaim that serves a model,
// and why it does not serve yet, whether or not a pod advertises it. With
// several claims for one name, the first by namespace and name answers, as for
// bindings.
func (c *Store) ModelClaimStatus(modelName string) (string, string, bool) {
	record, found := c.modelClaims.claim(modelName)
	return record.phase, record.reason, found
}

// modelClaimServedName is the name clients address a claim's model by, as the
// controller advertises it: its modelName, or else the claim's own name.
func modelClaimServedName(claim *modelv1alpha1.ModelClaim) string {
	if claim.Spec.ModelName != nil && *claim.Spec.ModelName != "" {
		return *claim.Spec.ModelName
	}
	return claim.Name
}

// modelClaimReason is why a claim does not serve yet, in the controller's own
// word: the reason on its Scheduled condition while that is False, or else the
// one on its Ready condition while that is False.
func modelClaimReason(claim *modelv1alpha1.ModelClaim) string {
	for _, conditionType := range []modelv1alpha1.ModelClaimConditionType{
		modelv1alpha1.ModelClaimConditionTypeScheduled,
		modelv1alpha1.ModelClaimConditionReady,
	} {
		condition := meta.FindStatusCondition(claim.Status.Conditions, string(conditionType))
		if condition != nil && condition.Status == metav1.ConditionFalse {
			return condition.Reason
		}
	}
	return ""
}

var _ ModelClaimBindingProvider = (*Store)(nil)
var _ ModelClaimStatusProvider = (*Store)(nil)
