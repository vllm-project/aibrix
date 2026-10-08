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
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

type resolvedTarget struct {
	NodeName string
	NodeUID  types.UID
	Sources  []string
}

func resolveTargetsFromNodes(
	w *modelv1alpha1.ModelWarmup,
	namespace *corev1.Namespace,
	nodes []corev1.Node,
) (map[string]resolvedTarget, map[string]string, error) {
	nodesByName := make(map[string]*corev1.Node, len(nodes))
	for i := range nodes {
		nodesByName[nodes[i].Name] = &nodes[i]
	}

	targets := make(map[string]resolvedTarget)
	missing := make(map[string]string)
	addTarget := func(node *corev1.Node, source string) {
		target := targets[node.Name]
		target.NodeName = node.Name
		target.NodeUID = node.UID
		for _, existing := range target.Sources {
			if existing == source {
				targets[node.Name] = target
				return
			}
		}
		target.Sources = append(target.Sources, source)
		targets[node.Name] = target
	}

	for i, target := range w.Spec.Targets {
		source := fmt.Sprintf("target[%d]", i)
		if target.Nodes != nil {
			for _, name := range target.Nodes.Names {
				node, ok := nodesByName[name]
				if !ok {
					missing[name] = "NodeNotFound"
					continue
				}
				if !isNodeAuthorized(namespace, node) {
					missing[name] = "NodeNotAuthorized"
					continue
				}
				addTarget(node, source)
			}
		}
		if target.NodeSelector != nil {
			selector, err := metav1.LabelSelectorAsSelector(target.NodeSelector)
			if err != nil {
				return nil, nil, err
			}
			for j := range nodes {
				node := &nodes[j]
				if !selector.Matches(labels.Set(node.Labels)) {
					continue
				}
				if !isNodeAuthorized(namespace, node) {
					missing[node.Name] = "NodeNotAuthorized"
					continue
				}
				addTarget(node, source)
			}
		}
	}
	for name, target := range targets {
		sort.Strings(target.Sources)
		targets[name] = target
	}
	return targets, missing, nil
}
