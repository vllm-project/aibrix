/*
Copyright 2025 The Aibrix Team.

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

package roleset

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	podutil "github.com/vllm-project/aibrix/pkg/utils"
)

const TopologyPolicyPendingPodReplacementEventType = "TopologyPolicyPendingPodReplacement"

func (r *RoleSetReconciler) emitTopologyPolicyPendingReplacementEvent(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) error {
	if r.EventRecorder == nil || roleSet.Spec.TopologyPolicy == nil {
		return nil
	}

	var outdatedPods int
	var outdatedPodSets int
	for i := range roleSet.Spec.Roles {
		role := &roleSet.Spec.Roles[i]
		matchLabels, ok := getTopologyMatchLabels(roleSet, role.Name, roleSet.Spec.TopologyPolicy)
		if !ok || !validateTopologyKey(roleSet, roleSet.Spec.TopologyPolicy) {
			continue
		}

		pods, err := getRolePods(ctx, r.Client, roleSet.Namespace, roleSet.Name, role.Name)
		if err != nil {
			return err
		}
		for _, rolePod := range pods {
			if !podutil.IsPodActive(rolePod) {
				continue
			}
			if !podSpecHasTopologyAffinity(&rolePod.Spec, matchLabels, roleSet.Spec.TopologyPolicy) {
				outdatedPods++
			}
		}

		if role.PodGroupSize == nil || *role.PodGroupSize <= 1 {
			continue
		}
		podSets, err := getRolePodSets(ctx, r.Client, roleSet.Namespace, roleSet.Name, role.Name)
		if err != nil {
			return err
		}
		for _, rolePodSet := range podSets {
			if rolePodSet.DeletionTimestamp != nil {
				continue
			}
			if !podSpecHasTopologyAffinity(&rolePodSet.Spec.Template.Spec, matchLabels, roleSet.Spec.TopologyPolicy) {
				outdatedPodSets++
			}
		}
	}

	if outdatedPods == 0 && outdatedPodSets == 0 {
		return nil
	}

	r.EventRecorder.Eventf(
		roleSet,
		corev1.EventTypeWarning,
		TopologyPolicyPendingPodReplacementEventType,
		"TopologyPolicy changes do not update %s; they will use the new affinity after replacement or recreation.",
		formatTopologyPolicyEventTargets(outdatedPods, outdatedPodSets),
	)
	return nil
}

func podSpecHasTopologyAffinity(
	spec *corev1.PodSpec,
	matchLabels map[string]string,
	tp *orchestrationv1alpha1.TopologyPolicy,
) bool {
	if spec.Affinity == nil || spec.Affinity.PodAffinity == nil {
		return false
	}

	mode := tp.Mode
	if mode == "" {
		mode = orchestrationv1alpha1.TopologyPolicyPreferred
	}

	switch mode {
	case orchestrationv1alpha1.TopologyPolicyRequired:
		for _, term := range spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			if term.TopologyKey == tp.Key &&
				term.LabelSelector != nil &&
				apiequality.Semantic.DeepEqual(term.LabelSelector.MatchLabels, matchLabels) {
				return true
			}
		}
		return false
	default:
		for _, term := range spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			if term.Weight == topologyPreferredAffinityWeight &&
				term.PodAffinityTerm.TopologyKey == tp.Key &&
				term.PodAffinityTerm.LabelSelector != nil &&
				apiequality.Semantic.DeepEqual(term.PodAffinityTerm.LabelSelector.MatchLabels, matchLabels) {
				return true
			}
		}
		return false
	}
}

func formatTopologyPolicyEventCount(count int, name string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", name)
	}
	return fmt.Sprintf("%d %ss", count, name)
}

func formatTopologyPolicyEventTargets(outdatedPods, outdatedPodSets int) string {
	targets := make([]string, 0, 2)
	if outdatedPods > 0 {
		targets = append(targets, formatTopologyPolicyEventCount(outdatedPods, "active Pod"))
	}
	if outdatedPodSets > 0 {
		targets = append(targets, formatTopologyPolicyEventCount(outdatedPodSets, "PodSet template"))
	}
	return strings.Join(targets, " and ")
}
