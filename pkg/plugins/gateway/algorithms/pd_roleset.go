/*
Copyright 2024 The Aibrix Team.

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

package routingalgorithms

import (
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
)

type pdRolesetBucket struct {
	prefills []*v1.Pod
	decodes  []*v1.Pod
}

func groupPDPodsByRoleset(readyPods []*v1.Pod) map[string]*pdRolesetBucket {
	byRoleset := make(map[string]*pdRolesetBucket)
	for _, pod := range readyPods {
		roleSetID, hasRoleset := pod.Labels[PDRoleSetIdentifier]
		if !hasRoleset {
			continue
		}
		roleID, hasRole := pod.Labels[PDRoleIdentifier]
		if !hasRole {
			continue
		}
		if !isPodWithHTTPServer(pod) {
			continue
		}
		switch roleID {
		case PDRolePrefill, PDRoleDecode:
			b := byRoleset[roleSetID]
			if b == nil {
				b = &pdRolesetBucket{}
				byRoleset[roleSetID] = b
			}
			if roleID == PDRolePrefill {
				b.prefills = append(b.prefills, pod)
			} else {
				b.decodes = append(b.decodes, pod)
			}
		}
	}
	return byRoleset
}

// roleReplicaCardinality returns how many distinct replicas of a role are present.
// When role-replica-index labels exist, distinct index values are counted; otherwise
// each routable pod is treated as its own replica.
func roleReplicaCardinality(pods []*v1.Pod) int {
	if len(pods) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(pods))
	hasIndexLabel := false
	for _, pod := range pods {
		idx, ok := pod.Labels[RoleReplicaIndex]
		if ok && idx != "" {
			hasIndexLabel = true
			seen[idx] = struct{}{}
		}
	}
	if hasIndexLabel {
		return len(seen)
	}
	return len(pods)
}

func fleetExpectedRoleCounts(byRoleset map[string]*pdRolesetBucket) (maxPrefill, maxDecode int) {
	for _, b := range byRoleset {
		if n := roleReplicaCardinality(b.prefills); n > maxPrefill {
			maxPrefill = n
		}
		if n := roleReplicaCardinality(b.decodes); n > maxDecode {
			maxDecode = n
		}
	}
	return maxPrefill, maxDecode
}

func isPDRolesetEligible(b *pdRolesetBucket) bool {
	return roleReplicaCardinality(b.prefills) > 0 && roleReplicaCardinality(b.decodes) > 0
}

// eligiblePDRolesets returns rolesets with at least one routable prefill and decode pod.
func eligiblePDRolesets(readyPods []*v1.Pod) map[string]*pdRolesetBucket {
	byRoleset := groupPDPodsByRoleset(readyPods)
	eligible := make(map[string]*pdRolesetBucket, len(byRoleset))
	for id, b := range byRoleset {
		if isPDRolesetEligible(b) {
			eligible[id] = b
		}
	}
	return eligible
}

// HasEligibleRoleset reports whether at least one PD roleset has both a prefill and
// decode pod ready for disaggregated routing.
func HasEligibleRoleset(readyPods []*v1.Pod) bool {
	return len(eligiblePDRolesets(readyPods)) > 0
}

func prefillPodsFromEligibleRolesets(readyPods []*v1.Pod) []*v1.Pod {
	eligible := eligiblePDRolesets(readyPods)
	out := make([]*v1.Pod, 0, len(eligible))
	for _, b := range eligible {
		out = append(out, b.prefills...)
	}
	return out
}

// PDRolesetEligibility describes one roleset's readiness for PD routing.
type PDRolesetEligibility struct {
	Roleset         string
	PrefillReplicas int
	DecodeReplicas  int
	PrefillPods     string
	DecodePods      string
	Eligible        bool
}

func pdPodNames(pods []*v1.Pod) string {
	if len(pods) == 0 {
		return ""
	}
	names := make([]string, len(pods))
	for i, p := range pods {
		names[i] = p.Name
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// DescribePDRolesetEligibility returns fleet-wide max replica counts (for diagnostics)
// and per-roleset eligibility (≥1 prefill and ≥1 decode).
func DescribePDRolesetEligibility(readyPods []*v1.Pod) (fleetMaxPrefill, fleetMaxDecode int, statuses []PDRolesetEligibility, eligibleCount int) {
	byRoleset := groupPDPodsByRoleset(readyPods)
	fleetMaxPrefill, fleetMaxDecode = fleetExpectedRoleCounts(byRoleset)
	statuses = make([]PDRolesetEligibility, 0, len(byRoleset))
	for id, b := range byRoleset {
		eligible := isPDRolesetEligible(b)
		if eligible {
			eligibleCount++
		}
		statuses = append(statuses, PDRolesetEligibility{
			Roleset:         id,
			PrefillReplicas: roleReplicaCardinality(b.prefills),
			DecodeReplicas:  roleReplicaCardinality(b.decodes),
			PrefillPods:     pdPodNames(b.prefills),
			DecodePods:      pdPodNames(b.decodes),
			Eligible:        eligible,
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Roleset < statuses[j].Roleset })
	return fleetMaxPrefill, fleetMaxDecode, statuses, eligibleCount
}
