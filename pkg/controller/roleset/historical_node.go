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

package roleset

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

const (
	maxHistoricalNodesPerRole = 8
	historicalNodeWeight      = int32(100)
)

type historicalNodeBindings struct {
	ReplicaSlots map[string]string   `json:"replicaSlots,omitempty"`
	Roles        map[string][]string `json:"roles,omitempty"`
}

func (b *historicalNodeBindings) normalize() {
	if b.ReplicaSlots == nil {
		b.ReplicaSlots = map[string]string{}
	}
	if b.Roles == nil {
		b.Roles = map[string][]string{}
	}
}

func parseHistoricalNodeBindings(roleSet *orchestrationv1alpha1.RoleSet) (historicalNodeBindings, error) {
	var bindings historicalNodeBindings
	value := roleSet.Annotations[constants.RoleSetHistoricalNodeBindingsAnnotationKey]
	if value == "" {
		bindings.normalize()
		return bindings, nil
	}
	if err := json.Unmarshal([]byte(value), &bindings); err != nil {
		return historicalNodeBindings{}, err
	}
	bindings.normalize()
	return bindings, nil
}

func refreshHistoricalNodeBindingsFromPods(roleSet *orchestrationv1alpha1.RoleSet, roles []orchestrationv1alpha1.RoleSpec, pods []*v1.Pod) (historicalNodeBindings, bool, error) {
	oldBindings, err := parseHistoricalNodeBindings(roleSet)
	invalidAnnotation := false
	if err != nil {
		klog.Warningf("roleset %s/%s historical-node bindings annotation is invalid, rebuilding from visible pods: %v", roleSet.Namespace, roleSet.Name, err)
		oldBindings = historicalNodeBindings{}
		oldBindings.normalize()
		invalidAnnotation = true
	}
	bindings := oldBindings.deepCopy()
	bindings.normalize()

	roleStateful := map[string]bool{}
	for i := range roles {
		roleStateful[roles[i].Name] = roles[i].Stateful
	}

	orderedPods := append([]*v1.Pod(nil), pods...)
	sort.SliceStable(orderedPods, func(i, j int) bool {
		left, right := orderedPods[i], orderedPods[j]
		if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
			return left.CreationTimestamp.Before(&right.CreationTimestamp)
		}
		if left.Namespace != right.Namespace {
			return left.Namespace < right.Namespace
		}
		return left.Name < right.Name
	})

	for _, pod := range orderedPods {
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			continue
		}
		roleName := pod.Labels[constants.RoleNameLabelKey]
		if roleName == "" {
			continue
		}
		if roleStateful[roleName] {
			index, ok := roleReplicaIndex(pod)
			if !ok {
				continue
			}
			bindings.ReplicaSlots[historicalReplicaSlotKey(roleName, index)] = nodeName
			continue
		}
		bindings.Roles[roleName] = prependUniqueNode(bindings.Roles[roleName], nodeName)
	}

	return bindings, invalidAnnotation || !reflect.DeepEqual(oldBindings, bindings), nil
}

func (b historicalNodeBindings) deepCopy() historicalNodeBindings {
	out := historicalNodeBindings{
		ReplicaSlots: make(map[string]string, len(b.ReplicaSlots)),
		Roles:        make(map[string][]string, len(b.Roles)),
	}
	for key, value := range b.ReplicaSlots {
		out.ReplicaSlots[key] = value
	}
	for key, values := range b.Roles {
		out.Roles[key] = append([]string(nil), values...)
	}
	return out
}

func roleReplicaIndex(pod *v1.Pod) (int, bool) {
	value := pod.Annotations[constants.RoleReplicaIndexAnnotationKey]
	if value == "" {
		value = pod.Labels[constants.RoleReplicaIndexLabelKey]
	}
	if value == "" {
		return 0, false
	}
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func historicalReplicaSlotKey(roleName string, index int) string {
	return fmt.Sprintf("%s/%d", roleName, index)
}

func prependUniqueNode(nodes []string, nodeName string) []string {
	out := []string{nodeName}
	for _, existing := range nodes {
		if existing == "" || existing == nodeName {
			continue
		}
		out = append(out, existing)
		if len(out) == maxHistoricalNodesPerRole {
			break
		}
	}
	return out
}

func syncHistoricalNodeBindings(ctx context.Context, cli client.Client, roleSet *orchestrationv1alpha1.RoleSet) error {
	if !roleSetHasHistoricalNodeScheduling(roleSet) {
		return nil
	}
	podList := &v1.PodList{}
	if err := cli.List(ctx, podList, client.InNamespace(roleSet.Namespace), client.MatchingLabels{
		constants.RoleSetNameLabelKey: roleSet.Name,
	}); err != nil {
		return err
	}
	pods := make([]*v1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pods = append(pods, &podList.Items[i])
	}
	bindings, changed, err := refreshHistoricalNodeBindingsFromPods(roleSet, roleSet.Spec.Roles, pods)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	data, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	original := roleSet.DeepCopy()
	patched := roleSet.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[constants.RoleSetHistoricalNodeBindingsAnnotationKey] = string(data)
	if err := cli.Patch(ctx, patched, client.MergeFrom(original)); err != nil {
		if apierrors.IsConflict(err) {
			klog.Warningf("roleset %s/%s failed to update historical-node bindings annotation due to conflict: %v", roleSet.Namespace, roleSet.Name, err)
			return nil
		}
		return err
	}
	roleSet.Annotations = patched.Annotations
	return nil
}

func roleSetHasHistoricalNodeScheduling(roleSet *orchestrationv1alpha1.RoleSet) bool {
	for i := range roleSet.Spec.Roles {
		if historicalNodeSchedulingEnabled(&roleSet.Spec.Roles[i]) {
			return true
		}
	}
	return false
}

func historicalNodeSchedulingEnabled(role *orchestrationv1alpha1.RoleSpec) bool {
	return role.UpdateStrategy.ReplacementScheduling != nil &&
		role.UpdateStrategy.ReplacementScheduling.HistoricalNode != nil
}

func historicalNodeBindingsForPodCreation(roleSet *orchestrationv1alpha1.RoleSet) *historicalNodeBindings {
	bindings, err := parseHistoricalNodeBindings(roleSet)
	if err != nil {
		klog.Warningf("roleset %s/%s skip historical-node affinity: invalid historical-node bindings annotation: %v", roleSet.Namespace, roleSet.Name, err)
		return nil
	}
	return &bindings
}

func maybeInjectHistoricalNodeAffinity(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, pod *v1.Pod, bindings *historicalNodeBindings, roleIndex *int, sameSlotActive bool) bool {
	if !historicalNodeSchedulingEnabled(role) || bindings == nil {
		return false
	}
	if role.PodGroupSize != nil && *role.PodGroupSize > 1 {
		return false
	}
	if hasRequiredNodeAffinity(&pod.Spec) {
		klog.Infof("roleset %s/%s role %s skip historical-node affinity for pod %s: pod template already has required node affinity", roleSet.Namespace, roleSet.Name, role.Name, pod.Name)
		return false
	}
	if hasRequiredHostnameTopologyPolicy(roleSet) {
		klog.Infof("roleset %s/%s role %s skip historical-node affinity for pod %s: required hostname topology policy may conflict", roleSet.Namespace, roleSet.Name, role.Name, pod.Name)
		return false
	}
	var nodes []string
	if role.Stateful {
		if roleIndex == nil {
			return false
		}
		if sameSlotActive {
			klog.Infof("roleset %s/%s role %s slot %d skip historical-node affinity for pod %s: old same-slot pod is still active", roleSet.Namespace, roleSet.Name, role.Name, *roleIndex, pod.Name)
			return false
		}
		nodeName := bindings.ReplicaSlots[historicalReplicaSlotKey(role.Name, *roleIndex)]
		if nodeName == "" {
			return false
		}
		nodes = []string{nodeName}
	} else {
		nodes = bindings.Roles[role.Name]
	}
	if len(nodes) == 0 {
		return false
	}
	return injectPreferredHistoricalNodeAffinity(&pod.Spec, nodes)
}

func hasRequiredNodeAffinity(spec *v1.PodSpec) bool {
	return spec.Affinity != nil &&
		spec.Affinity.NodeAffinity != nil &&
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil
}

func hasRequiredHostnameTopologyPolicy(roleSet *orchestrationv1alpha1.RoleSet) bool {
	return roleSet.Spec.TopologyPolicy != nil &&
		roleSet.Spec.TopologyPolicy.Mode == orchestrationv1alpha1.TopologyPolicyRequired &&
		roleSet.Spec.TopologyPolicy.Key == v1.LabelHostname
}

func injectPreferredHistoricalNodeAffinity(spec *v1.PodSpec, nodeNames []string) bool {
	if len(nodeNames) == 0 {
		return false
	}
	if spec.Affinity == nil {
		spec.Affinity = &v1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &v1.NodeAffinity{}
	}

	term := v1.PreferredSchedulingTerm{
		Weight: historicalNodeWeight,
		Preference: v1.NodeSelectorTerm{
			MatchExpressions: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelHostname,
					Operator: v1.NodeSelectorOpIn,
					Values:   nodeNames,
				},
			},
		},
	}
	preferred := spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	for _, existing := range preferred {
		if reflect.DeepEqual(existing, term) {
			return false
		}
	}
	spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(preferred, term)
	return true
}
