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

package modelclaim

import (
	"sync"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Short-lived runtime refusals rotate the next pass past hidden listeners.
// Entries expire so a released port is retried, and UID/generation separate
// a recreated or changed claim from its predecessor. No durable API state.
type portConflictHistory struct {
	mu      sync.Mutex
	entries map[types.NamespacedName]recentPortConflicts
}
type recentPortConflicts struct {
	uid        types.UID
	generation int64
	pods       map[string]recentPodPortConflict
}

type recentPodPortConflict struct {
	until time.Time
	uid   types.UID
	ip    string
}

func (r *ModelClaimReconciler) portConflictHistory() *portConflictHistory {
	if r.PortConflicts == nil {
		r.PortConflicts = &portConflictHistory{entries: map[types.NamespacedName]recentPortConflicts{}}
	}
	return r.PortConflicts
}
func (h *portConflictHistory) note(pm *modelv1alpha1.ModelClaim, pod *corev1.Pod, now time.Time, candidates int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name}
	entry := h.entries[key]
	if entry.uid != pm.UID || entry.generation != pm.Generation || entry.pods == nil {
		entry = recentPortConflicts{uid: pm.UID, generation: pm.Generation, pods: map[string]recentPodPortConflict{}}
	}
	// Retain long enough to visit a large pool in two-attempt batches.
	hold := maximumPlacementBackoff + time.Duration(candidates/2+1)*DefaultRequeueDuration
	entry.pods[pod.Name] = recentPodPortConflict{until: now.Add(hold), uid: pod.UID, ip: pod.Status.PodIP}
	h.entries[key] = entry
}
func (h *portConflictHistory) filter(pm *modelv1alpha1.ModelClaim, pods []corev1.Pod, now time.Time) ([]corev1.Pod, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, entry := range h.entries {
		for pod, conflict := range entry.pods {
			if !now.Before(conflict.until) {
				delete(entry.pods, pod)
			}
		}
		if len(entry.pods) == 0 {
			delete(h.entries, key)
		}
	}
	key := types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name}
	entry := h.entries[key]
	if pm.Spec.ServingPort == nil || entry.uid != pm.UID || entry.generation != pm.Generation {
		delete(h.entries, key)
		return pods, nil
	}
	usable := make([]corev1.Pod, 0, len(pods))
	var conflicts []string
	for _, pod := range pods {
		if conflict, found := entry.pods[pod.Name]; found && conflict.uid == pod.UID && conflict.ip == pod.Status.PodIP && now.Before(conflict.until) {
			conflicts = append(conflicts, pod.Name)
		} else {
			usable = append(usable, pod)
		}
	}
	return usable, conflicts
}
