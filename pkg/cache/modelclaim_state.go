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

// modelClaimRecord is what the gateway knows of one ModelClaim object: the
// name it serves, its phase, and why it does not serve yet.
type modelClaimRecord struct {
	model  string
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
	// claims holds each ModelClaim object by namespace/name.
	claims map[string]modelClaimRecord
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

// setClaim records a claim, in place of what was known of it before. A claim
// that now serves another name no longer answers for the old one.
func (s *modelClaimState) setClaim(key string, record modelClaimRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims == nil {
		s.claims = make(map[string]modelClaimRecord)
	}
	s.claims[key] = record
}

func (s *modelClaimState) clearClaim(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, key)
}

// claim returns the record of the first claim, by namespace and name, that
// serves a model. It walks every claim. It is asked only about a model that no
// pod advertises.
func (s *modelClaimState) claim(model string) (modelClaimRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var first string
	var record modelClaimRecord
	found := false
	for key, candidate := range s.claims {
		if candidate.model == model && (!found || key < first) {
			first, record, found = key, candidate, true
		}
	}
	return record, found
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
	c.modelClaims.setClaim(key, modelClaimRecord{
		model:  modelClaimServedName(claim),
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
// controller advertises it: its modelName, or else the claim's own name. It has
// to agree with servedModelName in the ModelClaim controller.
func modelClaimServedName(claim *modelv1alpha1.ModelClaim) string {
	if claim.Spec.ModelName != nil && *claim.Spec.ModelName != "" {
		return *claim.Spec.ModelName
	}
	return claim.Name
}

// modelClaimReason is why a claim does not serve yet, in the controller's own
// word. A claim that waits for a card says why on its Scheduled condition, and
// one that failed says why on its Ready condition. Only the condition its phase
// is about is read, since the other can be stale: the controller leaves
// Scheduled False from an earlier refusal after the claim moves on.
func modelClaimReason(claim *modelv1alpha1.ModelClaim) string {
	conditionType := modelv1alpha1.ModelClaimConditionReady
	switch claim.Status.Phase {
	case "", modelv1alpha1.ModelClaimPending, modelv1alpha1.ModelClaimScheduling:
		conditionType = modelv1alpha1.ModelClaimConditionTypeScheduled
	}
	condition := meta.FindStatusCondition(claim.Status.Conditions, string(conditionType))
	if condition == nil || condition.Status != metav1.ConditionFalse {
		return ""
	}
	return condition.Reason
}

var _ ModelClaimBindingProvider = (*Store)(nil)
var _ ModelClaimStatusProvider = (*Store)(nil)
