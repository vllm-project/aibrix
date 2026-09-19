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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
)

func init() {
	ctrllog.SetLogger(logr.Discard())
}

const (
	podSetE2EImage                = "aibrix/inplace-e2e:v1"
	podSetFixtureLabel            = "e2e.aibrix.ai/podset"
	podSetKeepOnFailureEnv        = "AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE"
	podSetControllerNamespace     = "aibrix-system"
	podSetControllerLabelSelector = "control-plane=controller-manager"
	podSetPollInterval            = 500 * time.Millisecond
	podSetPollTimeout             = 2 * time.Minute
	podSetCleanupTimeout          = 2 * time.Minute
)

type podSetHarness struct {
	namespace  string
	kubeClient kubernetes.Interface
	apiClient  client.Client
}

func newPodSetHarness(t *testing.T, ctx context.Context) *podSetHarness {
	t.Helper()

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatalf("build Kubernetes client configuration: %v", err)
	}
	config.QPS = 50
	config.Burst = 100

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	testScheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		t.Fatalf("register Kubernetes API scheme: %v", err)
	}
	if err := orchestrationv1alpha1.AddToScheme(testScheme); err != nil {
		t.Fatalf("register AIBrix orchestration API scheme: %v", err)
	}
	apiClient, err := client.New(config, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("create AIBrix API client: %v", err)
	}

	namespace := "podset-e2e-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create test namespace %s: %v", namespace, err)
	}

	harness := &podSetHarness{
		namespace:  namespace,
		kubeClient: kubeClient,
		apiClient:  apiClient,
	}
	t.Cleanup(func() { harness.cleanup(t) })
	return harness
}

func (h *podSetHarness) cleanup(t *testing.T) {
	t.Helper()
	if t.Failed() {
		h.logDiagnostics(t)
		if strings.EqualFold(strings.TrimSpace(os.Getenv(podSetKeepOnFailureEnv)), "true") {
			t.Logf("preserving PodSet E2E namespace %s after failure", h.namespace)
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), podSetCleanupTimeout)
	defer cancel()
	propagation := metav1.DeletePropagationForeground
	err := h.kubeClient.CoreV1().Namespaces().Delete(ctx, h.namespace, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete PodSet E2E namespace %s: %v", h.namespace, err)
		return
	}
	if err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetCleanupTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := h.kubeClient.CoreV1().Namespaces().Get(ctx, h.namespace, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}); err != nil {
		t.Errorf("wait for PodSet E2E namespace %s deletion: %v", h.namespace, err)
	}
}

func (h *podSetHarness) logDiagnostics(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	podSets := &orchestrationv1alpha1.PodSetList{}
	if err := h.apiClient.List(ctx, podSets, client.InNamespace(h.namespace)); err != nil {
		t.Logf("list PodSets in %s: %v", h.namespace, err)
	} else {
		t.Logf("PodSets in %s: %+v", h.namespace, podSets.Items)
	}
	if pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{}); err != nil {
		t.Logf("list Pods in %s: %v", h.namespace, err)
	} else {
		t.Logf("Pods in %s: %+v", h.namespace, pods.Items)
	}
	if events, err := h.kubeClient.CoreV1().Events(h.namespace).List(ctx, metav1.ListOptions{}); err != nil {
		t.Logf("list Events in %s: %v", h.namespace, err)
	} else {
		t.Logf("Events in %s: %+v", h.namespace, events.Items)
	}
	controllerPods, err := h.kubeClient.CoreV1().Pods(podSetControllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: podSetControllerLabelSelector,
	})
	if err != nil {
		t.Logf("list AIBrix controller Pods: %v", err)
		return
	}
	for i := range controllerPods.Items {
		logs, err := h.kubeClient.CoreV1().Pods(podSetControllerNamespace).
			GetLogs(controllerPods.Items[i].Name, &corev1.PodLogOptions{Container: "manager", TailLines: ptr.To[int64](200)}).
			DoRaw(ctx)
		if err != nil {
			t.Logf("get controller logs from %s: %v", controllerPods.Items[i].Name, err)
			continue
		}
		t.Logf("controller logs from %s:\n%s", controllerPods.Items[i].Name, logs)
	}
}

func (h *podSetHarness) createPodSet(ctx context.Context, t *testing.T, podSet *orchestrationv1alpha1.PodSet) {
	t.Helper()
	if err := h.apiClient.Create(ctx, podSet); err != nil {
		t.Fatalf("create PodSet %s/%s: %v", podSet.Namespace, podSet.Name, err)
	}
}

func (h *podSetHarness) updatePodSetSize(ctx context.Context, name string, size int32) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		podSet := &orchestrationv1alpha1.PodSet{}
		if err := h.apiClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, podSet); err != nil {
			return err
		}
		podSet.Spec.PodGroupSize = size
		return h.apiClient.Update(ctx, podSet)
	})
}

func (h *podSetHarness) listPods(ctx context.Context, name string) ([]corev1.Pod, error) {
	pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: podSetFixtureLabel + "=" + name,
	})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (h *podSetHarness) waitForReadyPods(ctx context.Context, name string, count int) ([]corev1.Pod, error) {
	var observed []corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return false, retryablePodSetAPIError(err)
			}
			observed = pods
			if len(pods) != count {
				return false, nil
			}
			for i := range pods {
				if !podReady(&pods[i]) || !pods[i].DeletionTimestamp.IsZero() {
					return false, nil
				}
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("wait for %d Ready Pods for PodSet %s/%s (last count %d): %w",
			count, h.namespace, name, len(observed), err)
	}
	return observed, nil
}

func (h *podSetHarness) waitForStatus(
	ctx context.Context,
	name string,
	phase orchestrationv1alpha1.PodSetPhase,
	total, ready int32,
) error {
	var last orchestrationv1alpha1.PodSetStatus
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			podSet := &orchestrationv1alpha1.PodSet{}
			err := h.apiClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, podSet)
			if err != nil {
				return false, retryablePodSetAPIError(err)
			}
			last = podSet.Status
			return last.Phase == phase && last.TotalPods == total && last.ReadyPods == ready, nil
		})
	if err != nil {
		return fmt.Errorf("wait for PodSet %s/%s status phase=%s total=%d ready=%d (last=%+v): %w",
			h.namespace, name, phase, total, ready, last, err)
	}
	return nil
}

func (h *podSetHarness) waitForScaleInDrain(ctx context.Context, name string, expectedIndex int) (*corev1.Pod, error) {
	var observed []corev1.Pod
	var matched *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return false, retryablePodSetAPIError(err)
			}
			observed = pods
			matched = nil
			for i := range pods {
				if !scaleInDrainStarted(&pods[i]) {
					continue
				}
				if matched != nil {
					return false, fmt.Errorf(
						"multiple Pods are draining during single-replica scale-down: %s and %s",
						matched.Name,
						pods[i].Name,
					)
				}
				matched = pods[i].DeepCopy()
			}
			if matched == nil {
				return false, nil
			}
			index, ok := podIndex(matched)
			if !ok {
				return false, fmt.Errorf("draining Pod %s has an invalid index label", matched.Name)
			}
			if index != expectedIndex {
				return false, fmt.Errorf("PodSet selected index %d for scale-down, want highest index %d", index, expectedIndex)
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("wait for PodSet %s/%s index %d scale-in drain (last Pods=%s): %w",
			h.namespace, name, expectedIndex, describePods(observed), err)
	}
	return matched, nil
}

func (h *podSetHarness) waitForDrainCancellation(
	ctx context.Context,
	name string,
	expectedUIDs map[int]types.UID,
) error {
	var observed []corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return false, retryablePodSetAPIError(err)
			}
			observed = pods
			if len(pods) != len(expectedUIDs) {
				return false, nil
			}
			uids, err := podUIDsByIndex(pods)
			if err != nil {
				return false, err
			}
			for index, expectedUID := range expectedUIDs {
				if uids[index] != expectedUID {
					return false, fmt.Errorf(
						"Pod index %d UID changed from %s to %s while cancelling drain",
						index,
						expectedUID,
						uids[index],
					)
				}
			}
			for i := range pods {
				annotations := pods[i].Annotations
				if annotations[aibrixconst.PodDrainingAnnotationKey] != "" ||
					annotations[aibrixconst.PodDrainStartTimeAnnotationKey] != "" ||
					annotations[aibrixconst.PodDrainReasonAnnotationKey] != "" ||
					annotations[aibrixconst.PodDrainTargetActionAnnotationKey] != "" {
					return false, nil
				}
			}
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("wait for PodSet %s/%s drain cancellation (last Pods=%s): %w",
			h.namespace, name, describePods(observed), err)
	}
	return nil
}

func (h *podSetHarness) ensurePodNotDeletingUntil(
	ctx context.Context,
	name string,
	uid types.UID,
	deadline time.Time,
) error {
	duration := time.Until(deadline)
	if duration <= 0 {
		return fmt.Errorf("Pod UID %s drain deadline %s has already passed", uid, deadline)
	}
	return observeConsistently(ctx, podSetPollInterval, duration,
		func(ctx context.Context) error {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return retryablePodSetAPIError(err)
			}
			for i := range pods {
				if pods[i].UID != uid {
					continue
				}
				if !pods[i].DeletionTimestamp.IsZero() {
					return fmt.Errorf("Pod %s entered deletion before drain observation completed", pods[i].Name)
				}
				return nil
			}
			return fmt.Errorf("Pod UID %s disappeared before drain observation completed", uid)
		})
}

func (h *podSetHarness) waitForPodDeleting(ctx context.Context, name string, uid types.UID) (*corev1.Pod, error) {
	var observed []corev1.Pod
	var deleting *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return false, retryablePodSetAPIError(err)
			}
			observed = pods
			for i := range pods {
				if pods[i].UID != uid {
					continue
				}
				if pods[i].DeletionTimestamp.IsZero() {
					return false, nil
				}
				deleting = pods[i].DeepCopy()
				return true, nil
			}
			return false, fmt.Errorf("Pod UID %s disappeared before its deletion timestamp could be observed", uid)
		})
	if err != nil {
		return nil, fmt.Errorf("wait for Pod UID %s to begin deleting in PodSet %s/%s (last Pods=%s): %w",
			uid, h.namespace, name, describePods(observed), err)
	}
	return deleting, nil
}

func (h *podSetHarness) waitForPodUIDGone(ctx context.Context, name string, uid types.UID) error {
	var observed []corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return false, retryablePodSetAPIError(err)
			}
			observed = pods
			for i := range pods {
				if pods[i].UID == uid {
					return false, nil
				}
			}
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("wait for Pod UID %s to leave PodSet %s/%s (last Pods=%s): %w",
			uid, h.namespace, name, describePods(observed), err)
	}
	return nil
}

func (h *podSetHarness) deletePodByUID(ctx context.Context, name string, uid types.UID) error {
	pods, err := h.listPods(ctx, name)
	if err != nil {
		return err
	}
	for i := range pods {
		if pods[i].UID != uid {
			continue
		}
		return h.kubeClient.CoreV1().Pods(h.namespace).Delete(ctx, pods[i].Name, metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](0),
		})
	}
	return fmt.Errorf("PodSet %s/%s has no Pod with UID %s", h.namespace, name, uid)
}

func (h *podSetHarness) waitForReplacement(
	ctx context.Context,
	name string,
	index int,
	oldUID types.UID,
	expectedCount int,
) (*corev1.Pod, error) {
	var observed []corev1.Pod
	var replacement *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return false, retryablePodSetAPIError(err)
			}
			observed = pods
			replacement = nil
			active := 0
			indexes := make(map[int]struct{}, len(pods))
			for i := range pods {
				if !pods[i].DeletionTimestamp.IsZero() {
					continue
				}
				active++
				currentIndex, ok := podIndex(&pods[i])
				if !ok {
					return false, fmt.Errorf("Pod %s has an invalid index label", pods[i].Name)
				}
				if _, duplicate := indexes[currentIndex]; duplicate {
					return false, fmt.Errorf("multiple active Pods have index %d", currentIndex)
				}
				indexes[currentIndex] = struct{}{}
				if currentIndex == index && pods[i].UID != oldUID && podReady(&pods[i]) {
					replacement = pods[i].DeepCopy()
				}
			}
			return active == expectedCount && len(indexes) == expectedCount && replacement != nil, nil
		})
	if err != nil {
		return nil, fmt.Errorf("wait for replacement of PodSet %s/%s index %d UID %s (last Pods=%s): %w",
			h.namespace, name, index, oldUID, describePods(observed), err)
	}
	return replacement, nil
}

func (h *podSetHarness) ensureStableReadyPods(
	ctx context.Context,
	name string,
	count int,
	duration time.Duration,
) error {
	return observeConsistently(ctx, podSetPollInterval, duration,
		func(ctx context.Context) error {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return retryablePodSetAPIError(err)
			}
			if len(pods) != count {
				return fmt.Errorf("PodSet %s/%s has %d Pods during stability window, want %d: %s",
					h.namespace, name, len(pods), count, describePods(pods))
			}
			if _, err := podUIDsByIndex(pods); err != nil {
				return err
			}
			for i := range pods {
				if !podReady(&pods[i]) || !pods[i].DeletionTimestamp.IsZero() {
					return fmt.Errorf("PodSet %s/%s became unstable: %s", h.namespace, name, describePods(pods))
				}
			}
			return nil
		})
}

func (h *podSetHarness) deletePodSetForeground(ctx context.Context, name string) error {
	podSet := &orchestrationv1alpha1.PodSet{}
	if err := h.apiClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, podSet); err != nil {
		return client.IgnoreNotFound(err)
	}
	return client.IgnoreNotFound(h.apiClient.Delete(
		ctx,
		podSet,
		client.PropagationPolicy(metav1.DeletePropagationForeground),
	))
}

func (h *podSetHarness) waitForPodSetGone(ctx context.Context, name string) error {
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			podSet := &orchestrationv1alpha1.PodSet{}
			err := h.apiClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, podSet)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, retryablePodSetAPIError(err)
		})
	if err != nil {
		return fmt.Errorf("wait for PodSet %s/%s deletion: %w", h.namespace, name, err)
	}
	return nil
}

func (h *podSetHarness) waitForNoPods(ctx context.Context, name string) error {
	var observed []corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, podSetPollInterval, podSetPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.listPods(ctx, name)
			if err != nil {
				return false, retryablePodSetAPIError(err)
			}
			observed = pods
			return len(pods) == 0, nil
		})
	if err != nil {
		return fmt.Errorf("wait for Pods owned by PodSet %s/%s to disappear (last Pods=%s): %w",
			h.namespace, name, describePods(observed), err)
	}
	return nil
}

func describePods(pods []corev1.Pod) string {
	parts := make([]string, 0, len(pods))
	for i := range pods {
		index, _ := podIndex(&pods[i])
		parts = append(parts, fmt.Sprintf("%s(uid=%s,index=%d,ready=%t,deleting=%t,draining=%t)",
			pods[i].Name,
			pods[i].UID,
			index,
			podReady(&pods[i]),
			!pods[i].DeletionTimestamp.IsZero(),
			scaleInDrainStarted(&pods[i]),
		))
	}
	return strings.Join(parts, ",")
}

func retryablePodSetAPIError(err error) error {
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

func observeConsistently(
	parentCtx context.Context,
	interval, duration time.Duration,
	observe func(context.Context) error,
) error {
	err := wait.PollUntilContextTimeout(parentCtx, interval, duration, true,
		func(context.Context) (bool, error) {
			return false, observe(parentCtx)
		})
	if wait.Interrupted(err) && parentCtx.Err() == nil {
		return nil
	}
	return err
}

func newPodSet(namespace, name string, size, drainSeconds int32) *orchestrationv1alpha1.PodSet {
	labels := map[string]string{podSetFixtureLabel: name}
	podSet := &orchestrationv1alpha1.PodSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: orchestrationv1alpha1.GroupVersion.String(),
			Kind:       orchestrationv1alpha1.PodSetKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: size,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyAlways,
					Containers: []corev1.Container{{
						Name:            "worker",
						Image:           podSetE2EImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
					}},
				},
			},
			RecoveryPolicy: orchestrationv1alpha1.ReplaceUnhealthy,
		},
	}
	if drainSeconds > 0 {
		podSet.Spec.Drain = &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To(drainSeconds)}
	}
	return podSet
}

func podReady(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podIndex(pod *corev1.Pod) (int, bool) {
	if pod == nil {
		return 0, false
	}
	value, ok := pod.Labels[controllerconstants.PodGroupIndexLabelKey]
	if !ok {
		return 0, false
	}
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func scaleInDrainStarted(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	annotations := pod.Annotations
	return annotations[aibrixconst.PodDrainingAnnotationKey] == "true" &&
		annotations[aibrixconst.PodDrainStartTimeAnnotationKey] != "" &&
		annotations[aibrixconst.PodDrainReasonAnnotationKey] == aibrixconst.PodDrainReasonScaleIn &&
		annotations[aibrixconst.PodDrainTargetActionAnnotationKey] == aibrixconst.PodDrainTargetActionDelete
}

func drainStartTime(pod *corev1.Pod) (time.Time, error) {
	if pod == nil {
		return time.Time{}, fmt.Errorf("pod is nil")
	}
	raw := pod.Annotations[aibrixconst.PodDrainStartTimeAnnotationKey]
	if raw == "" {
		return time.Time{}, fmt.Errorf("pod %q has no drain start time", pod.Name)
	}
	start, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse pod %q drain start time %q: %w", pod.Name, raw, err)
	}
	return start, nil
}

func podUIDsByIndex(pods []corev1.Pod) (map[int]types.UID, error) {
	uids := make(map[int]types.UID, len(pods))
	for i := range pods {
		index, ok := podIndex(&pods[i])
		if !ok {
			return nil, fmt.Errorf("pod %q has an invalid %q label", pods[i].Name, controllerconstants.PodGroupIndexLabelKey)
		}
		if _, exists := uids[index]; exists {
			return nil, fmt.Errorf("multiple pods have index %d", index)
		}
		uids[index] = pods[i].UID
	}
	return uids, nil
}
