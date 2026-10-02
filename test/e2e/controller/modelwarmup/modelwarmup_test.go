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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	modelwarmup "github.com/vllm-project/aibrix/pkg/controller/modelwarmup"
)

const (
	keepOnFailureEnv        = "AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE"
	controllerNamespaceEnv  = "AIBRIX_ROLESET_CONTROLLER_NAMESPACE"
	controllerDeploymentEnv = "AIBRIX_ROLESET_CONTROLLER_DEPLOYMENT"
	controllerNamespace     = "aibrix-system"
	controllerDeployment    = "aibrix-controller-manager"
	testSelectorLabel       = "e2e.aibrix.ai/modelwarmup"
	testImage               = "aibrix/vllm-mock:nightly"
	controllerLogTailLines  = int64(300)
	diagnosticsTimeout      = 30 * time.Second
)

type testEnvironment struct {
	kube      kubernetes.Interface
	apiClient client.Client
	namespace string
}

func successfulWarmupCommand() []string {
	return []string{"python", "-c", "raise SystemExit(0)"}
}

func TestModelWarmupPreloadsImageForPullNeverPod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := newTestEnvironment(t, ctx)
	node := env.readyWarmupNodes(t, ctx, 1)[0]

	warmup := env.createWarmup(t, ctx, "preload", []modelapi.ModelWarmupTarget{{
		Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{node.Name}},
	}})
	env.waitForWarmupSucceeded(t, ctx, warmup, 1)
	env.waitForSucceededJobsAndPods(t, ctx, warmup.Name, []string{node.Name})
	env.verifyPullNeverPod(t, ctx, node.Name)
}

func TestModelWarmupRunsCombinedImagePreloadAndCustomActions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := newTestEnvironment(t, ctx)
	node := env.readyWarmupNodes(t, ctx, 1)[0]

	cacheMount := corev1.VolumeMount{Name: "cache", MountPath: "/cache"}
	precheckCommand := []string{
		"python", "-c", "import os; assert os.path.isdir('/cache'); open('/cache/precheck', 'w').close()",
	}
	prepareCommand := []string{"python", "-c", "from pathlib import Path; assert Path('/cache/precheck').is_file()"}
	warmup := &modelapi.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "combined-actions", Namespace: env.namespace},
		Spec: modelapi.ModelWarmupSpec{
			Targets: []modelapi.ModelWarmupTarget{{
				Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{node.Name}},
			}},
			ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
				Image:           testImage,
				Command:         successfulWarmupCommand(),
				ImagePullPolicy: corev1.PullIfNotPresent,
			}}},
			Custom: &modelapi.ModelWarmupCustomAction{
				InitContainers: []corev1.Container{{
					Name:         "precheck",
					Image:        testImage,
					Command:      slices.Clone(precheckCommand),
					VolumeMounts: []corev1.VolumeMount{cacheMount},
				}},
				Containers: []corev1.Container{{
					Name:         "prepare",
					Image:        testImage,
					Command:      slices.Clone(prepareCommand),
					VolumeMounts: []corev1.VolumeMount{cacheMount},
				}},
				Volumes: []corev1.Volume{{
					Name: "cache",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				}},
			},
		},
	}
	if err := env.apiClient.Create(ctx, warmup); err != nil {
		t.Fatal(err)
	}

	env.waitForWarmupSucceeded(t, ctx, warmup, 1)
	env.waitForSucceededJobsAndPods(t, ctx, warmup.Name, []string{node.Name})

	selector, err := env.warmupJobSelector(ctx, warmup.Name)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := env.kube.BatchV1().Jobs(env.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("got %d Jobs, want 1", len(jobs.Items))
	}
	job := jobs.Items[0]
	pod := job.Spec.Template.Spec
	if job.Annotations[modelwarmup.TargetNodeAnnotationKey] != node.Name {
		t.Fatalf("Job target node annotation = %q, want %q", job.Annotations[modelwarmup.TargetNodeAnnotationKey], node.Name)
	}
	if job.Annotations[modelwarmup.WarmupNameAnnotationKey] != warmup.Name {
		t.Fatalf("Job warmup name annotation = %q, want %q",
			job.Annotations[modelwarmup.WarmupNameAnnotationKey], warmup.Name)
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "precheck" {
		t.Fatalf("Job init containers = %+v, want precheck", pod.InitContainers)
	}
	if len(pod.Containers) != 2 || pod.Containers[0].Name != "image-0" || pod.Containers[1].Name != "prepare" {
		t.Fatalf("Job containers = %+v, want image-0 then prepare", pod.Containers)
	}
	if pod.Containers[0].Image != testImage || !slices.Equal(pod.Containers[0].Command, successfulWarmupCommand()) ||
		pod.Containers[0].ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("image-0 container = %+v, want image %q, successful command, and PullIfNotPresent",
			pod.Containers[0], testImage)
	}
	if pod.InitContainers[0].Image != testImage || !slices.Equal(pod.InitContainers[0].Command, precheckCommand) ||
		pod.Containers[1].Image != testImage || !slices.Equal(pod.Containers[1].Command, prepareCommand) {
		t.Fatalf("custom containers = init:%+v regular:%+v, want precheck and prepare commands",
			pod.InitContainers[0], pod.Containers[1])
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].Name != "cache" || pod.Volumes[0].EmptyDir == nil {
		t.Fatalf("Job volumes = %+v, want cache emptyDir", pod.Volumes)
	}
	if len(pod.InitContainers[0].VolumeMounts) != 1 || pod.InitContainers[0].VolumeMounts[0] != cacheMount ||
		len(pod.Containers[1].VolumeMounts) != 1 || pod.Containers[1].VolumeMounts[0] != cacheMount {
		t.Fatalf("custom container cache mounts = init:%+v regular:%+v, want %+v",
			pod.InitContainers[0].VolumeMounts, pod.Containers[1].VolumeMounts, cacheMount)
	}
	assertModelWarmupJobPlacementAndSafety(t, pod, node.Name)
}

func assertModelWarmupJobPlacementAndSafety(t *testing.T, pod corev1.PodSpec, nodeName string) {
	t.Helper()
	if pod.NodeName != "" || pod.Affinity == nil || pod.Affinity.NodeAffinity == nil ||
		pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("Job node placement = nodeName:%q affinity:%+v, want required node affinity", pod.NodeName, pod.Affinity)
	}
	terms := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 || terms[0].MatchFields[0].Key != "metadata.name" ||
		terms[0].MatchFields[0].Operator != corev1.NodeSelectorOpIn || len(terms[0].MatchFields[0].Values) != 1 ||
		terms[0].MatchFields[0].Values[0] != nodeName {
		t.Fatalf("Job node affinity terms = %+v, want node %q", terms, nodeName)
	}
	imageSecurity := pod.Containers[0].SecurityContext
	if pod.RestartPolicy != corev1.RestartPolicyNever ||
		pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		imageSecurity == nil || imageSecurity.AllowPrivilegeEscalation == nil || *imageSecurity.AllowPrivilegeEscalation {
		t.Fatalf("Job controller invariants = restart:%q automount:%v image security context:%+v",
			pod.RestartPolicy, pod.AutomountServiceAccountToken, imageSecurity)
	}
}

func TestModelWarmupDeduplicatesNodeNameAndSelector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := newTestEnvironment(t, ctx)
	node := env.readyWarmupNodes(t, ctx, 1)[0]
	env.setNodeLabel(t, ctx, node.Name, "deduplicate")

	warmup := env.createWarmup(t, ctx, "deduplicate", []modelapi.ModelWarmupTarget{
		{Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{node.Name}}},
		{NodeSelector: selectorFor("deduplicate")},
	})
	env.waitForWarmupSucceeded(t, ctx, warmup, 1)
	env.waitForSucceededJobsAndPods(t, ctx, warmup.Name, []string{node.Name})
}

func TestModelWarmupOnceIgnoresSelectorMatchAfterSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := newTestEnvironment(t, ctx)
	nodes := env.readyWarmupNodes(t, ctx, 2)
	env.setNodeLabel(t, ctx, nodes[0].Name, "expand")

	warmup := env.createWarmup(t, ctx, "expand", []modelapi.ModelWarmupTarget{{
		NodeSelector: selectorFor("expand"),
	}})
	env.waitForWarmupSucceeded(t, ctx, warmup, 1)
	env.waitForSucceededJobsAndPods(t, ctx, warmup.Name, []string{nodes[0].Name})

	env.setNodeLabel(t, ctx, nodes[1].Name, "expand")
	env.waitForWarmupSucceeded(t, ctx, warmup, 1)
	env.waitForSucceededJobsAndPods(t, ctx, warmup.Name, []string{nodes[0].Name})
}

func TestModelWarmupReportsFailedJobWithoutMutatingExistingWorkload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := newTestEnvironment(t, ctx)
	node := env.readyWarmupNodes(t, ctx, 1)[0]

	workload := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-workload", Namespace: env.namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](0),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "existing-workload"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "existing-workload"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "workload", Image: testImage,
					Command: []string{"python", "-c", "import time; time.sleep(300)"},
				}}},
			},
		},
	}
	if _, err := env.kube.AppsV1().Deployments(env.namespace).Create(ctx, workload, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := env.kube.AppsV1().Deployments(env.namespace).Get(ctx, workload.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	warmup := env.createWarmupWithImage(t, ctx, "failure", []modelapi.ModelWarmupTarget{{
		Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{node.Name}},
	}}, []string{"python", "-c", "raise SystemExit(1)"}, ptr.To[int32](1))
	env.waitForFailedJobsAndPods(t, ctx, warmup.Name, []string{node.Name})

	err = wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		latest := &modelapi.ModelWarmup{}
		if err := env.apiClient.Get(ctx, client.ObjectKeyFromObject(warmup), latest); err != nil {
			return false, err
		}
		if latest.Status.Phase != modelapi.ModelWarmupFailed || len(latest.Status.Targets) != 1 {
			return false, nil
		}
		target := latest.Status.Targets[0]
		if strings.TrimSpace(target.Reason) == "" || strings.TrimSpace(target.Message) == "" ||
			target.LastTransitionTime == nil {
			return false, fmt.Errorf(
				"failed target diagnostics incomplete: reason=%q message=%q transition=%v",
				target.Reason,
				target.Message,
				target.LastTransitionTime,
			)
		}
		for _, condition := range latest.Status.Conditions {
			if condition.Type == "Degraded" && condition.Status == metav1.ConditionTrue {
				if condition.Reason != "NodeFailed" || strings.TrimSpace(condition.Message) == "" {
					return false, fmt.Errorf(
						"degraded condition diagnostics incomplete: reason=%q message=%q",
						condition.Reason,
						condition.Message,
					)
				}
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		env.dumpWarmupDiagnostics(t, warmup.Name)
		t.Fatal(err)
	}

	after, err := env.kube.AppsV1().Deployments(env.namespace).Get(ctx, workload.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before.Spec.Replicas == nil || after.Spec.Replicas == nil || *before.Spec.Replicas != *after.Spec.Replicas {
		t.Fatalf("existing workload replicas changed: before=%v after=%v", before.Spec.Replicas, after.Spec.Replicas)
	}
	statusChanged := before.Status.Replicas != after.Status.Replicas ||
		before.Status.ReadyReplicas != after.Status.ReadyReplicas ||
		before.Status.AvailableReplicas != after.Status.AvailableReplicas ||
		before.Status.UpdatedReplicas != after.Status.UpdatedReplicas
	if statusChanged {
		t.Fatalf("existing workload status changed: before=%+v after=%+v", before.Status, after.Status)
	}
}

func selectorFor(value string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{testSelectorLabel: value}}
}

func newTestEnvironment(t *testing.T, ctx context.Context) *testEnvironment {
	t.Helper()
	config, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		t.Fatal(err)
	}
	restConfig, err := clientcmd.NewDefaultClientConfig(
		*config,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := modelapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	apiClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	namespace := fmt.Sprintf("modelwarmup-e2e-%d", time.Now().UnixNano())
	if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{
			modelwarmup.ResourcePoolLabelKey: "e2e",
		}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() && strings.EqualFold(os.Getenv(keepOnFailureEnv), "true") {
			t.Logf(
				"preserving namespace %q; inspect with: kubectl get modelwarmups,jobs,pods -n %s",
				namespace,
				namespace,
			)
			return
		}
		_ = kube.CoreV1().Namespaces().Delete(
			context.Background(),
			namespace,
			metav1.DeleteOptions{},
		)
	})
	return &testEnvironment{kube: kube, apiClient: apiClient, namespace: namespace}
}

func (e *testEnvironment) readyWarmupNodes(
	t *testing.T,
	ctx context.Context,
	count int,
) []corev1.Node {
	t.Helper()
	nodes, err := e.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	ready := make([]corev1.Node, 0, count)
	for _, node := range nodes.Items {
		if nodeCanRunWarmup(node) {
			ready = append(ready, node)
		}
	}
	if len(ready) < count {
		t.Fatalf("requires %d Ready Kubernetes nodes capable of warmup, found %d", count, len(ready))
	}
	for i := range ready[:count] {
		e.setNodeAuthorizationLabels(t, ctx, ready[i].Name)
	}
	return ready[:count]
}

func nodeCanRunWarmup(node corev1.Node) bool {
	if node.Spec.Unschedulable || node.Status.NodeInfo.KubeletVersion == "" ||
		node.Status.NodeInfo.ContainerRuntimeVersion == "" {
		return false
	}
	for key := range node.Labels {
		if key == "node-role.kubernetes.io/control-plane" || key == "node-role.kubernetes.io/master" {
			return false
		}
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return false
		}
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func TestNodeCanRunWarmupRejectsControlPlaneNode(t *testing.T) {
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule,
		}}},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion: "v1.31.0", ContainerRuntimeVersion: "containerd://1.7.0",
			},
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: corev1.ConditionTrue,
			}},
		},
	}

	if nodeCanRunWarmup(node) {
		t.Fatal("expected a control-plane node to be ineligible for warmup")
	}
}

func (e *testEnvironment) setNodeAuthorizationLabels(t *testing.T, ctx context.Context, nodeName string) {
	t.Helper()
	node, err := e.kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	oldPool, hadPool := node.Labels[modelwarmup.ResourcePoolLabelKey]
	oldEnabled, hadEnabled := node.Labels[modelwarmup.WarmupEnabledLabelKey]
	node.Labels[modelwarmup.ResourcePoolLabelKey] = "e2e"
	node.Labels[modelwarmup.WarmupEnabledLabelKey] = modelwarmup.WarmupEnabledLabelValue
	if _, err := e.kube.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		latest, err := e.kube.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		if err != nil {
			return
		}
		if hadPool {
			latest.Labels[modelwarmup.ResourcePoolLabelKey] = oldPool
		} else {
			delete(latest.Labels, modelwarmup.ResourcePoolLabelKey)
		}
		if hadEnabled {
			latest.Labels[modelwarmup.WarmupEnabledLabelKey] = oldEnabled
		} else {
			delete(latest.Labels, modelwarmup.WarmupEnabledLabelKey)
		}
		_, _ = e.kube.CoreV1().Nodes().Update(context.Background(), latest, metav1.UpdateOptions{})
	})
}

func (e *testEnvironment) setNodeLabel(
	t *testing.T,
	ctx context.Context,
	nodeName string,
	value string,
) {
	t.Helper()
	node, err := e.kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	previous, existed := node.Labels[testSelectorLabel]
	node.Labels[testSelectorLabel] = value
	if _, err := e.kube.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		latest, err := e.kube.CoreV1().Nodes().Get(
			context.Background(),
			nodeName,
			metav1.GetOptions{},
		)
		if err != nil {
			return
		}
		if existed {
			latest.Labels[testSelectorLabel] = previous
		} else {
			delete(latest.Labels, testSelectorLabel)
		}
		_, _ = e.kube.CoreV1().Nodes().Update(
			context.Background(),
			latest,
			metav1.UpdateOptions{},
		)
	})
}

func (e *testEnvironment) createWarmup(
	t *testing.T,
	ctx context.Context,
	name string,
	targets []modelapi.ModelWarmupTarget,
) *modelapi.ModelWarmup {
	t.Helper()
	warmup := &modelapi.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.namespace},
		Spec: modelapi.ModelWarmupSpec{
			Targets: targets,
			ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
				Image:           testImage,
				Command:         successfulWarmupCommand(),
				ImagePullPolicy: corev1.PullIfNotPresent,
			}}},
		},
	}
	if err := e.apiClient.Create(ctx, warmup); err != nil {
		t.Fatal(err)
	}
	return warmup
}

func (e *testEnvironment) createWarmupWithImage(
	t *testing.T,
	ctx context.Context,
	name string,
	targets []modelapi.ModelWarmupTarget,
	command []string,
	retryLimit *int32,
) *modelapi.ModelWarmup {
	t.Helper()
	warmup := &modelapi.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.namespace},
		Spec: modelapi.ModelWarmupSpec{
			Targets: targets,
			ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
				Image: testImage, Command: command, ImagePullPolicy: corev1.PullIfNotPresent,
			}}},
			Policies: &modelapi.ModelWarmupPolicies{RetryLimit: retryLimit},
		},
	}
	if err := e.apiClient.Create(ctx, warmup); err != nil {
		t.Fatal(err)
	}
	return warmup
}

func (e *testEnvironment) waitForWarmupSucceeded(
	t *testing.T,
	ctx context.Context,
	warmup *modelapi.ModelWarmup,
	desiredNodes int32,
) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		3*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			latest := &modelapi.ModelWarmup{}
			if err := e.apiClient.Get(ctx, client.ObjectKeyFromObject(warmup), latest); err != nil {
				return false, err
			}
			if latest.Status.Phase == modelapi.ModelWarmupDegraded ||
				latest.Status.Phase == modelapi.ModelWarmupFailed {
				return false, fmt.Errorf("warmup failed: %+v", latest.Status.Targets)
			}
			return latest.Status.Phase == modelapi.ModelWarmupSucceeded &&
				latest.Status.DesiredNodes == desiredNodes, nil
		},
	)
	if err != nil {
		e.dumpWarmupDiagnostics(t, warmup.Name)
		t.Fatal(err)
	}
}

func (e *testEnvironment) waitForSucceededJobsAndPods(
	t *testing.T,
	ctx context.Context,
	warmupName string,
	nodeNames []string,
) {
	t.Helper()
	selector, err := e.warmupJobSelector(ctx, warmupName)
	if err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		3*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			jobs, err := e.kube.BatchV1().Jobs(e.namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil || len(jobs.Items) != len(nodeNames) {
				return false, err
			}
			seen := make(map[string]bool, len(nodeNames))
			for _, job := range jobs.Items {
				if job.Status.Succeeded == 0 || job.Status.Failed > 0 {
					return false, nil
				}
				succeeded, err := e.jobPodSucceeded(ctx, job)
				if err != nil || !succeeded {
					return false, err
				}
				seen[job.Annotations[modelwarmup.TargetNodeAnnotationKey]] = true
			}
			for _, nodeName := range nodeNames {
				if !seen[nodeName] {
					return false, nil
				}
			}
			return true, nil
		},
	)
	if err != nil {
		e.dumpWarmupDiagnostics(t, warmupName)
		t.Fatal(err)
	}
}

func (e *testEnvironment) waitForFailedJobsAndPods(
	t *testing.T,
	ctx context.Context,
	warmupName string,
	nodeNames []string,
) {
	t.Helper()
	selector, err := e.warmupJobSelector(ctx, warmupName)
	if err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		jobs, err := e.kube.BatchV1().Jobs(e.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return false, err
		}
		if len(jobs.Items) != len(nodeNames) {
			return false, nil
		}
		for _, job := range jobs.Items {
			if job.Status.Failed == 0 {
				return false, nil
			}
			pods, err := e.kube.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "job-name=" + job.Name,
			})
			if err != nil {
				return false, err
			}
			failedPod := false
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodFailed {
					failedPod = true
					break
				}
			}
			if !failedPod {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		e.dumpWarmupDiagnostics(t, warmupName)
		t.Fatal(err)
	}
}

func (e *testEnvironment) dumpWarmupDiagnostics(t *testing.T, warmupName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), diagnosticsTimeout)
	defer cancel()

	warmup := &modelapi.ModelWarmup{}
	warmupErr := e.apiClient.Get(ctx, client.ObjectKey{Namespace: e.namespace, Name: warmupName}, warmup)
	jobListOptions := metav1.ListOptions{}
	if warmupErr == nil {
		jobListOptions.LabelSelector = modelwarmup.WarmupLabelKey + "=" + string(warmup.UID)
	}
	jobs, jobsErr := e.kube.BatchV1().Jobs(e.namespace).List(ctx, jobListOptions)
	pods, podsErr := e.kube.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{})
	events, eventsErr := e.kube.CoreV1().Events(e.namespace).List(ctx, metav1.ListOptions{})

	t.Logf("ModelWarmup diagnostics: warmup=%s err=%v", diagnosticJSON(warmup), warmupErr)
	t.Logf("ModelWarmup diagnostics: jobs=%s err=%v", diagnosticJSON(jobs), jobsErr)
	t.Logf("ModelWarmup diagnostics: pods=%s err=%v", diagnosticJSON(pods), podsErr)
	t.Logf("ModelWarmup diagnostics: events=%s err=%v", diagnosticJSON(events), eventsErr)
	e.dumpControllerDiagnostics(t, ctx)
}

func (e *testEnvironment) warmupJobSelector(ctx context.Context, warmupName string) (string, error) {
	warmup := &modelapi.ModelWarmup{}
	if err := e.apiClient.Get(ctx, client.ObjectKey{Namespace: e.namespace, Name: warmupName}, warmup); err != nil {
		return "", err
	}
	return modelwarmup.WarmupLabelKey + "=" + string(warmup.UID), nil
}

func (e *testEnvironment) dumpControllerDiagnostics(t *testing.T, ctx context.Context) {
	t.Helper()
	namespace := envOrDefault(controllerNamespaceEnv, controllerNamespace)
	deploymentName := envOrDefault(controllerDeploymentEnv, controllerDeployment)
	deployment, err := e.kube.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		t.Logf("ModelWarmup diagnostics: get controller Deployment %s/%s: %v", namespace, deploymentName, err)
		return
	}
	t.Logf("ModelWarmup diagnostics: controller deployment=%s", diagnosticJSON(deployment))

	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		t.Logf("ModelWarmup diagnostics: build controller selector for %s/%s: %v", namespace, deploymentName, err)
		return
	}
	controllerPods, err := e.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		t.Logf("ModelWarmup diagnostics: list controller Pods for %s/%s: %v", namespace, deploymentName, err)
		return
	}
	t.Logf("ModelWarmup diagnostics: controller pods=%s", diagnosticJSON(controllerPods))
	for i := range controllerPods.Items {
		pod := &controllerPods.Items[i]
		logs, err := e.kube.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "manager",
			TailLines: ptr.To(controllerLogTailLines),
		}).DoRaw(ctx)
		if err != nil {
			t.Logf("ModelWarmup diagnostics: get controller logs from %s/%s: %v", namespace, pod.Name, err)
			continue
		}
		t.Logf("ModelWarmup diagnostics: controller logs from %s/%s:\n%s", namespace, pod.Name, logs)
	}
}

func envOrDefault(name, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return defaultValue
}

func diagnosticJSON(value interface{}) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(data)
}

func (e *testEnvironment) jobPodSucceeded(ctx context.Context, job batchv1.Job) (bool, error) {
	pods, err := e.kube.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job.Name,
	})
	if err != nil {
		return false, err
	}
	if len(pods.Items) == 0 {
		return false, nil
	}
	if len(pods.Items) != 1 {
		return false, fmt.Errorf("Job %q has %d Pods, want 1", job.Name, len(pods.Items))
	}
	if pods.Items[0].Status.Phase == corev1.PodFailed {
		return false, fmt.Errorf("Job %q Pod failed: %s", job.Name, pods.Items[0].Status.Message)
	}
	return pods.Items[0].Status.Phase == corev1.PodSucceeded, nil
}

func (e *testEnvironment) verifyPullNeverPod(
	t *testing.T,
	ctx context.Context,
	nodeName string,
) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "verify-cache", Namespace: e.namespace},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "verify",
				Image:           testImage,
				ImagePullPolicy: corev1.PullNever,
				Command:         successfulWarmupCommand(),
			}},
		},
	}
	if _, err := e.kube.CoreV1().Pods(e.namespace).Create(
		ctx,
		pod,
		metav1.CreateOptions{},
	); err != nil {
		t.Fatal(err)
	}
	err := wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		2*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			latest, err := e.kube.CoreV1().Pods(e.namespace).Get(
				ctx,
				pod.Name,
				metav1.GetOptions{},
			)
			if err != nil {
				return false, err
			}
			if latest.Status.Phase == corev1.PodFailed {
				return false, fmt.Errorf("verification Pod failed: %s", latest.Status.Message)
			}
			return latest.Status.Phase == corev1.PodSucceeded, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestModelWarmupWebhookRejectsInvalidSpecs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	config, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		t.Fatal(err)
	}
	restConfig, err := clientcmd.NewDefaultClientConfig(
		*config,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := modelapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	apiClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	newWarmup := func(name string) *modelapi.ModelWarmup {
		return &modelapi.ModelWarmup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: modelapi.ModelWarmupSpec{
				Targets: []modelapi.ModelWarmupTarget{{
					Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{"worker-0"}},
				}},
				ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
					Image: testImage, Command: successfulWarmupCommand(),
				}}},
			},
		}
	}
	cases := map[string]func(*modelapi.ModelWarmup){
		"missing command": func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images[0].Command = nil
		},
		"empty selector": func(w *modelapi.ModelWarmup) {
			w.Spec.Targets = []modelapi.ModelWarmupTarget{{NodeSelector: &metav1.LabelSelector{}}}
		},
		"invalid pull policy": func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images[0].ImagePullPolicy = "Invalid"
		},
		"zero parallelism": func(w *modelapi.ModelWarmup) {
			zero := int32(0)
			w.Spec.Policies = &modelapi.ModelWarmupPolicies{Parallelism: &zero}
		},
		"duplicate image": func(w *modelapi.ModelWarmup) {
			w.Spec.ImagePreload.Images = append(
				w.Spec.ImagePreload.Images,
				w.Spec.ImagePreload.Images[0],
			)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			warmup := newWarmup(fmt.Sprintf("modelwarmup-webhook-%d", time.Now().UnixNano()))
			mutate(warmup)
			if err := apiClient.Create(ctx, warmup); err == nil {
				t.Fatalf("expected admission webhook to reject %s", name)
			}
		})
	}
}

func TestModelWarmupWebhookPreservesOmittedDefaults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env := newTestEnvironment(t, ctx)
	warmup := &modelapi.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook-defaults", Namespace: env.namespace},
		Spec: modelapi.ModelWarmupSpec{
			Targets: []modelapi.ModelWarmupTarget{{
				Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{"worker-0"}},
			}},
			ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
				Image: testImage, Command: successfulWarmupCommand(),
			}}},
		},
	}
	if err := env.apiClient.Create(ctx, warmup); err != nil {
		t.Fatal(err)
	}
	latest := &modelapi.ModelWarmup{}
	if err := env.apiClient.Get(ctx, client.ObjectKeyFromObject(warmup), latest); err != nil {
		t.Fatal(err)
	}
	if latest.Spec.Policies != nil {
		t.Fatalf("expected omitted policies to remain absent: %+v", latest.Spec.Policies)
	}
	if latest.Spec.ImagePreload.Images[0].ImagePullPolicy != "" {
		t.Fatalf("unexpected persisted image pull policy: %q", latest.Spec.ImagePreload.Images[0].ImagePullPolicy)
	}
}
