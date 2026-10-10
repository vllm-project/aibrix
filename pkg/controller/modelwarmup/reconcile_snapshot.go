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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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

type reconcileSnapshot struct {
	Targets         map[string]resolvedTarget
	Missing         map[string]string
	JobsByNode      map[string]batchv1.Job
	StaleActiveJobs []batchv1.Job
	ActiveJobs      int32
	MissingNodes    []string
}

func modelWarmupStatusEqual(left, right modelv1alpha1.ModelWarmupStatus) bool {
	return apiequality.Semantic.DeepEqual(left, right)
}

func buildReconcileSnapshot(
	w *modelv1alpha1.ModelWarmup,
	revision string,
	targets map[string]resolvedTarget,
	missing map[string]string,
	jobs []batchv1.Job,
) reconcileSnapshot {
	snapshot := reconcileSnapshot{
		Targets:    targets,
		Missing:    missing,
		JobsByNode: make(map[string]batchv1.Job),
	}
	for i := range jobs {
		job := &jobs[i]
		if !metav1.IsControlledBy(job, w) {
			continue
		}
		node := targetNodeForJob(job)
		_, targetExists := targets[node]
		currentRevision := job.Labels[RevisionLabelKey] == revision
		terminal := isJobComplete(job) || isJobFailed(job)

		if currentRevision && targetExists {
			snapshot.JobsByNode[node] = *job
		}
		if !terminal && (!currentRevision || !targetExists) {
			snapshot.StaleActiveJobs = append(snapshot.StaleActiveJobs, *job)
		}
		if job.DeletionTimestamp == nil && !terminal {
			snapshot.ActiveJobs++
		}
	}
	for node := range targets {
		if _, exists := snapshot.JobsByNode[node]; !exists {
			snapshot.MissingNodes = append(snapshot.MissingNodes, node)
		}
	}
	sort.Strings(snapshot.MissingNodes)
	return snapshot
}

func (s reconcileSnapshot) jobsForStatus(created []*batchv1.Job) []batchv1.Job {
	jobs := make([]batchv1.Job, 0, len(s.JobsByNode)+len(created))
	for _, job := range s.JobsByNode {
		jobs = append(jobs, job)
	}
	for _, job := range created {
		if job != nil {
			jobs = append(jobs, *job)
		}
	}
	return jobs
}

func resolveTargetsFromNodes(
	w *modelv1alpha1.ModelWarmup,
	namespace *corev1.Namespace,
	nodes []corev1.Node,
) (map[string]resolvedTarget, map[string]string, error) {
	if w == nil || namespace == nil {
		return nil, nil, fmt.Errorf("modelwarmup and namespace must not be nil")
	}

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
