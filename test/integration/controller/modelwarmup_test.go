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

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	modelwarmup "github.com/vllm-project/aibrix/pkg/controller/modelwarmup"
	controllerutils "github.com/vllm-project/aibrix/test/utils/controller"
)

var _ = Describe("ModelWarmup controller", func() {
	const timeout = 20 * time.Second
	const interval = 100 * time.Millisecond

	It("creates a safe node-pinned Job and reports Running", func() {
		ns := newModelWarmupNamespace("template")
		node := newModelWarmupNode("template", map[string]string{"pool": "warm"})
		warmup := controllerutils.NewModelWarmup(ns.Name, "template", node.Name)
		warmup.Spec.Targets = []modelapi.ModelWarmupTarget{
			{Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{node.Name}}},
			{NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "warm"}}},
		}
		warmup.Spec.ImagePreload.PullSecrets = []corev1.LocalObjectReference{{Name: "pull-secret"}}
		warmup.Spec.ImagePreload.Images[0].Command = []string{"sh"}
		warmup.Spec.ImagePreload.Images[0].Args = []string{"-c", "exit 0"}
		warmup.Spec.Policies = &modelapi.ModelWarmupPolicies{
			Parallelism: ptr.To[int32](1), JobTimeoutSeconds: ptr.To[int64](31),
			RetryLimit: ptr.To[int32](3), TTLSecondsAfterFinished: ptr.To[int32](41),
		}
		Expect(k8sClient.Create(ctx, warmup)).To(Succeed())

		var job batchv1.Job
		Eventually(func(g Gomega) {
			jobs := controllerutils.ListModelWarmupJobs(g, ctx, k8sClient, ns.Name, warmup.Name)
			g.Expect(jobs).To(HaveLen(1))
			job = jobs[0]
			g.Expect(job.OwnerReferences).To(HaveLen(1))
			g.Expect(job.OwnerReferences[0].UID).To(Equal(warmup.UID))
		}, timeout, interval).Should(Succeed())
		Expect(job.Spec.Template.Spec.NodeName).To(BeEmpty())
		Expect(controllerutils.ModelWarmupJobNode(&job)).To(Equal(node.Name))
		Expect(job.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.
			NodeSelectorTerms[0].MatchFields[0].Values).To(Equal([]string{node.Name}))
		Expect(job.Spec.BackoffLimit).To(Equal(ptr.To[int32](3)))
		Expect(job.Spec.ActiveDeadlineSeconds).To(Equal(ptr.To[int64](31)))
		Expect(job.Spec.TTLSecondsAfterFinished).To(Equal(ptr.To[int32](41)))
		Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(job.Spec.Template.Spec.AutomountServiceAccountToken).To(Equal(ptr.To(false)))
		Expect(job.Spec.Template.Spec.ImagePullSecrets).To(Equal(warmup.Spec.ImagePreload.PullSecrets))
		Expect(job.Spec.Template.Spec.Tolerations).To(BeEmpty())
		Expect(job.Spec.Template.Spec.Volumes).To(BeEmpty())
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(Equal(ptr.To(false)))
		Expect(job.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))
		Expect(job.Spec.Template.Spec.Containers[0].Resources.Requests).To(BeEmpty())

		Eventually(func(g Gomega) {
			latest := getModelWarmup(g, warmup)
			g.Expect(latest.Status.Phase).To(Equal(modelapi.ModelWarmupRunning))
			g.Expect(latest.Status.DesiredNodes).To(Equal(int32(1)))
			g.Expect(latest.Status.ActiveNodes).To(Equal(int32(1)))
			g.Expect(latest.Status.Targets[0].Source).To(Equal("target[0]"))
			g.Expect(latest.Status.Targets[0].SourceCount).To(Equal(int32(2)))
			g.Expect(condition(latest, "Progressing").Status).To(Equal(metav1.ConditionTrue))
			g.Expect(condition(latest, "Complete").Status).To(Equal(metav1.ConditionFalse))
			g.Expect(condition(latest, "Degraded").Status).To(Equal(metav1.ConditionFalse))
		}, timeout, interval).Should(Succeed())
	})

	It("does not reopen a terminal Once warmup for a newly matching node", func() {
		ns := newModelWarmupNamespace("selector")
		firstNode := newModelWarmupNode("selector-a", map[string]string{"pool": "selector"})
		warmup := controllerutils.NewModelWarmup(ns.Name, "selector", firstNode.Name)
		warmup.Spec.Targets = []modelapi.ModelWarmupTarget{{
			NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "selector"}},
		}}
		Expect(k8sClient.Create(ctx, warmup)).To(Succeed())
		var oldJob batchv1.Job
		Eventually(func(g Gomega) {
			jobs := controllerutils.ListModelWarmupJobs(g, ctx, k8sClient, ns.Name, warmup.Name)
			g.Expect(jobs).To(HaveLen(1))
			oldJob = jobs[0]
		}, timeout, interval).Should(Succeed())
		setJobSucceeded(oldJob)
		Eventually(func(g Gomega) {
			latest := getModelWarmup(g, warmup)
			g.Expect(latest.Status.Phase).To(Equal(modelapi.ModelWarmupSucceeded))
			g.Expect(condition(latest, "Complete").Status).To(Equal(metav1.ConditionTrue))
			g.Expect(condition(latest, "Progressing").Status).To(Equal(metav1.ConditionFalse))
			g.Expect(condition(latest, "Degraded").Status).To(Equal(metav1.ConditionFalse))
		}, timeout, interval).Should(Succeed())

		_ = newModelWarmupNode("selector-b", map[string]string{"pool": "selector"})
		Eventually(func(g Gomega) {
			jobs := controllerutils.ListModelWarmupJobs(g, ctx, k8sClient, ns.Name, warmup.Name)
			g.Expect(jobs).To(HaveLen(1))
			for _, job := range jobs {
				if controllerutils.ModelWarmupJobNode(&job) == firstNode.Name {
					g.Expect(job.Name).To(Equal(oldJob.Name))
				}
			}
		}, timeout, interval).Should(Succeed())
	})

	It("adds a newly matching node while Once is still running", func() {
		ns := newModelWarmupNamespace("active-selector")
		firstNode := newModelWarmupNode("active-selector-a", map[string]string{"pool": "active-selector"})
		warmup := controllerutils.NewModelWarmup(ns.Name, "active-selector", firstNode.Name)
		warmup.Spec.Targets = []modelapi.ModelWarmupTarget{{
			NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "active-selector"}},
		}}
		warmup.Spec.Policies.Parallelism = ptr.To[int32](2)
		Expect(k8sClient.Create(ctx, warmup)).To(Succeed())
		Eventually(func(g Gomega) {
			jobs := controllerutils.ListModelWarmupJobs(g, ctx, k8sClient, ns.Name, warmup.Name)
			g.Expect(jobs).To(HaveLen(1))
		}, timeout, interval).Should(Succeed())

		secondNode := newModelWarmupNode("active-selector-b", map[string]string{"pool": "active-selector"})
		Eventually(func(g Gomega) {
			jobs := controllerutils.ListModelWarmupJobs(g, ctx, k8sClient, ns.Name, warmup.Name)
			g.Expect(jobs).To(HaveLen(2))
			nodes := []string{controllerutils.ModelWarmupJobNode(&jobs[0]), controllerutils.ModelWarmupJobNode(&jobs[1])}
			g.Expect(nodes).To(ConsistOf(firstNode.Name, secondNode.Name))
		}, timeout, interval).Should(Succeed())
	})

	It("rejects a node outside the namespace resource pool", func() {
		ns := newModelWarmupNamespace("unauthorized")
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "modelwarmup-unauthorized", Labels: map[string]string{
			modelwarmup.ResourcePoolLabelKey:  "other",
			modelwarmup.WarmupEnabledLabelKey: modelwarmup.WarmupEnabledLabelValue,
		}}}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		warmup := controllerutils.NewModelWarmup(ns.Name, "unauthorized", node.Name)
		Expect(k8sClient.Create(ctx, warmup)).To(Succeed())

		Eventually(func(g Gomega) {
			latest := getModelWarmup(g, warmup)
			g.Expect(latest.Status.Phase).To(Equal(modelapi.ModelWarmupFailed))
			g.Expect(latest.Status.Targets).To(HaveLen(1))
			g.Expect(latest.Status.Targets[0].Reason).To(Equal("NodeNotAuthorized"))
			g.Expect(controllerutils.ListModelWarmupJobs(g, ctx, k8sClient, ns.Name, warmup.Name)).To(BeEmpty())
		}, timeout, interval).Should(Succeed())
	})

	It("aggregates partial success and failure with target diagnostics", func() {
		ns := newModelWarmupNamespace("status")
		successNode := newModelWarmupNode("status-success", nil)
		failedNode := newModelWarmupNode("status-failed", nil)
		warmup := controllerutils.NewModelWarmup(ns.Name, "status", successNode.Name)
		warmup.Spec.Targets = []modelapi.ModelWarmupTarget{{
			Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{successNode.Name, failedNode.Name}},
		}}
		warmup.Spec.Policies.Parallelism = ptr.To[int32](2)
		Expect(k8sClient.Create(ctx, warmup)).To(Succeed())
		var jobs []batchv1.Job
		Eventually(func(g Gomega) {
			jobs = controllerutils.ListModelWarmupJobs(g, ctx, k8sClient, ns.Name, warmup.Name)
			g.Expect(jobs).To(HaveLen(2))
		}, timeout, interval).Should(Succeed())
		for _, job := range jobs {
			if controllerutils.ModelWarmupJobNode(&job) == successNode.Name {
				setJobSucceeded(job)
			} else {
				setJobFailed(job, "image pull failed")
			}
		}
		Eventually(func(g Gomega) {
			latest := getModelWarmup(g, warmup)
			g.Expect(latest.Status.Phase).To(Equal(modelapi.ModelWarmupDegraded))
			g.Expect(latest.Status.DesiredNodes).To(Equal(int32(2)))
			g.Expect(latest.Status.SucceededNodes).To(Equal(int32(1)))
			g.Expect(latest.Status.FailedNodes).To(Equal(int32(1)))
			g.Expect(condition(latest, "Degraded").Status).To(Equal(metav1.ConditionTrue))
			g.Expect(condition(latest, "Degraded").Reason).To(Equal("NodeFailed"))
			var failedTarget modelapi.ModelWarmupTargetStatus
			for _, target := range latest.Status.Targets {
				if target.NodeName == failedNode.Name {
					failedTarget = target
				}
			}
			g.Expect(failedTarget.Phase).To(Equal(modelapi.ModelWarmupTargetFailed))
			g.Expect(failedTarget.Reason).To(Equal("Failed"))
			g.Expect(failedTarget.Message).To(Equal("image pull failed"))
			g.Expect(failedTarget.LastTransitionTime).NotTo(BeNil())
		}, timeout, interval).Should(Succeed())
	})

	It("reports missing nodes and carries per-Job timeout and retry settings", func() {
		ns := newModelWarmupNamespace("missing")
		warmup := controllerutils.NewModelWarmup(ns.Name, "missing", "node-not-present")
		warmup.Spec.Policies.JobTimeoutSeconds = ptr.To[int64](1)
		warmup.Spec.Policies.RetryLimit = ptr.To[int32](4)
		Expect(k8sClient.Create(ctx, warmup)).To(Succeed())
		Eventually(func(g Gomega) {
			latest := getModelWarmup(g, warmup)
			g.Expect(latest.Status.Phase).To(Equal(modelapi.ModelWarmupFailed))
			g.Expect(latest.Status.DesiredNodes).To(Equal(int32(1)))
			g.Expect(latest.Status.FailedNodes).To(Equal(int32(1)))
			g.Expect(latest.Status.Targets[0].Reason).To(Equal("NodeNotFound"))
			g.Expect(latest.Status.Targets[0].Message).NotTo(BeEmpty())
			g.Expect(condition(latest, "Degraded").Status).To(Equal(metav1.ConditionTrue))
		}, timeout, interval).Should(Succeed())
	})

	It("waits with a clear status when a selector resolves no nodes", func() {
		ns := newModelWarmupNamespace("empty-selector")
		warmup := controllerutils.NewModelWarmup(ns.Name, "empty-selector", "unused")
		warmup.Spec.Targets = []modelapi.ModelWarmupTarget{{
			NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"pool": "not-present",
			}},
		}}
		Expect(k8sClient.Create(ctx, warmup)).To(Succeed())
		Eventually(func(g Gomega) {
			latest := getModelWarmup(g, warmup)
			g.Expect(latest.Status.Phase).To(Equal(modelapi.ModelWarmupPending))
			g.Expect(latest.Status.DesiredNodes).To(BeZero())
			g.Expect(condition(latest, "Progressing").Status).To(Equal(metav1.ConditionTrue))
			g.Expect(condition(latest, "Progressing").Reason).To(Equal("NoTargetsResolved"))
		}, timeout, interval).Should(Succeed())
	})

	It("sets the owner reference used to garbage-collect Jobs", func() {
		ns := newModelWarmupNamespace("gc")
		node := newModelWarmupNode("gc", nil)
		warmup := controllerutils.NewModelWarmup(ns.Name, "gc", node.Name)
		Expect(k8sClient.Create(ctx, warmup)).To(Succeed())
		var job batchv1.Job
		Eventually(func(g Gomega) {
			jobs := controllerutils.ListModelWarmupJobs(g, ctx, k8sClient, ns.Name, warmup.Name)
			g.Expect(jobs).To(HaveLen(1))
			job = jobs[0]
		}, timeout, interval).Should(Succeed())
		Expect(job.OwnerReferences).To(HaveLen(1))
		Expect(job.OwnerReferences[0].UID).To(Equal(warmup.UID))
		Expect(job.OwnerReferences[0].Controller).To(Equal(ptr.To(true)))
		Expect(k8sClient.Delete(ctx, warmup)).To(Succeed())
		Eventually(func(g Gomega) {
			latest := &modelapi.ModelWarmup{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(warmup), latest)
			g.Expect(client.IgnoreNotFound(err)).To(Succeed())
			g.Expect(err).To(HaveOccurred())
		}, timeout, interval).Should(Succeed())
	})
})

func newModelWarmupNamespace(prefix string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "modelwarmup-" + prefix + "-",
		Labels:       map[string]string{modelwarmup.ResourcePoolLabelKey: "integration"},
	}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
	return ns
}

func newModelWarmupNode(name string, labels map[string]string) *corev1.Node {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[modelwarmup.ResourcePoolLabelKey] = "integration"
	labels[modelwarmup.WarmupEnabledLabelKey] = modelwarmup.WarmupEnabledLabelValue
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "modelwarmup-" + name, Labels: labels}}
	Expect(k8sClient.Create(ctx, node)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
	return node
}

func getModelWarmup(g Gomega, warmup *modelapi.ModelWarmup) *modelapi.ModelWarmup {
	latest := &modelapi.ModelWarmup{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(warmup), latest)).To(Succeed())
	return latest
}

func setJobSucceeded(job batchv1.Job) {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&job), &job)).To(Succeed())
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
}

func setJobFailed(job batchv1.Job, message string) {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&job), &job)).To(Succeed())
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "Failed", Message: message,
	}}
	Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
}

func condition(warmup *modelapi.ModelWarmup, conditionType string) metav1.Condition {
	for _, item := range warmup.Status.Conditions {
		if item.Type == conditionType {
			return item
		}
	}
	return metav1.Condition{}
}

func nonDeletingJobs(jobs []batchv1.Job) []batchv1.Job {
	result := make([]batchv1.Job, 0, len(jobs))
	for _, job := range jobs {
		if job.DeletionTimestamp == nil {
			result = append(result, job)
		}
	}
	return result
}

func jobByName(jobs []batchv1.Job, name string) batchv1.Job {
	for _, job := range jobs {
		if job.Name == name {
			return job
		}
	}
	return batchv1.Job{}
}
