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

//nolint:lll // E2E polling keeps full client operations and latest-observation diagnostics together.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixclientset "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	orchestrationclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/typed/orchestration/v1alpha1"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
)

const (
	stormServiceVolcanoE2EEnv       = "AIBRIX_STORMSERVICE_VOLCANO_E2E"
	stormServiceE2EKeepOnFailureEnv = "AIBRIX_STORMSERVICE_E2E_KEEP_ON_FAILURE"
	stormServiceE2ENamespaceEnv     = "AIBRIX_E2E_NAMESPACE"
	stormServiceE2EDefaultNamespace = "default"
	stormServiceWorkerRoleName      = "worker"
	stormServiceWorkerContainerName = "worker"
	stormServiceInPlaceImageV1      = "aibrix/inplace-e2e:v1"
	stormServiceInPlaceImageV2      = "aibrix/inplace-e2e:v2"
	stormServiceMissingImage        = "aibrix/inplace-e2e:missing"
	stormServicePollInterval        = time.Second
	stormServicePollTimeout         = 3 * time.Minute
	stormServiceCleanupTimeout      = 3 * time.Minute
	stormServiceKeepLogTimeout      = 10 * time.Second
	stormServiceVolcanoDefaultQueue = "default"
)

var (
	roleSetGVR = schema.GroupVersionResource{
		Group: "orchestration.aibrix.ai", Version: "v1alpha1", Resource: "rolesets",
	}
	podSetGVR = schema.GroupVersionResource{
		Group: "orchestration.aibrix.ai", Version: "v1alpha1", Resource: "podsets",
	}
	volcanoPodGroupGVR = schema.GroupVersionResource{
		Group: "scheduling.volcano.sh", Version: "v1beta1", Resource: "podgroups",
	}
)

// stormServiceHarness keeps the clients and namespace needed for cleanup together.
// Cleanup cannot derive the original StormService UID after foreground deletion, and
// the UID is needed to distinguish generated ControllerRevisions from same-name data.
type stormServiceHarness struct {
	namespace     string
	kubeClient    kubernetes.Interface
	stormServices orchestrationclient.StormServiceInterface
	dynamicClient dynamic.Interface
	roleSetNames  map[string]map[string]struct{}
}

func stormServiceNamespace() string {
	if namespace := strings.TrimSpace(os.Getenv(stormServiceE2ENamespaceEnv)); namespace != "" {
		return namespace
	}
	return stormServiceE2EDefaultNamespace
}

func newStormServiceHarness(t *testing.T, namespace string) *stormServiceHarness {
	t.Helper()
	kubeClient, stormServices, dynamicClient := stormServiceClients(t, namespace)
	return &stormServiceHarness{
		namespace:     namespace,
		kubeClient:    kubeClient,
		stormServices: stormServices,
		dynamicClient: dynamicClient,
		roleSetNames:  make(map[string]map[string]struct{}),
	}
}

// stormServiceClients creates direct clients without starting shared informers.
// Live-controller tests only poll API state, so framework.InitializeClient's
// informer setup is unnecessary here.
func stormServiceClients(
	t *testing.T,
	namespace string,
) (kubernetes.Interface, orchestrationclient.StormServiceInterface, dynamic.Interface) {
	t.Helper()

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Fatalf("build Kubernetes client configuration: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	aibrixClient, err := aibrixclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("create AIBrix client: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatalf("create dynamic client: %v", err)
	}

	return kubeClient, aibrixClient.OrchestrationV1alpha1().StormServices(namespace), dynamicClient
}

func newStormService(namespace, name, image string) *orchestrationv1alpha1.StormService {
	labels := map[string]string{"app": name}
	return &orchestrationv1alpha1.StormService{
		TypeMeta: metav1.TypeMeta{
			APIVersion: orchestrationv1alpha1.GroupVersion.String(),
			Kind:       orchestrationv1alpha1.StormServiceKind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To(int32(1)),
			Mode:     orchestrationv1alpha1.StormServicePooledMode,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					UpdateStrategy: orchestrationv1alpha1.ParallelRoleSetUpdateStrategyType,
					Roles: []orchestrationv1alpha1.RoleSpec{{
						Name:     stormServiceWorkerRoleName,
						Replicas: ptr.To(int32(1)),
						UpdateStrategy: orchestrationv1alpha1.RoleUpdateStrategy{
							Type: orchestrationv1alpha1.InPlaceIfPossibleRoleUpdateStrategyType,
						},
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{
								Name:            stormServiceWorkerContainerName,
								Image:           image,
								ImagePullPolicy: corev1.PullIfNotPresent,
							}}},
						},
					}},
				},
			},
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type: orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType,
			},
		},
	}
}

func newReplicaLifecycleStormService(namespace, name string, replicas int32) *orchestrationv1alpha1.StormService {
	stormService := newStormService(namespace, name, stormServiceInPlaceImageV1)
	maxSurge := intstr.FromInt32(0)
	maxUnavailable := intstr.FromInt32(1)
	stormService.Spec.Replicas = ptr.To(replicas)
	stormService.Spec.Mode = orchestrationv1alpha1.StormServiceReplicaMode
	stormService.Spec.UpdateStrategy = orchestrationv1alpha1.StormServiceUpdateStrategy{
		Type:           orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
		MaxSurge:       &maxSurge,
		MaxUnavailable: &maxUnavailable,
	}
	stormService.Spec.Template.Spec.Roles[0].UpdateStrategy.Type = orchestrationv1alpha1.RecreateRoleUpdateStrategyType
	return stormService
}

func newUpdateStormService(namespace, name string) *orchestrationv1alpha1.StormService {
	return newStormService(namespace, name, stormServiceInPlaceImageV1)
}

func newDeadlineStormService(namespace, name string, deadlineSeconds int32) *orchestrationv1alpha1.StormService {
	stormService := newReplicaLifecycleStormService(namespace, name, 1)
	stormService.Spec.ProgressDeadlineSeconds = ptr.To(deadlineSeconds)
	return stormService
}

func newVolcanoStormService(
	namespace, name string,
	impossible bool,
	eligibleNodes int,
) *orchestrationv1alpha1.StormService {
	stormService := newStormService(namespace, name, stormServiceInPlaceImageV1)
	volcanoStrategy := &orchestrationv1alpha1.VolcanoSchedulingStrategySpec{Queue: stormServiceVolcanoDefaultQueue}
	if impossible {
		if eligibleNodes < 1 {
			eligibleNodes = 1
		}
		members := int32(eligibleNodes + 1)
		memberLabel := "stormservice-volcano-member"
		role := &stormService.Spec.Template.Spec.Roles[0]
		role.Replicas = ptr.To(members)
		role.Template.Labels = map[string]string{memberLabel: name}
		role.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("10m"),
		}
		role.Template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{memberLabel: name}},
				TopologyKey:   corev1.LabelHostname,
			}},
		}}
		volcanoStrategy.MinMember = members
		volcanoStrategy.MinTaskMember = map[string]int32{stormServiceWorkerRoleName: members}
	} else {
		roles := make([]orchestrationv1alpha1.RoleSpec, 0, 2)
		for _, roleName := range []string{"prefill", "decode"} {
			role := stormService.Spec.Template.Spec.Roles[0].DeepCopy()
			role.Name = roleName
			role.Template.Spec.Containers[0].Name = roleName
			roles = append(roles, *role)
		}
		stormService.Spec.Template.Spec.Roles = roles
		volcanoStrategy.MinMember = 2
		volcanoStrategy.MinTaskMember = map[string]int32{"prefill": 1, "decode": 1}
	}
	stormService.Spec.Template.Spec.SchedulingStrategy = &orchestrationv1alpha1.SchedulingStrategy{
		VolcanoSchedulingStrategy: volcanoStrategy,
	}
	return stormService
}

func eligibleNodeCount(nodes []corev1.Node) int {
	eligible := 0
	for i := range nodes {
		node := &nodes[i]
		if node.Spec.Unschedulable || !nodeReady(node) || hasUntoleratedSchedulingTaint(node) {
			continue
		}
		eligible++
	}
	return eligible
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hasUntoleratedSchedulingTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

func waitForStormServiceState(
	ctx context.Context,
	stormServices orchestrationclient.StormServiceInterface,
	name string,
	predicate func(*orchestrationv1alpha1.StormService) bool,
) (*orchestrationv1alpha1.StormService, error) {
	var latest string
	var observed *orchestrationv1alpha1.StormService
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServicePollTimeout, true, func(ctx context.Context) (bool, error) {
		stormService, err := stormServices.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, retryPollError(err, true, &latest)
		}
		observed = stormService
		latest = describeStormService(stormService)
		return predicate(stormService), nil
	})
	if err != nil {
		return observed, fmt.Errorf("wait for StormService %q state: %w; latest observation: %s", name, err, latest)
	}
	return observed, nil
}

func waitForRoleSets(
	ctx context.Context,
	dynamicClient dynamic.Interface,
	namespace, stormServiceName string,
	count int,
) ([]unstructured.Unstructured, error) {
	return waitForRoleSetsObserved(ctx, dynamicClient, namespace, stormServiceName, count, nil)
}

func waitForRoleSetsObserved(
	ctx context.Context,
	dynamicClient dynamic.Interface,
	namespace, stormServiceName string,
	count int,
	observe func([]unstructured.Unstructured),
) ([]unstructured.Unstructured, error) {
	selector := stormServiceSelector(stormServiceName)
	var latest string
	var observed []unstructured.Unstructured
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServicePollTimeout, true, func(ctx context.Context) (bool, error) {
		roleSets, err := dynamicClient.Resource(roleSetGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, retryPollError(err, false, &latest)
		}
		observed = roleSets.Items
		if observe != nil {
			observe(roleSets.Items)
		}
		latest = describeUnstructuredList(roleSets.Items)
		return len(roleSets.Items) == count, nil
	})
	if err != nil {
		return observed, fmt.Errorf("wait for %d RoleSets for StormService %q: %w; latest observation: %s", count, stormServiceName, err, latest)
	}
	return observed, nil
}

// waitForRoleSets records every observed generated RoleSet name. Volcano
// PodGroups are named after RoleSets, so these identities must outlive a
// preceding StormService deletion for cleanup to find orphaned PodGroups.
func (h *stormServiceHarness) waitForRoleSets(
	ctx context.Context,
	stormServiceName string,
	count int,
) ([]unstructured.Unstructured, error) {
	return waitForRoleSetsObserved(ctx, h.dynamicClient, h.namespace, stormServiceName, count, func(roleSets []unstructured.Unstructured) {
		h.recordRoleSets(stormServiceName, roleSets)
	})
}

func (h *stormServiceHarness) recordRoleSets(stormServiceName string, roleSets []unstructured.Unstructured) {
	if h.roleSetNames == nil {
		h.roleSetNames = make(map[string]map[string]struct{})
	}
	if h.roleSetNames[stormServiceName] == nil {
		h.roleSetNames[stormServiceName] = make(map[string]struct{})
	}
	for i := range roleSets {
		if name := roleSets[i].GetName(); name != "" {
			h.roleSetNames[stormServiceName][name] = struct{}{}
		}
	}
}

func (h *stormServiceHarness) recordedRoleSetNames(stormServiceName string) map[string]struct{} {
	recorded := make(map[string]struct{})
	for name := range h.roleSetNames[stormServiceName] {
		recorded[name] = struct{}{}
	}
	return recorded
}

func waitForPods(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace, stormServiceName string,
	count int,
	ready bool,
) ([]corev1.Pod, error) {
	selector := stormServiceSelector(stormServiceName)
	var latest string
	var observed []corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServicePollTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, retryPollError(err, false, &latest)
		}
		observed = pods.Items
		latest = describePods(pods.Items)
		if len(pods.Items) != count {
			return false, nil
		}
		if !ready {
			return true, nil
		}
		for i := range pods.Items {
			if !podReady(&pods.Items[i]) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return observed, fmt.Errorf("wait for %d pods for StormService %q (ready=%t): %w; latest observation: %s", count, stormServiceName, ready, err, latest)
	}
	return observed, nil
}

func waitForOwnedService(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace, name string,
	ownerUID types.UID,
) (*corev1.Service, error) {
	var latest string
	var observed *corev1.Service
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServicePollTimeout, true, func(ctx context.Context) (bool, error) {
		service, err := kubeClient.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, retryPollError(err, true, &latest)
		}
		observed = service
		latest = describeService(service)
		return hasOwnerUID(service.OwnerReferences, ownerUID), nil
	})
	if err != nil {
		return observed, fmt.Errorf("wait for Service %s/%s owned by %q: %w; latest observation: %s", namespace, name, ownerUID, err, latest)
	}
	return observed, nil
}

func updateStormService(
	ctx context.Context,
	stormServices orchestrationclient.StormServiceInterface,
	name string,
	mutate func(*orchestrationv1alpha1.StormService),
) (*orchestrationv1alpha1.StormService, error) {
	var updated *orchestrationv1alpha1.StormService
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		stormService, err := stormServices.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		mutate(stormService)
		updated, err = stormServices.Update(ctx, stormService, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return updated, fmt.Errorf("update StormService %q: %w", name, err)
	}
	return updated, nil
}

// requireConsistentFor runs check through the complete duration. The timer
// controls only the observation window; checks retain the caller's longer
// context so client-side rate limiting cannot fail a final boundary sample.
func requireConsistentFor(
	ctx context.Context,
	interval, duration time.Duration,
	check func(context.Context) error,
) error {
	if err := check(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
			if err := check(ctx); err != nil {
				return err
			}
		}
	}
}

func pausedUpdateRemainsUnchanged(
	roleSets []unstructured.Unstructured,
	pods []corev1.Pod,
	roleSetUID, podUID types.UID,
	wantImage string,
) bool {
	if len(roleSets) != 1 || len(pods) != 1 || roleSets[0].GetUID() != roleSetUID || pods[0].UID != podUID {
		return false
	}
	image, found := podContainerImage(&pods[0], stormServiceWorkerContainerName)
	return found && image == wantImage
}

func podContainerImage(pod *corev1.Pod, containerName string) (string, bool) {
	if pod == nil {
		return "", false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			return container.Image, true
		}
	}
	return "", false
}

func (h *stormServiceHarness) observeRoleSetsAndPods(
	ctx context.Context,
	stormServiceName string,
) ([]unstructured.Unstructured, []corev1.Pod, error) {
	roleSets, err := h.dynamicClient.Resource(roleSetGVR).Namespace(h.namespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: stormServiceSelector(stormServiceName)},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list RoleSets: %w", err)
	}
	h.recordRoleSets(stormServiceName, roleSets.Items)
	pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: stormServiceSelector(stormServiceName)},
	)
	if err != nil {
		return roleSets.Items, nil, fmt.Errorf("list Pods: %w", err)
	}
	return roleSets.Items, pods.Items, nil
}

func (h *stormServiceHarness) waitForSingleRoleSetAndPodState(
	ctx context.Context,
	stormServiceName string,
	predicate func(*unstructured.Unstructured, *corev1.Pod) bool,
) (*unstructured.Unstructured, *corev1.Pod, error) {
	var observedRoleSet *unstructured.Unstructured
	var observedPod *corev1.Pod
	latest := "no observation"
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServicePollTimeout, true, func(ctx context.Context) (bool, error) {
		roleSets, pods, err := h.observeRoleSetsAndPods(ctx, stormServiceName)
		if err != nil {
			return false, retryPollError(err, false, &latest)
		}
		latest = fmt.Sprintf("roleSets=%s pods=%s", describeUnstructuredList(roleSets), describePods(pods))
		if len(roleSets) != 1 || len(pods) != 1 {
			return false, nil
		}
		observedRoleSet = roleSets[0].DeepCopy()
		observedPod = pods[0].DeepCopy()
		return predicate(observedRoleSet, observedPod), nil
	})
	if err != nil {
		return observedRoleSet, observedPod, fmt.Errorf("wait for one RoleSet and Pod for StormService %q: %w; latest observation: %s", stormServiceName, err, latest)
	}
	return observedRoleSet, observedPod, nil
}

func podCompletedInPlaceUpdate(pod *corev1.Pod, originalUID types.UID, originalHash string) bool {
	if pod == nil || pod.UID != originalUID || !podReady(pod) {
		return false
	}
	image, found := podContainerImage(pod, stormServiceWorkerContainerName)
	if !found || image != stormServiceInPlaceImageV2 || pod.Labels[controllerconstants.RoleTemplateHashLabelKey] == "" || pod.Labels[controllerconstants.RoleTemplateHashLabelKey] == originalHash {
		return false
	}
	for _, key := range []string{
		controllerconstants.RoleInPlaceUpdateTargetHashAnnotationKey,
		controllerconstants.RoleInPlaceUpdateStateAnnotationKey,
		controllerconstants.RoleInPlaceUpdatePendingReasonAnnotationKey,
	} {
		if _, found := pod.Annotations[key]; found {
			return false
		}
	}
	return true
}

func podCompletedFallbackUpdate(pod *corev1.Pod, originalUID types.UID) bool {
	if pod == nil || pod.UID == originalUID || !podReady(pod) {
		return false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != stormServiceWorkerContainerName {
			continue
		}
		_, targetPresent := pod.Annotations[controllerconstants.RoleInPlaceUpdateTargetHashAnnotationKey]
		return container.Image == stormServiceInPlaceImageV2 &&
			reflect.DeepEqual(container.Command, []string{"sh", "-c", "sleep 3600"}) &&
			!targetPresent
	}
	return false
}

func condition(conditions orchestrationv1alpha1.Conditions, conditionType orchestrationv1alpha1.ConditionType) *orchestrationv1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func (h *stormServiceHarness) cleanupStormService(t *testing.T, name string, expectPodGroup bool) {
	t.Helper()
	if t.Failed() && strings.EqualFold(strings.TrimSpace(os.Getenv(stormServiceE2EKeepOnFailureEnv)), "true") {
		t.Logf("preserving StormService e2e resources for %s/%s", h.namespace, name)
		h.logRetainedStormServiceResources(t, name, expectPodGroup)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), stormServiceCleanupTimeout)
	defer cancel()
	if err := h.cleanupStormServiceResources(ctx, name, expectPodGroup); err != nil {
		t.Errorf("cleanup StormService %s/%s: %v", h.namespace, name, err)
	}
}

func (h *stormServiceHarness) cleanupStormServiceResources(ctx context.Context, name string, expectPodGroup bool) error {
	var cleanupErrors []error

	stormService, err := h.stormServices.Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("get StormService identity: %w", err))
	}
	var ownerUID types.UID
	if stormService != nil {
		ownerUID = stormService.UID
	}

	roleSets, err := h.dynamicClient.Resource(roleSetGVR).Namespace(h.namespace).List(ctx, metav1.ListOptions{LabelSelector: stormServiceSelector(name)})
	if err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("list RoleSet identities: %w", err))
	} else {
		h.recordRoleSets(name, roleSets.Items)
	}

	foreground := metav1.DeletePropagationForeground
	if err := h.stormServices.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("foreground delete StormService: %w", err))
	}

	if err := waitForStormServiceDeleted(ctx, h.stormServices, name); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if _, err := h.waitForRoleSets(ctx, name, 0); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := waitForNoPodSets(ctx, h.dynamicClient, h.namespace, name); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if _, err := waitForPods(ctx, h.kubeClient, h.namespace, name, 0, false); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := waitForControllerRevisionsDeleted(ctx, h.kubeClient, h.namespace, name, ownerUID); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := waitForServiceDeleted(ctx, h.kubeClient, h.namespace, name, ownerUID); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if expectPodGroup {
		recordedRoleSetNames := h.recordedRoleSetNames(name)
		roleSetNames := make([]string, 0, len(recordedRoleSetNames))
		for roleSetName := range recordedRoleSetNames {
			roleSetNames = append(roleSetNames, roleSetName)
		}
		if err := waitForPodGroupsDeleted(ctx, h.dynamicClient, h.namespace, roleSetNames); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

// logRetainedStormServiceResources captures cleanup targets after a failed test
// without changing its outcome. Each API request is bounded and failures are
// logged rather than reported through testing.T's failure methods.
func (h *stormServiceHarness) logRetainedStormServiceResources(t *testing.T, name string, expectPodGroup bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stormServiceKeepLogTimeout)
	defer cancel()

	roleSets, err := h.dynamicClient.Resource(roleSetGVR).Namespace(h.namespace).List(ctx, metav1.ListOptions{LabelSelector: stormServiceSelector(name)})
	if err != nil {
		t.Logf("list retained RoleSets for StormService %s/%s: %v", h.namespace, name, err)
	} else {
		h.recordRoleSets(name, roleSets.Items)
		t.Logf("retained current RoleSets for StormService %s/%s: %s", h.namespace, name, describeUnstructuredList(roleSets.Items))
	}
	recordedRoleSetNames := h.recordedRoleSetNames(name)
	t.Logf("retained recorded RoleSet names for StormService %s/%s: %v", h.namespace, name, roleSetNamesSlice(recordedRoleSetNames))

	podSets, err := h.dynamicClient.Resource(podSetGVR).Namespace(h.namespace).List(ctx, metav1.ListOptions{LabelSelector: stormServiceSelector(name)})
	if err != nil {
		t.Logf("list retained PodSets for StormService %s/%s: %v", h.namespace, name, err)
	} else {
		t.Logf("retained PodSets for StormService %s/%s: %s", h.namespace, name, describeUnstructuredList(podSets.Items))
	}

	pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{LabelSelector: stormServiceSelector(name)})
	if err != nil {
		t.Logf("list retained pods for StormService %s/%s: %v", h.namespace, name, err)
	} else {
		t.Logf("retained pods for StormService %s/%s: %s", h.namespace, name, describePods(pods.Items))
	}

	revisions, err := h.kubeClient.AppsV1().ControllerRevisions(h.namespace).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("name=%s", name)})
	if err != nil {
		t.Logf("list retained ControllerRevisions for StormService %s/%s: %v", h.namespace, name, err)
	} else {
		t.Logf("retained ControllerRevisions for StormService %s/%s: %s", h.namespace, name, describeControllerRevisions(revisions.Items))
	}

	service, err := h.kubeClient.CoreV1().Services(h.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Logf("get retained Service %s/%s: %v", h.namespace, name, err)
	} else {
		t.Logf("retained Service %s/%s: %s", h.namespace, name, describeService(service))
	}

	if !expectPodGroup {
		return
	}
	for _, roleSetName := range roleSetNamesSlice(recordedRoleSetNames) {
		podGroups, err := h.dynamicClient.Resource(volcanoPodGroupGVR).Namespace(h.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", controllerconstants.RoleSetNameLabelKey, roleSetName),
		})
		if err != nil {
			t.Logf("list retained Volcano PodGroups for RoleSet %s: %v", roleSetName, err)
			continue
		}
		t.Logf("retained Volcano PodGroups for RoleSet %s: %s", roleSetName, describeUnstructuredList(podGroups.Items))
	}
}

func stormServiceSelector(name string) string {
	return fmt.Sprintf("%s=%s", controllerconstants.StormServiceNameLabelKey, name)
}

func roleSetNamesSlice(roleSetNames map[string]struct{}) []string {
	names := make([]string, 0, len(roleSetNames))
	for name := range roleSetNames {
		names = append(names, name)
	}
	return names
}

// retryPollError preserves the latest API observation while allowing only
// expected appearance NotFounds and transient API failures to be retried.
func retryPollError(err error, retryNotFound bool, latest *string) error {
	*latest = fmt.Sprintf("API error: %v", err)
	if retryNotFound && apierrors.IsNotFound(err) {
		return nil
	}
	if apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsInternalError(err) {
		return nil
	}
	return err
}

func waitForStormServiceDeleted(ctx context.Context, stormServices orchestrationclient.StormServiceInterface, name string) error {
	var latest string
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServiceCleanupTimeout, true, func(ctx context.Context) (bool, error) {
		stormService, err := stormServices.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, retryPollError(err, false, &latest)
		}
		latest = describeStormService(stormService)
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("wait for StormService %q deletion: %w; latest observation: %s", name, err, latest)
	}
	return nil
}

func waitForNoPodSets(ctx context.Context, dynamicClient dynamic.Interface, namespace, stormServiceName string) error {
	selector := stormServiceSelector(stormServiceName)
	var latest string
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServiceCleanupTimeout, true, func(ctx context.Context) (bool, error) {
		podSets, err := dynamicClient.Resource(podSetGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, retryPollError(err, false, &latest)
		}
		latest = describeUnstructuredList(podSets.Items)
		return len(podSets.Items) == 0, nil
	})
	if err != nil {
		return fmt.Errorf("wait for PodSets for StormService %q deletion: %w; latest observation: %s", stormServiceName, err, latest)
	}
	return nil
}

func waitForControllerRevisionsDeleted(ctx context.Context, kubeClient kubernetes.Interface, namespace, name string, ownerUID types.UID) error {
	var latest string
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServiceCleanupTimeout, true, func(ctx context.Context) (bool, error) {
		revisions, err := kubeClient.AppsV1().ControllerRevisions(namespace).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("name=%s", name)})
		if err != nil {
			return false, retryPollError(err, false, &latest)
		}
		owned := controllerRevisionsWithOwner(revisions.Items, ownerUID)
		latest = describeControllerRevisions(owned)
		return len(owned) == 0, nil
	})
	if err != nil {
		return fmt.Errorf("wait for ControllerRevisions for StormService %q deletion: %w; latest observation: %s", name, err, latest)
	}
	return nil
}

func waitForServiceDeleted(ctx context.Context, kubeClient kubernetes.Interface, namespace, name string, ownerUID types.UID) error {
	var latest string
	err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServiceCleanupTimeout, true, func(ctx context.Context) (bool, error) {
		service, err := kubeClient.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, retryPollError(err, false, &latest)
		}
		if ownerUID != "" {
			owned := hasOwnerUID(service.OwnerReferences, ownerUID)
			latest = fmt.Sprintf("ownedByStormService=%t %s", owned, describeService(service))
			return !owned, nil
		}
		owned := hasStormServiceOwner(service.OwnerReferences, name)
		latest = fmt.Sprintf("ownedByStormService=%t %s", owned, describeService(service))
		return !owned, nil
	})
	if err != nil {
		return fmt.Errorf("wait for Service %s/%s deletion: %w; latest observation: %s", namespace, name, err, latest)
	}
	return nil
}

func waitForPodGroupsDeleted(ctx context.Context, dynamicClient dynamic.Interface, namespace string, roleSetNames []string) error {
	for _, roleSetName := range roleSetNames {
		selector := fmt.Sprintf("%s=%s", controllerconstants.RoleSetNameLabelKey, roleSetName)
		var latest string
		err := wait.PollUntilContextTimeout(ctx, stormServicePollInterval, stormServiceCleanupTimeout, true, func(ctx context.Context) (bool, error) {
			podGroups, err := dynamicClient.Resource(volcanoPodGroupGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return false, retryPollError(err, false, &latest)
			}
			latest = describeUnstructuredList(podGroups.Items)
			return len(podGroups.Items) == 0, nil
		})
		if err != nil {
			return fmt.Errorf("wait for Volcano PodGroups for RoleSet %q deletion: %w; latest observation: %s", roleSetName, err, latest)
		}
	}
	return nil
}

func hasOwnerUID(ownerReferences []metav1.OwnerReference, ownerUID types.UID) bool {
	for _, ownerReference := range ownerReferences {
		if ownerReference.UID == ownerUID {
			return true
		}
	}
	return false
}

func hasStormServiceOwner(ownerReferences []metav1.OwnerReference, name string) bool {
	for _, ownerReference := range ownerReferences {
		if ownerReference.APIVersion == orchestrationv1alpha1.GroupVersion.String() &&
			ownerReference.Kind == orchestrationv1alpha1.StormServiceKind &&
			ownerReference.Name == name {
			return true
		}
	}
	return false
}

func controllerRevisionsWithOwner(revisions []appsv1.ControllerRevision, ownerUID types.UID) []appsv1.ControllerRevision {
	if ownerUID == "" {
		return revisions
	}
	owned := make([]appsv1.ControllerRevision, 0, len(revisions))
	for i := range revisions {
		if hasOwnerUID(revisions[i].OwnerReferences, ownerUID) {
			owned = append(owned, revisions[i])
		}
	}
	return owned
}

func podReady(pod *corev1.Pod) bool {
	for _, podCondition := range pod.Status.Conditions {
		if podCondition.Type == corev1.PodReady {
			return podCondition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func describeStormService(stormService *orchestrationv1alpha1.StormService) string {
	return fmt.Sprintf("generation=%d observedGeneration=%d replicas=%d readyReplicas=%d conditions=%v", stormService.Generation, stormService.Status.ObservedGeneration, stormService.Status.Replicas, stormService.Status.ReadyReplicas, stormService.Status.Conditions)
}

func describeUnstructuredList(items []unstructured.Unstructured) string {
	names := make([]string, 0, len(items))
	for i := range items {
		names = append(names, fmt.Sprintf("%s(uid=%s)", items[i].GetName(), items[i].GetUID()))
	}
	return fmt.Sprintf("count=%d items=%v", len(items), names)
}

func describePods(pods []corev1.Pod) string {
	descriptions := make([]string, 0, len(pods))
	for i := range pods {
		descriptions = append(descriptions, fmt.Sprintf("%s(uid=%s ready=%t phase=%s)", pods[i].Name, pods[i].UID, podReady(&pods[i]), pods[i].Status.Phase))
	}
	return fmt.Sprintf("count=%d pods=%v", len(pods), descriptions)
}

func describeService(service *corev1.Service) string {
	return fmt.Sprintf("uid=%s owners=%v clusterIP=%s", service.UID, service.OwnerReferences, service.Spec.ClusterIP)
}

func describeControllerRevisions(revisions []appsv1.ControllerRevision) string {
	descriptions := make([]string, 0, len(revisions))
	for i := range revisions {
		descriptions = append(descriptions, fmt.Sprintf("%s(uid=%s revision=%d)", revisions[i].Name, revisions[i].UID, revisions[i].Revision))
	}
	return fmt.Sprintf("count=%d revisions=%v", len(revisions), descriptions)
}
