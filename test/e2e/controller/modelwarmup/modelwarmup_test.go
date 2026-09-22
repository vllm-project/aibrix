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
	"fmt"
	"os"
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
)

const (
	keepOnFailureEnv  = "AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE"
	testSelectorLabel = "e2e.aibrix.ai/modelwarmup"
	warmupNameLabel   = "model.aibrix.ai/warmup"
	testImage         = "busybox:1.36"
)

type testEnvironment struct {
	kube      kubernetes.Interface
	apiClient client.Client
	namespace string
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
	env.waitForTargetSources(t, ctx, warmup, 1, 2)
	env.waitForSucceededJobsAndPods(t, ctx, warmup.Name, []string{node.Name})
}

func TestModelWarmupCreatesJobForNewSelectorMatch(t *testing.T) {
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
	env.waitForWarmupSucceeded(t, ctx, warmup, 2)
	env.waitForSucceededJobsAndPods(t, ctx, warmup.Name, []string{nodes[0].Name, nodes[1].Name})
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
					Name: "workload", Image: testImage, Command: []string{"sh", "-c", "sleep 300"},
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
	}}, []string{"sh", "-c", "exit 1"}, ptr.To[int32](1))
	env.waitForFailedJobsAndPods(t, ctx, warmup.Name, []string{node.Name})

	err = wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		latest := &modelapi.ModelWarmup{}
		if err := env.apiClient.Get(ctx, client.ObjectKeyFromObject(warmup), latest); err != nil {
			return false, err
		}
		if latest.Status.Phase != modelapi.ModelWarmupDegraded || len(latest.Status.Targets) != 1 {
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
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
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
	return ready[:count]
}

func nodeCanRunWarmup(node corev1.Node) bool {
	if node.Spec.Unschedulable || node.Status.NodeInfo.KubeletVersion == "" ||
		node.Status.NodeInfo.ContainerRuntimeVersion == "" {
		return false
	}
	// Warmup Jobs tolerate all taints, so a Ready tainted node is a valid target.
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func TestNodeCanRunWarmupAllowsTaintedNode(t *testing.T) {
	node := corev1.Node{
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

	if !nodeCanRunWarmup(node) {
		t.Fatal("expected a Ready tainted node to be eligible for an all-taints-tolerating warmup Job")
	}
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
				Command:         []string{"sh", "-c", "exit 0"},
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
		t.Fatal(err)
	}
}

func (e *testEnvironment) waitForTargetSources(
	t *testing.T,
	ctx context.Context,
	warmup *modelapi.ModelWarmup,
	targets int,
	sources int,
) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			latest := &modelapi.ModelWarmup{}
			if err := e.apiClient.Get(ctx, client.ObjectKeyFromObject(warmup), latest); err != nil {
				return false, err
			}
			return len(latest.Status.Targets) == targets &&
				len(latest.Status.Targets[0].Sources) == sources, nil
		},
	)
	if err != nil {
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
	err := wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		3*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			jobs, err := e.kube.BatchV1().Jobs(e.namespace).List(ctx, metav1.ListOptions{
				LabelSelector: warmupNameLabel + "=" + warmupName,
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
				seen[job.Spec.Template.Spec.NodeName] = true
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
	err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		jobs, err := e.kube.BatchV1().Jobs(e.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: warmupNameLabel + "=" + warmupName,
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
		t.Fatal(err)
	}
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
				Command:         []string{"sh", "-c", "exit 0"},
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
					Image: testImage, Command: []string{"sh", "-c", "exit 0"},
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

func TestModelWarmupWebhookDefaultsPersistedValues(t *testing.T) {
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
				Image: testImage, Command: []string{"sh", "-c", "exit 0"},
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
	if latest.Spec.Policies == nil {
		t.Fatal("webhook did not persist policy defaults")
	}
	policies := latest.Spec.Policies
	invalidDefaults := policies.Parallelism == nil ||
		*policies.Parallelism != modelapi.DefaultModelWarmupParallelism ||
		policies.GlobalTimeoutSeconds == nil ||
		*policies.GlobalTimeoutSeconds != modelapi.DefaultModelWarmupGlobalTimeoutSeconds ||
		policies.RetryLimit == nil ||
		*policies.RetryLimit != modelapi.DefaultModelWarmupRetryLimit ||
		policies.TTLSecondsAfterFinished == nil ||
		*policies.TTLSecondsAfterFinished != modelapi.DefaultModelWarmupTTLSecondsAfterFinished
	if invalidDefaults {
		t.Fatalf("unexpected persisted policy defaults: %+v", policies)
	}
	if latest.Spec.ImagePreload.Images[0].ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("unexpected persisted image pull policy: %q", latest.Spec.ImagePreload.Images[0].ImagePullPolicy)
	}
}
