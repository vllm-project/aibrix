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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	ctrlutil "github.com/vllm-project/aibrix/pkg/controller/util"
	podutil "github.com/vllm-project/aibrix/pkg/utils"
)

const defaultServiceAccountName = "default"

type inPlaceUpdateState struct {
	LastContainerStatuses map[string]inPlaceUpdateContainerStatus `json:"lastContainerStatuses,omitempty"`
}

type inPlaceUpdateContainerStatus struct {
	ImageID string `json:"imageID,omitempty"`
}

type inPlaceUpdatePendingStatus struct {
	Reason         string
	Container      string
	DesiredImage   string
	RuntimeImage   string
	OldImageID     string
	CurrentImageID string
}

// roleUpdateStrategyTypeOrDefault preserves the existing recreate behavior for
// roles that were created before the strategy field existed or omit it.
func roleUpdateStrategyTypeOrDefault(role *orchestrationv1alpha1.RoleSpec) orchestrationv1alpha1.RoleUpdateStrategyType {
	if role.UpdateStrategy.Type == "" {
		return orchestrationv1alpha1.RecreateRoleUpdateStrategyType
	}
	return role.UpdateStrategy.Type
}

// buildRenderedPod renders the desired pod exactly as the RoleSet controller
// would create it, including StormService-specific labels and annotations.
func buildRenderedPod(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, roleIndex *int) (*corev1.Pod, error) {
	pod, err := ctrlutil.GetPodFromTemplate(&role.Template, roleSet, metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)))
	if err != nil {
		return nil, err
	}
	renderStormServicePod(roleSet, role, pod, roleIndex)
	return pod, nil
}

// canInPlaceUpdatePod allows an in-place rollout only when the rendered desired
// pod differs from the current pod by container images and controller-managed
// rollout metadata. Any other spec drift must fall back to pod replacement.
func canInPlaceUpdatePod(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, pod *corev1.Pod, hashFunc func(*corev1.PodTemplateSpec, *int32) string) (bool, string, error) {
	var roleIndex *int
	if pod.Annotations != nil {
		if rawIndex, ok := pod.Annotations[constants.RoleReplicaIndexAnnotationKey]; ok {
			index, err := strconv.Atoi(rawIndex)
			if err != nil {
				return false, fmt.Sprintf("invalid role replica index %q", rawIndex), nil
			}
			roleIndex = &index
		}
	}

	desired, err := buildRenderedPod(roleSet, role, roleIndex)
	if err != nil {
		return false, "", err
	}

	if containersImagesDiffer(pod.Spec.InitContainers, role.Template.Spec.InitContainers) {
		return false, "init container image changes require pod recreation", nil
	}

	currentComparable := normalizePodForInPlaceComparison(pod.DeepCopy(), desired)
	desiredComparable := normalizePodForInPlaceComparison(desired, desired)
	if !apiequality.Semantic.DeepEqual(currentComparable, desiredComparable) {
		return false, "non-image pod fields changed", nil
	}

	if !podImagesDiffer(pod, role) {
		desiredHash := hashFunc(&role.Template, nil)
		if pod.Labels[constants.RoleTemplateHashLabelKey] == desiredHash {
			return false, "pod is already updated", nil
		}
		return false, "no container image changes found", nil
	}
	return true, "", nil
}

// normalizePodForInPlaceComparison removes fields that are expected to differ
// after admission, scheduling, or RoleSet bookkeeping so DeepEqual can focus on
// user-controlled pod fields that would make an in-place update unsafe.
func normalizePodForInPlaceComparison(pod *corev1.Pod, desired *corev1.Pod) *corev1.Pod {
	pod.TypeMeta = metav1.TypeMeta{}
	normalizePodObjectMetaForInPlaceComparison(&pod.ObjectMeta)
	delete(pod.Labels, constants.RoleTemplateHashLabelKey)
	delete(pod.Labels, constants.RoleRevisionLabelKey)
	delete(pod.Labels, constants.RoleRevisionNameLabelKey)
	delete(pod.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)

	pod.Spec.Hostname = ""
	normalizePodSpecRuntimeFields(&pod.Spec, &desired.Spec)
	normalizeContainerImages(pod.Spec.InitContainers, desired.Spec.InitContainers)
	normalizeContainerImagesAndHashEnv(pod.Spec.Containers, desired.Spec.Containers)
	pod.Status = corev1.PodStatus{}
	return pod
}

func normalizePodObjectMetaForInPlaceComparison(meta *metav1.ObjectMeta) {
	meta.Name = ""
	meta.Namespace = ""
	meta.UID = ""
	meta.ResourceVersion = ""
	meta.Generation = 0
	meta.CreationTimestamp = metav1.Time{}
	meta.DeletionTimestamp = nil
	meta.DeletionGracePeriodSeconds = nil
	meta.ManagedFields = nil
}

// normalizePodSpecRuntimeFields strips Kubernetes defaults that are injected on
// live pods but are absent from the desired template. These defaults should not
// force an otherwise image-only update to recreate the pod.
func normalizePodSpecRuntimeFields(spec, desired *corev1.PodSpec) {
	if desired.NodeName == "" {
		spec.NodeName = ""
	}
	if desired.SchedulerName == "" && spec.SchedulerName == corev1.DefaultSchedulerName {
		spec.SchedulerName = ""
	}
	if desired.RestartPolicy == "" && spec.RestartPolicy == corev1.RestartPolicyAlways {
		spec.RestartPolicy = ""
	}
	if desired.DNSPolicy == "" && spec.DNSPolicy == corev1.DNSClusterFirst {
		spec.DNSPolicy = ""
	}
	if desired.TerminationGracePeriodSeconds == nil && spec.TerminationGracePeriodSeconds != nil && *spec.TerminationGracePeriodSeconds == 30 {
		spec.TerminationGracePeriodSeconds = nil
	}
	if desired.EnableServiceLinks == nil && spec.EnableServiceLinks != nil && *spec.EnableServiceLinks {
		spec.EnableServiceLinks = nil
	}
	if desired.ServiceAccountName == "" && spec.ServiceAccountName == defaultServiceAccountName {
		spec.ServiceAccountName = ""
	}
	if desired.DeprecatedServiceAccount == "" && spec.DeprecatedServiceAccount == defaultServiceAccountName {
		spec.DeprecatedServiceAccount = ""
	}
	if desired.SecurityContext == nil && apiequality.Semantic.DeepEqual(spec.SecurityContext, &corev1.PodSecurityContext{}) {
		spec.SecurityContext = nil
	}
	if desired.Priority == nil && spec.Priority != nil && *spec.Priority == 0 {
		spec.Priority = nil
	}
	if desired.PreemptionPolicy == nil && spec.PreemptionPolicy != nil && *spec.PreemptionPolicy == corev1.PreemptLowerPriority {
		spec.PreemptionPolicy = nil
	}
	spec.Tolerations = removeDefaultNoExecuteTolerations(spec.Tolerations)
	removedVolumes := removeDefaultServiceAccountVolumes(spec)
	removeVolumeMounts(spec.InitContainers, removedVolumes)
	removeVolumeMounts(spec.Containers, removedVolumes)
}

// normalizeContainerImages copies desired images into the comparable pod. Image
// changes are the only workload spec changes allowed during in-place rollout.
func normalizeContainerImages(containers, desired []corev1.Container) {
	desiredByName := make(map[string]corev1.Container, len(desired))
	for i := range desired {
		desiredByName[desired[i].Name] = desired[i]
	}
	for i := range containers {
		if desiredContainer, ok := desiredByName[containers[i].Name]; ok {
			containers[i].Image = desiredContainer.Image
			normalizeContainerRuntimeDefaults(&containers[i], &desiredContainer)
		} else {
			normalizeContainerRuntimeDefaults(&containers[i], nil)
		}
	}
}

// normalizeContainerImagesAndHashEnv also clears the injected template hash env
// value, which is controller-managed metadata rather than user pod spec intent.
func normalizeContainerImagesAndHashEnv(containers, desired []corev1.Container) {
	desiredByName := make(map[string]corev1.Container, len(desired))
	for i := range desired {
		desiredByName[desired[i].Name] = desired[i]
	}
	for i := range containers {
		if desiredContainer, ok := desiredByName[containers[i].Name]; ok {
			containers[i].Image = desiredContainer.Image
			normalizeContainerRuntimeDefaults(&containers[i], &desiredContainer)
		} else {
			normalizeContainerRuntimeDefaults(&containers[i], nil)
		}
		for j := range containers[i].Env {
			if containers[i].Env[j].Name == constants.RoleTemplateHashEnvKey {
				containers[i].Env[j].Value = ""
			}
		}
	}
}

// normalizeContainerRuntimeDefaults removes kubelet defaults from live
// containers so they do not look like user-authored template changes.
func normalizeContainerRuntimeDefaults(container *corev1.Container, desired *corev1.Container) {
	if desired != nil && desired.ImagePullPolicy == "" && container.ImagePullPolicy == defaultImagePullPolicy(desired.Image) {
		container.ImagePullPolicy = ""
	}
	if container.TerminationMessagePath == corev1.TerminationMessagePathDefault {
		container.TerminationMessagePath = ""
	}
	if container.TerminationMessagePolicy == corev1.TerminationMessageReadFile {
		container.TerminationMessagePolicy = ""
	}
}

// defaultImagePullPolicy mirrors Kubernetes defaulting for image pull policy.
func defaultImagePullPolicy(image string) corev1.PullPolicy {
	if strings.Contains(image, "@") {
		return corev1.PullIfNotPresent
	}
	if !imageHasTagOrDigest(image) || imageTag(image) == "latest" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func imageTag(image string) string {
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon <= lastSlash {
		return ""
	}
	return image[lastColon+1:]
}

func removeDefaultNoExecuteTolerations(tolerations []corev1.Toleration) []corev1.Toleration {
	var out []corev1.Toleration
	for _, toleration := range tolerations {
		if isDefaultNoExecuteToleration(toleration) {
			continue
		}
		out = append(out, toleration)
	}
	return out
}

func isDefaultNoExecuteToleration(toleration corev1.Toleration) bool {
	if toleration.Operator != corev1.TolerationOpExists || toleration.Effect != corev1.TaintEffectNoExecute || toleration.TolerationSeconds == nil || *toleration.TolerationSeconds != 300 {
		return false
	}
	return toleration.Key == "node.kubernetes.io/not-ready" || toleration.Key == "node.kubernetes.io/unreachable"
}

func removeDefaultServiceAccountVolumes(spec *corev1.PodSpec) map[string]struct{} {
	removed := make(map[string]struct{})
	var volumes []corev1.Volume
	for _, volume := range spec.Volumes {
		if isDefaultServiceAccountVolume(volume) {
			removed[volume.Name] = struct{}{}
			continue
		}
		volumes = append(volumes, volume)
	}
	spec.Volumes = volumes
	return removed
}

func isDefaultServiceAccountVolume(volume corev1.Volume) bool {
	if !strings.HasPrefix(volume.Name, "kube-api-access-") {
		return false
	}
	if volume.Projected == nil || len(volume.Projected.Sources) == 0 {
		return false
	}
	for _, source := range volume.Projected.Sources {
		if source.ServiceAccountToken != nil {
			return true
		}
	}
	return false
}

func removeVolumeMounts(containers []corev1.Container, removedVolumes map[string]struct{}) {
	if len(removedVolumes) == 0 {
		return
	}
	for i := range containers {
		var mounts []corev1.VolumeMount
		for _, mount := range containers[i].VolumeMounts {
			if _, ok := removedVolumes[mount.Name]; ok {
				continue
			}
			mounts = append(mounts, mount)
		}
		containers[i].VolumeMounts = mounts
	}
}

// patchPodImagesForInPlaceUpdate updates only container images and records the
// target hash. The hash annotation lets later reconciles detect that kubelet
// still needs to restart containers and report completion only after readiness.
func patchPodImagesForInPlaceUpdate(ctx context.Context, cli client.Client, pod *corev1.Pod, role *orchestrationv1alpha1.RoleSpec, targetHash string) (bool, error) {
	updated := pod.DeepCopy()
	changed := updateContainerImages(updated.Spec.Containers, role.Template.Spec.Containers)

	if updated.Annotations == nil {
		updated.Annotations = make(map[string]string)
	}
	if changed && updated.Annotations[constants.RoleInPlaceUpdateStateAnnotationKey] == "" {
		stateJSON, err := buildInPlaceUpdateStateJSON(pod, role)
		if err != nil {
			return false, err
		}
		updated.Annotations[constants.RoleInPlaceUpdateStateAnnotationKey] = stateJSON
	}
	if updated.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] != targetHash {
		updated.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] = targetHash
		changed = true
	}
	if !changed {
		return false, nil
	}
	if err := setInPlaceReadinessCondition(ctx, cli, pod, corev1.ConditionFalse); err != nil {
		return false, err
	}
	if err := cli.Patch(ctx, updated, client.MergeFrom(pod)); err != nil {
		return false, err
	}
	return true, nil
}

func buildInPlaceUpdateStateJSON(pod *corev1.Pod, role *orchestrationv1alpha1.RoleSpec) (string, error) {
	desiredImages := make(map[string]string, len(role.Template.Spec.Containers))
	for _, container := range role.Template.Spec.Containers {
		desiredImages[container.Name] = container.Image
	}
	state := inPlaceUpdateState{
		LastContainerStatuses: make(map[string]inPlaceUpdateContainerStatus),
	}
	for _, status := range pod.Status.ContainerStatuses {
		for _, container := range pod.Spec.Containers {
			if container.Name == status.Name && desiredImages[container.Name] != "" && desiredImages[container.Name] != container.Image {
				state.LastContainerStatuses[status.Name] = inPlaceUpdateContainerStatus{ImageID: status.ImageID}
				break
			}
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// hasInPlaceUpdateInProgress gates replacement rollouts while an image patch is
// still waiting for kubelet to restart containers and report the new image. The
// controller currently has no timeout or automatic rollback for a pod that never
// reports the new runtime image; operators must clear or recreate the stuck pod.
func hasInPlaceUpdateInProgress(ctx context.Context, cli client.Client, namespace, roleSetName string) (bool, error) {
	pods := &corev1.PodList{}
	if err := cli.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabels{constants.RoleSetNameLabelKey: roleSetName},
	); err != nil {
		return false, err
	}
	for i := range pods.Items {
		if pods.Items[i].Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] != "" {
			return true, nil
		}
	}
	return false, nil
}

// markInPlaceUpdateComplete promotes the target hash to the pod labels only
// after the pod is ready and container statuses show the requested images. This
// keeps rollout bookkeeping aligned with the actual runtime state.
func markInPlaceUpdateComplete(ctx context.Context, cli client.Client, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, pod *corev1.Pod, targetHash string) (bool, error) {
	if pod.Labels[constants.RoleTemplateHashLabelKey] == targetHash {
		if _, ok := pod.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey]; ok {
			if err := setInPlaceReadinessCondition(ctx, cli, pod, corev1.ConditionTrue); err != nil {
				return false, err
			}
			updated := pod.DeepCopy()
			delete(updated.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)
			delete(updated.Annotations, constants.RoleInPlaceUpdateStateAnnotationKey)
			delete(updated.Annotations, constants.RoleInPlaceUpdatePendingReasonAnnotationKey)
			updateRoleRevisionLabels(updated, roleSet, role)
			if err := cli.Patch(ctx, updated, client.MergeFrom(pod)); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if pod.Annotations[constants.RoleInPlaceUpdateTargetHashAnnotationKey] != targetHash {
		return false, nil
	}
	if pending := inPlaceUpdatePendingStatusForPod(pod); pending != nil {
		logInPlaceUpdatePending(roleSet, role, pod, pending)
		if err := setInPlaceUpdatePendingReason(ctx, cli, pod, pending.Reason); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := setInPlaceReadinessCondition(ctx, cli, pod, corev1.ConditionTrue); err != nil {
		return false, err
	}

	updated := pod.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = make(map[string]string)
	}
	updated.Labels[constants.RoleTemplateHashLabelKey] = targetHash
	updateRoleRevisionLabels(updated, roleSet, role)
	delete(updated.Annotations, constants.RoleInPlaceUpdateTargetHashAnnotationKey)
	delete(updated.Annotations, constants.RoleInPlaceUpdateStateAnnotationKey)
	delete(updated.Annotations, constants.RoleInPlaceUpdatePendingReasonAnnotationKey)
	if err := cli.Patch(ctx, updated, client.MergeFrom(pod)); err != nil {
		return false, err
	}
	return true, nil
}

// updateRoleRevisionLabels keeps the role revision labels in sync when an
// in-place rollout completes without recreating the pod.
func updateRoleRevisionLabels(pod *corev1.Pod, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) {
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	roleRevisionKey := fmt.Sprintf("%s.%s", constants.RoleRevisionAnnotationPrefix, role.Name)
	if roleRevision, ok := roleSet.Annotations[roleRevisionKey]; ok {
		pod.Labels[constants.RoleRevisionLabelKey] = roleRevision
	}
	roleRevisionNameKey := fmt.Sprintf("%s.%s", constants.RoleRevisionNameAnnotationPrefix, role.Name)
	if roleRevisionName, ok := roleSet.Annotations[roleRevisionNameKey]; ok {
		pod.Labels[constants.RoleRevisionNameLabelKey] = roleRevisionName
	}
}

// podReadyWithRuntimeImages verifies that kubelet has restarted the containers
// and is reporting image IDs that match the patched pod spec. This intentionally
// waits forever rather than promoting a hash on a timer; timeout handling is a
// future enhancement.
func podReadyWithRuntimeImages(pod *corev1.Pod) bool {
	return inPlaceUpdatePendingStatusForPod(pod) == nil
}

func inPlaceUpdatePendingStatusForPod(pod *corev1.Pod) *inPlaceUpdatePendingStatus {
	if !podutil.IsPodReady(pod) {
		return &inPlaceUpdatePendingStatus{Reason: constants.RoleInPlaceUpdatePendingReasonPodNotReady}
	}

	state, ok := getInPlaceUpdateState(pod)
	if !ok {
		state = inPlaceUpdateState{}
	}
	statusByName := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		statusByName[status.Name] = status
	}
	for _, container := range pod.Spec.Containers {
		status, ok := statusByName[container.Name]
		if !ok {
			return &inPlaceUpdatePendingStatus{
				Reason:       constants.RoleInPlaceUpdatePendingReasonContainerStatusMissing,
				Container:    container.Name,
				DesiredImage: container.Image,
			}
		}
		if !status.Ready {
			return &inPlaceUpdatePendingStatus{
				Reason:         constants.RoleInPlaceUpdatePendingReasonContainerNotReady,
				Container:      container.Name,
				DesiredImage:   container.Image,
				RuntimeImage:   status.Image,
				CurrentImageID: status.ImageID,
			}
		}
		if !runtimeImageMatchesSpec(status.Image, container.Image) {
			return &inPlaceUpdatePendingStatus{
				Reason:         constants.RoleInPlaceUpdatePendingReasonRuntimeImageMismatch,
				Container:      container.Name,
				DesiredImage:   container.Image,
				RuntimeImage:   status.Image,
				CurrentImageID: status.ImageID,
			}
		}
		if oldStatus, ok := state.LastContainerStatuses[container.Name]; ok && oldStatus.ImageID != "" && status.ImageID == oldStatus.ImageID {
			return &inPlaceUpdatePendingStatus{
				Reason:         constants.RoleInPlaceUpdatePendingReasonImageIDUnchanged,
				Container:      container.Name,
				DesiredImage:   container.Image,
				RuntimeImage:   status.Image,
				OldImageID:     oldStatus.ImageID,
				CurrentImageID: status.ImageID,
			}
		}
	}

	initStatusByName := make(map[string]corev1.ContainerStatus, len(pod.Status.InitContainerStatuses))
	for _, status := range pod.Status.InitContainerStatuses {
		initStatusByName[status.Name] = status
	}
	for _, container := range pod.Spec.InitContainers {
		status, ok := initStatusByName[container.Name]
		if !ok {
			return &inPlaceUpdatePendingStatus{
				Reason:       constants.RoleInPlaceUpdatePendingReasonContainerStatusMissing,
				Container:    container.Name,
				DesiredImage: container.Image,
			}
		}
		if !runtimeImageMatchesSpec(status.Image, container.Image) {
			return &inPlaceUpdatePendingStatus{
				Reason:         constants.RoleInPlaceUpdatePendingReasonRuntimeImageMismatch,
				Container:      container.Name,
				DesiredImage:   container.Image,
				RuntimeImage:   status.Image,
				CurrentImageID: status.ImageID,
			}
		}
	}
	return nil
}

func setInPlaceUpdatePendingReason(ctx context.Context, cli client.Client, pod *corev1.Pod, reason string) error {
	if pod.Annotations[constants.RoleInPlaceUpdatePendingReasonAnnotationKey] == reason {
		return nil
	}
	updated := pod.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = make(map[string]string)
	}
	updated.Annotations[constants.RoleInPlaceUpdatePendingReasonAnnotationKey] = reason
	return cli.Patch(ctx, updated, client.MergeFrom(pod))
}

func logInPlaceUpdatePending(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, pod *corev1.Pod, pending *inPlaceUpdatePendingStatus) {
	klog.InfoS("in-place update pending",
		"roleset", fmt.Sprintf("%s/%s", roleSet.Namespace, roleSet.Name),
		"role", role.Name,
		"pod", pod.Name,
		"reason", pending.Reason,
		"container", pending.Container,
		"desiredImage", pending.DesiredImage,
		"runtimeImage", pending.RuntimeImage,
		"oldImageID", pending.OldImageID,
		"currentImageID", pending.CurrentImageID)
}

func getInPlaceUpdateState(pod *corev1.Pod) (inPlaceUpdateState, bool) {
	if pod.Annotations == nil || pod.Annotations[constants.RoleInPlaceUpdateStateAnnotationKey] == "" {
		return inPlaceUpdateState{}, false
	}
	state := inPlaceUpdateState{}
	if err := json.Unmarshal([]byte(pod.Annotations[constants.RoleInPlaceUpdateStateAnnotationKey]), &state); err != nil {
		return inPlaceUpdateState{}, false
	}
	return state, true
}

func setInPlaceReadinessCondition(ctx context.Context, cli client.Client, pod *corev1.Pod, status corev1.ConditionStatus) error {
	if !hasInPlaceReadinessGate(pod) {
		return nil
	}
	updated := pod.DeepCopy()
	setPodCondition(updated, corev1.PodCondition{
		Type:   corev1.PodConditionType(constants.RoleInPlaceUpdateReadyCondition),
		Status: status,
	})
	if status == corev1.ConditionFalse {
		setPodCondition(updated, corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
		})
	}
	return cli.Status().Patch(ctx, updated, client.MergeFrom(pod))
}

func hasInPlaceReadinessGate(pod *corev1.Pod) bool {
	for _, gate := range pod.Spec.ReadinessGates {
		if gate.ConditionType == corev1.PodConditionType(constants.RoleInPlaceUpdateReadyCondition) {
			return true
		}
	}
	return false
}

func setPodCondition(pod *corev1.Pod, condition corev1.PodCondition) {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == condition.Type {
			pod.Status.Conditions[i].Status = condition.Status
			return
		}
	}
	pod.Status.Conditions = append(pod.Status.Conditions, condition)
}

// runtimeImageMatchesSpec accepts the formats Kubernetes commonly reports in
// ContainerStatus.Image: exact spec image, implicit ":latest" for untagged
// images, Docker Hub library defaulting ("nginx" -> "docker.io/library/nginx"),
// and Docker Hub namespace defaulting ("org/image" -> "docker.io/org/image").
// It does not try to resolve arbitrary registry aliases or compare digests from
// ImageID; ImageID is only used to detect that a restarted container changed.
func runtimeImageMatchesSpec(runtimeImage, specImage string) bool {
	for _, candidate := range runtimeImageCandidates(specImage) {
		if runtimeImage == candidate {
			return true
		}
	}
	return false
}

func runtimeImageCandidates(specImage string) []string {
	candidates := []string{specImage}
	if !imageHasTagOrDigest(specImage) {
		candidates = append(candidates, specImage+":latest")
	}
	if defaulted := defaultDockerImageName(specImage); defaulted != specImage {
		candidates = append(candidates, defaulted)
		if !imageHasTagOrDigest(defaulted) {
			candidates = append(candidates, defaulted+":latest")
		}
	}
	return candidates
}

func defaultDockerImageName(image string) string {
	parts := strings.Split(image, "/")
	if len(parts) == 1 {
		return "docker.io/library/" + image
	}
	first := parts[0]
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return image
	}
	return "docker.io/" + image
}

func imageHasTagOrDigest(image string) bool {
	if strings.Contains(image, "@") {
		return true
	}
	lastSlash := strings.LastIndex(image, "/")
	return strings.Contains(image[lastSlash+1:], ":")
}

func podImagesDiffer(pod *corev1.Pod, role *orchestrationv1alpha1.RoleSpec) bool {
	return containersImagesDiffer(pod.Spec.Containers, role.Template.Spec.Containers)
}

func containersImagesDiffer(current, desired []corev1.Container) bool {
	desiredByName := make(map[string]string, len(desired))
	for _, container := range desired {
		desiredByName[container.Name] = container.Image
	}
	for _, container := range current {
		if image, ok := desiredByName[container.Name]; ok && container.Image != image {
			return true
		}
	}
	return false
}

func updateContainerImages(current, desired []corev1.Container) bool {
	desiredByName := make(map[string]string, len(desired))
	for _, container := range desired {
		desiredByName[container.Name] = container.Image
	}

	changed := false
	for i := range current {
		if image, ok := desiredByName[current[i].Name]; ok && current[i].Image != image {
			current[i].Image = image
			changed = true
		}
	}
	return changed
}
