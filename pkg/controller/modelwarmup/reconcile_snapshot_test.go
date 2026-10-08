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

package modelwarmup

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestResolveTargetsFromNodes(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant",
		Labels: map[string]string{
			ResourcePoolLabelKey: "warm",
		},
	}}
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{
			Name: "node-a", UID: types.UID("uid-a"),
			Labels: map[string]string{
				"group": "gpu", ResourcePoolLabelKey: "warm", WarmupEnabledLabelKey: WarmupEnabledLabelValue,
			},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name: "node-b", UID: types.UID("uid-b"),
			Labels: map[string]string{
				"group": "gpu", ResourcePoolLabelKey: "warm", WarmupEnabledLabelKey: WarmupEnabledLabelValue,
			},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name: "node-disabled", UID: types.UID("uid-disabled"),
			Labels: map[string]string{
				"group": "disabled", ResourcePoolLabelKey: "warm",
			},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name: "node-control-plane", UID: types.UID("uid-control-plane"),
			Labels: map[string]string{
				"group": "control-plane", ResourcePoolLabelKey: "warm", WarmupEnabledLabelKey: WarmupEnabledLabelValue,
				"node-role.kubernetes.io/control-plane": "",
			},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name: "node-master", UID: types.UID("uid-master"),
			Labels: map[string]string{
				"group": "control-plane", ResourcePoolLabelKey: "warm", WarmupEnabledLabelKey: WarmupEnabledLabelValue,
				"node-role.kubernetes.io/master": "",
			},
		}},
	}

	t.Run("explicit only captures node identity", func(t *testing.T) {
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-b"}},
		}}}}

		targets, missing, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Empty(t, missing)
		require.Equal(t, map[string]resolvedTarget{
			"node-b": {NodeName: "node-b", NodeUID: types.UID("uid-b"), Sources: []string{"target[0]"}},
		}, targets)
	})

	t.Run("selector only", func(t *testing.T) {
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "gpu"}},
		}}}}

		targets, missing, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Empty(t, missing)
		require.Equal(t, map[string]resolvedTarget{
			"node-a": {NodeName: "node-a", NodeUID: types.UID("uid-a"), Sources: []string{"target[0]"}},
			"node-b": {NodeName: "node-b", NodeUID: types.UID("uid-b"), Sources: []string{"target[0]"}},
		}, targets)
	})

	t.Run("union deduplicates nodes and sorts sources", func(t *testing.T) {
		targetSpecs := make([]modelv1alpha1.ModelWarmupTarget, 11)
		targetSpecs[2].Nodes = &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a", "node-a"}}
		targetSpecs[10].NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"group": "gpu"}}
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: targetSpecs}}

		targets, missing, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Empty(t, missing)
		require.Equal(t, map[string]resolvedTarget{
			"node-a": {NodeName: "node-a", NodeUID: types.UID("uid-a"), Sources: []string{"target[10]", "target[2]"}},
			"node-b": {NodeName: "node-b", NodeUID: types.UID("uid-b"), Sources: []string{"target[10]"}},
		}, targets)
	})

	t.Run("invalid selector", func(t *testing.T) {
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			NodeSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "group", Operator: metav1.LabelSelectorOperator("Invalid"), Values: []string{"gpu"},
			}}},
		}}}}

		_, _, err := resolveTargetsFromNodes(warmup, namespace, nodes)
		require.Error(t, err)
	})

	t.Run("missing explicit node", func(t *testing.T) {
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"missing-node"}},
		}}}}

		targets, missing, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Empty(t, targets)
		require.Equal(t, map[string]string{"missing-node": "NodeNotFound"}, missing)
	})

	t.Run("unauthorized explicit node", func(t *testing.T) {
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-disabled"}},
		}}}}

		targets, missing, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Empty(t, targets)
		require.Equal(t, map[string]string{"node-disabled": "NodeNotAuthorized"}, missing)
	})

	t.Run("unauthorized selector match", func(t *testing.T) {
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "disabled"}},
		}}}}

		targets, missing, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Empty(t, targets)
		require.Equal(t, map[string]string{"node-disabled": "NodeNotAuthorized"}, missing)
	})

	t.Run("control plane nodes are unauthorized", func(t *testing.T) {
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "control-plane"}},
		}}}}

		targets, missing, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Empty(t, targets)
		require.Equal(t, map[string]string{
			"node-control-plane": "NodeNotAuthorized",
			"node-master":        "NodeNotAuthorized",
		}, missing)
	})

	t.Run("does not mutate inputs", func(t *testing.T) {
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{
			{Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a"}}},
			{NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "gpu"}}},
		}}}
		originalWarmup := warmup.DeepCopy()
		originalNamespace := namespace.DeepCopy()
		originalNodes := make([]corev1.Node, len(nodes))
		for i := range nodes {
			nodes[i].DeepCopyInto(&originalNodes[i])
		}

		_, _, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Equal(t, originalWarmup, warmup)
		require.Equal(t, originalNamespace, namespace)
		require.Equal(t, originalNodes, nodes)
	})
}
