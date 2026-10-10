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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestBuildReconcileSnapshotIndexesOwnedJobs(t *testing.T) {
	warmup := snapshotWarmup()
	targets := testResolvedTargets(map[string][]string{
		"node-a": {"target[0]"},
		"node-b": {"target[0]"},
	})
	running := snapshotJob(warmup, "running", "node-a", "rev")
	completed := snapshotJob(warmup, "completed", "node-b", "rev")
	completed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}

	snapshot := buildReconcileSnapshot(warmup, "rev", targets, nil, []batchv1.Job{*running, *completed})

	require.Equal(t, targets, snapshot.Targets)
	require.Empty(t, snapshot.Missing)
	require.Equal(t, map[string]batchv1.Job{
		"node-a": *running,
		"node-b": *completed,
	}, snapshot.JobsByNode)
	require.Empty(t, snapshot.StaleActiveJobs)
	require.Equal(t, int32(1), snapshot.ActiveJobs)
	require.Empty(t, snapshot.MissingNodes)
}

func TestBuildReconcileSnapshotIgnoresForeignLabeledJobs(t *testing.T) {
	warmup := snapshotWarmup()
	targets := testResolvedTargets(map[string][]string{"node-a": {"target[0]"}})
	foreign := snapshotJob(warmup, "foreign", "node-a", "rev")
	foreign.OwnerReferences = []metav1.OwnerReference{{UID: "different-warmup", Controller: ptr.To(true)}}

	snapshot := buildReconcileSnapshot(warmup, "rev", targets, nil, []batchv1.Job{*foreign})

	require.Empty(t, snapshot.JobsByNode)
	require.Empty(t, snapshot.StaleActiveJobs)
	require.Zero(t, snapshot.ActiveJobs)
	require.Equal(t, []string{"node-a"}, snapshot.MissingNodes)
}

func TestBuildReconcileSnapshotClassifiesStaleActiveJobs(t *testing.T) {
	warmup := snapshotWarmup()
	targets := testResolvedTargets(map[string][]string{"node-a": {"target[0]"}})
	oldRevision := snapshotJob(warmup, "old-revision", "node-a", "old")
	removedTarget := snapshotJob(warmup, "removed-target", "node-b", "rev")

	snapshot := buildReconcileSnapshot(warmup, "rev", targets, nil, []batchv1.Job{*oldRevision, *removedTarget})

	require.Empty(t, snapshot.JobsByNode)
	require.Equal(t, []batchv1.Job{*oldRevision, *removedTarget}, snapshot.StaleActiveJobs)
	require.Equal(t, int32(2), snapshot.ActiveJobs)
	require.Equal(t, []string{"node-a"}, snapshot.MissingNodes)
}

func TestBuildReconcileSnapshotPreservesTerminalStaleJobs(t *testing.T) {
	warmup := snapshotWarmup()
	targets := testResolvedTargets(map[string][]string{"node-a": {"target[0]"}})
	completed := snapshotJob(warmup, "old-completed", "node-a", "old")
	completed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	failed := snapshotJob(warmup, "removed-failed", "node-b", "rev")
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}

	snapshot := buildReconcileSnapshot(warmup, "rev", targets, nil, []batchv1.Job{*completed, *failed})

	require.Empty(t, snapshot.JobsByNode)
	require.Empty(t, snapshot.StaleActiveJobs)
	require.Zero(t, snapshot.ActiveJobs)
	require.Equal(t, []string{"node-a"}, snapshot.MissingNodes)
}

func TestBuildReconcileSnapshotCalculatesMissingNodesAndCapacity(t *testing.T) {
	warmup := snapshotWarmup()
	targets := testResolvedTargets(map[string][]string{
		"node-a": {"target[0]"},
		"node-b": {"target[0]"},
		"node-c": {"target[0]"},
		"node-e": {"target[0]"},
	})
	running := snapshotJob(warmup, "running", "node-a", "rev")
	completed := snapshotJob(warmup, "completed", "node-b", "rev")
	completed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	stale := snapshotJob(warmup, "stale", "node-d", "old")

	snapshot := buildReconcileSnapshot(warmup, "rev", targets, map[string]string{"missing-node": "NodeNotFound"}, []batchv1.Job{*running, *completed, *stale})

	require.Equal(t, map[string]string{"missing-node": "NodeNotFound"}, snapshot.Missing)
	require.Equal(t, []string{"node-c", "node-e"}, snapshot.MissingNodes)
	require.Equal(t, []batchv1.Job{*stale}, snapshot.StaleActiveJobs)
	require.Equal(t, int32(2), snapshot.ActiveJobs)
}

func TestReconcileSnapshotJobsForStatusIncludesCreatedJobs(t *testing.T) {
	warmup := snapshotWarmup()
	existing := snapshotJob(warmup, "existing", "node-a", "rev")
	created := snapshotJob(warmup, "created", "node-b", "rev")
	snapshot := reconcileSnapshot{JobsByNode: map[string]batchv1.Job{"node-a": *existing}}

	jobs := snapshot.jobsForStatus([]*batchv1.Job{created})

	require.ElementsMatch(t, []batchv1.Job{*existing, *created}, jobs)
}

func TestModelWarmupStatusEqualUsesKubernetesSemanticEquality(t *testing.T) {
	instant := time.Date(2026, time.October, 8, 9, 0, 0, 0, time.UTC)
	leftTime := metav1.NewTime(instant)
	rightTime := metav1.NewTime(instant.In(time.FixedZone("UTC+8", 8*60*60)))
	left := modelv1alpha1.ModelWarmupStatus{StartTime: &leftTime}
	right := modelv1alpha1.ModelWarmupStatus{StartTime: &rightTime}

	require.True(t, modelWarmupStatusEqual(left, right))
	right.Phase = modelv1alpha1.ModelWarmupRunning
	require.False(t, modelWarmupStatusEqual(left, right))
}

func snapshotWarmup() *modelv1alpha1.ModelWarmup {
	return &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
}

func snapshotJob(warmup *modelv1alpha1.ModelWarmup, name, node, revision string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			WarmupLabelKey: string(warmup.UID), RevisionLabelKey: revision,
		},
		Annotations:     map[string]string{TargetNodeAnnotationKey: node},
		OwnerReferences: []metav1.OwnerReference{{UID: warmup.UID, Controller: ptr.To(true)}},
	}}
}

func BenchmarkBuildReconcileSnapshot(b *testing.B) {
	for _, benchmark := range []struct {
		name     string
		nodes    int
		jobs     int
		jobState func(*batchv1.Job, int)
	}{
		{name: "100-nodes-100-jobs", nodes: 100, jobs: 100},
		{name: "1000-nodes-0-jobs", nodes: 1000},
		{name: "1000-nodes-1000-completed-jobs", nodes: 1000, jobs: 1000, jobState: func(job *batchv1.Job, _ int) {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}},
		{name: "1000-nodes-mixed-jobs", nodes: 1000, jobs: 1000, jobState: func(job *batchv1.Job, index int) {
			switch index % 4 {
			case 1:
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			case 2:
				job.Labels[RevisionLabelKey] = "old"
			case 3:
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			}
		}},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			warmup := snapshotWarmup()
			targets := make(map[string]resolvedTarget, benchmark.nodes)
			jobs := make([]batchv1.Job, 0, benchmark.jobs)
			for i := 0; i < benchmark.nodes; i++ {
				node := fmt.Sprintf("node-%04d", i)
				targets[node] = resolvedTarget{NodeName: node, Sources: []string{"target[0]"}}
			}
			for i := 0; i < benchmark.jobs; i++ {
				node := fmt.Sprintf("node-%04d", i)
				job := snapshotJob(warmup, fmt.Sprintf("job-%04d", i), node, "rev")
				if benchmark.jobState != nil {
					benchmark.jobState(job, i)
				}
				jobs = append(jobs, *job)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = buildReconcileSnapshot(warmup, "rev", targets, nil, jobs)
			}
		})
	}
}

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

	t.Run("rejects nil inputs", func(t *testing.T) {
		for name, warmupAndNamespace := range map[string]struct {
			warmup    *modelv1alpha1.ModelWarmup
			namespace *corev1.Namespace
		}{
			"warmup":    {warmup: nil, namespace: namespace},
			"namespace": {warmup: &modelv1alpha1.ModelWarmup{}, namespace: nil},
		} {
			t.Run(name, func(t *testing.T) {
				targets, missing, err := resolveTargetsFromNodes(
					warmupAndNamespace.warmup,
					warmupAndNamespace.namespace,
					nodes,
				)

				require.Nil(t, targets)
				require.Nil(t, missing)
				require.ErrorContains(t, err, "modelwarmup and namespace must not be nil")
			})
		}
	})

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

	t.Run("union deduplicates nodes, preserves source multiplicity, and sorts sources", func(t *testing.T) {
		targetSpecs := make([]modelv1alpha1.ModelWarmupTarget, 11)
		targetSpecs[2].Nodes = &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a", "node-a"}}
		targetSpecs[10].NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"group": "gpu"}}
		warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: targetSpecs}}

		targets, missing, err := resolveTargetsFromNodes(warmup, namespace, nodes)

		require.NoError(t, err)
		require.Empty(t, missing)
		require.Equal(t, map[string]resolvedTarget{
			"node-a": {NodeName: "node-a", NodeUID: types.UID("uid-a"), Sources: []string{"target[10]", "target[2]", "target[2]"}},
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
