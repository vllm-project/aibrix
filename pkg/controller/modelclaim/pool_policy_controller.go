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

package modelclaim

import (
	"context"
	"fmt"
	"sync"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// poolPolicyManager is deliberately controller-local. A controller restart
// clears its counter baseline and therefore delays activity-derived decisions
// until another snapshot arrives, which is safer than reconstructing activity
// from stale annotations or status.
type poolPolicyManager struct {
	mu       sync.Mutex
	now      func() time.Time
	lastRun  map[types.NamespacedName]time.Time
	activity map[string]poolActivityRecord
	pending  map[string]time.Time
}

type poolActivityRecord struct {
	successTotal int64
	known        bool
	lastActive   time.Time
}

func newPoolPolicyManager(now func() time.Time) *poolPolicyManager {
	if now == nil {
		now = time.Now
	}
	return &poolPolicyManager{
		now:      now,
		lastRun:  make(map[types.NamespacedName]time.Time),
		activity: make(map[string]poolActivityRecord),
		pending:  make(map[string]time.Time),
	}
}

func (m *poolPolicyManager) begin(pool types.NamespacedName) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if last, found := m.lastRun[pool]; found && now.Sub(last) < DefaultRequeueDuration {
		return false
	}
	m.lastRun[pool] = now
	return true
}

func (m *poolPolicyManager) observe(
	key string,
	model RuntimeSnapshotModel,
) (poolRequestActivity, bool) {
	if !model.RequestMetricsObserved || model.RequestSuccessTotal == nil {
		return poolRequestActivity{}, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	current := *model.RequestSuccessTotal
	record := m.activity[key]
	initialized := record.known
	delta := int64(0)
	if record.known && current >= record.successTotal {
		delta = current - record.successTotal
	}
	inFlight := max(model.RequestsRunning, int64(0)) + max(model.RequestsWaiting, int64(0))
	active := inFlight > 0 || delta > 0
	if !record.known || current < record.successTotal || active {
		record.lastActive = now
	}
	record.successTotal = current
	record.known = true
	m.activity[key] = record
	return poolRequestActivity{
		Active:           active,
		RequestsInFlight: inFlight,
		CompletionDelta:  delta,
		LastActive:       record.lastActive,
		Initialized:      initialized,
	}, true
}

// reserveSleep atomically verifies that a claim still has another active
// instance before parking one. A short reservation bridges the runtime sleep
// request and the following status update, so concurrent pool ticks cannot
// sleep the final routable replica.
func (m *poolPolicyManager) reserveSleep(pm *modelv1alpha1.ModelClaim, podName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for key, at := range m.pending {
		if now.Sub(at) > 2*time.Minute {
			delete(m.pending, key)
		}
	}
	targetKey := poolSleepReservationKey(pm, podName)
	if _, pending := m.pending[targetKey]; pending {
		return false
	}
	targetActive := false
	hasOtherActive := false
	for _, instance := range pm.Status.Instances {
		if instance.Phase != modelv1alpha1.ModelClaimActive {
			continue
		}
		if instance.Pod == podName {
			targetActive = true
			continue
		}
		if _, pending := m.pending[poolSleepReservationKey(pm, instance.Pod)]; !pending {
			hasOtherActive = true
		}
	}
	if !targetActive || !hasOtherActive {
		return false
	}
	m.pending[targetKey] = now
	return true
}

func (m *poolPolicyManager) releaseSleep(pm *modelv1alpha1.ModelClaim, podName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, poolSleepReservationKey(pm, podName))
}

func poolSleepReservationKey(pm *modelv1alpha1.ModelClaim, podName string) string {
	claimID := string(pm.UID)
	if claimID == "" {
		claimID = pm.Namespace + "/" + pm.Name
	}
	return claimID + "/" + podName
}

func (r *ModelClaimReconciler) poolPolicyManager() *poolPolicyManager {
	if r.PoolPolicy != nil {
		return r.PoolPolicy
	}
	// Production and normal tests initialize the manager. Keeping this fallback
	// makes narrow reconciler unit tests safe without turning policy into a
	// persistent or globally shared singleton.
	return newPoolPolicyManager(time.Now)
}

type poolPolicySource struct {
	key        types.NamespacedName
	deployment *appsv1.Deployment
	policy     *poolPolicy
}

// reconcilePoolPolicies finds the Deployment that owns each selected warm pod
// and runs an optional policy once per pool. Lifecycle reconciliation remains
// independent: any policy issue is logged and retried next tick rather than
// failing an otherwise healthy ModelClaim.
func (r *ModelClaimReconciler) reconcilePoolPolicies(ctx context.Context, candidates []corev1.Pod) {
	seen := make(map[types.NamespacedName]struct{}, len(candidates))
	manager := r.poolPolicyManager()
	for i := range candidates {
		source, err := r.poolPolicyForPod(ctx, &candidates[i])
		if err != nil {
			klog.ErrorS(err, "unable to resolve ModelClaim pool policy", "pod", klog.KObj(&candidates[i]))
			continue
		}
		if source == nil {
			continue
		}
		if _, done := seen[source.key]; done {
			continue
		}
		seen[source.key] = struct{}{}
		if !manager.begin(source.key) {
			continue
		}
		if err := r.reconcilePoolPolicy(ctx, source, manager); err != nil {
			klog.ErrorS(err, "ModelClaim pool policy tick failed", "deployment", klog.KObj(source.deployment))
		}
	}
}

func (r *ModelClaimReconciler) poolPolicyForPod(
	ctx context.Context,
	pod *corev1.Pod,
) (*poolPolicySource, error) {
	podOwner := metav1.GetControllerOf(pod)
	if podOwner == nil || podOwner.Kind != "ReplicaSet" {
		return nil, nil
	}
	replicaSet := &appsv1.ReplicaSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: podOwner.Name}, replicaSet); err != nil {
		return nil, err
	}
	replicaSetOwner := metav1.GetControllerOf(replicaSet)
	if replicaSetOwner == nil || replicaSetOwner.Kind != "Deployment" {
		return nil, nil
	}
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: pod.Namespace, Name: replicaSetOwner.Name}
	if err := r.Get(ctx, key, deployment); err != nil {
		return nil, err
	}
	raw := deployment.Annotations[constants.ModelPoolPolicyAnnotationKey]
	if raw == "" {
		return nil, nil
	}
	policy, err := parsePoolPolicy(raw)
	if err != nil {
		return nil, err
	}
	return &poolPolicySource{key: key, deployment: deployment, policy: policy}, nil
}

func (r *ModelClaimReconciler) reconcilePoolPolicy(
	ctx context.Context,
	source *poolPolicySource,
	manager *poolPolicyManager,
) error {
	if source.policy.Reclaim == nil &&
		(source.policy.Lifecycle == nil || source.policy.Lifecycle.SleepAfterSeconds == 0) {
		return nil
	}
	pods, err := r.poolPolicyPods(ctx, source.deployment)
	if err != nil {
		return err
	}
	for i := range pods {
		pod := &pods[i]
		snapshot, err := r.Runtime.Snapshot(ctx, pod.Status.PodIP, DefaultRuntimePort)
		if err != nil {
			klog.V(4).InfoS("pool policy snapshot failed", "pod", klog.KObj(pod), "err", err)
			continue
		}
		if snapshot == nil {
			klog.V(4).InfoS("pool policy received empty runtime snapshot", "pod", klog.KObj(pod))
			continue
		}
		if len(snapshot.Accelerators) != 1 {
			// Both dynamic KV limits and vLLM sleep are held to the verified
			// single-GPU contract until multi-GPU kvcached accounting is tested.
			klog.V(4).InfoS("pool policy skips non-single-GPU runtime", "pod", klog.KObj(pod))
			continue
		}
		if source.policy.Reclaim != nil {
			models, observed := manager.modelsForKVPolicy(pod, snapshot)
			if !observed {
				klog.V(4).InfoS("pool policy waits for complete request observations", "pod", klog.KObj(pod))
			} else {
				targets, err := computePoolKVTargets(
					source.policy.Reclaim.CapacityBytes,
					source.policy.Reclaim.GuaranteedFloorPercent,
					models,
				)
				if err != nil {
					klog.V(4).InfoS("pool policy did not produce a safe KV plan", "pod", klog.KObj(pod), "err", err)
				} else {
					for _, model := range snapshot.Models {
						target, found := targets[model.ModelName]
						if !found || target == model.KVCapacityBytes {
							continue
						}
						operationID := fmt.Sprintf(
							"pool-policy-kv/%s/%s/%s/%d",
							source.key.String(), pod.UID, model.IPCName, target,
						)
						if _, err := r.Runtime.SetKVLimit(ctx, pod.Status.PodIP, DefaultRuntimePort, &SetKVLimitRequest{
							ModelName: model.ModelName, LimitBytes: target, OperationID: operationID,
						}); err != nil {
							klog.ErrorS(err, "pool policy could not apply KV limit", "pod", klog.KObj(pod), "model", model.ModelName, "target", target)
						}
					}
				}
			}
		}
		if source.policy.Lifecycle != nil && source.policy.Lifecycle.SleepAfterSeconds > 0 {
			r.reconcilePoolIdleSleep(ctx, source, manager, pod, snapshot)
		}
	}
	return nil
}

func (r *ModelClaimReconciler) reconcilePoolIdleSleep(
	ctx context.Context,
	source *poolPolicySource,
	manager *poolPolicyManager,
	pod *corev1.Pod,
	snapshot *RuntimeSnapshot,
) {
	claims := &modelv1alpha1.ModelClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(pod.Namespace)); err != nil {
		klog.ErrorS(err, "pool policy could not list ModelClaims for idle sleep", "pod", klog.KObj(pod))
		return
	}
	idleAfter := time.Duration(source.policy.Lifecycle.SleepAfterSeconds) * time.Second
	for i := range snapshot.Models {
		model := snapshot.Models[i]
		if model.Phase != runtimePhaseActive || !model.Alive || !model.Ready {
			continue
		}
		activity, observed := manager.observe(string(pod.UID)+"/"+model.IPCName, model)
		if !observed || !activity.Initialized || activity.Active || manager.now().Sub(activity.LastActive) < idleAfter {
			continue
		}
		claim := claimForRuntimeSnapshot(claims, model)
		if claim == nil || !r.hasOtherRuntimeReadyInstance(ctx, claim, pod.Name) ||
			!manager.reserveSleep(claim, pod.Name) {
			continue
		}
		port, active := activeClaimInstancePort(claim, pod.Name)
		if !active {
			manager.releaseSleep(claim, pod.Name)
			continue
		}
		// Remove the route before sleeping. If the runtime request fails, restore
		// the previous port so an unsuccessful policy operation cannot strand an
		// otherwise healthy engine.
		if err := r.annotateWarmPod(ctx, claim, pod, 0); err != nil {
			manager.releaseSleep(claim, pod.Name)
			klog.ErrorS(err, "pool policy could not de-route idle engine", "pod", klog.KObj(pod), "model", model.ModelName)
			continue
		}
		operationID := fmt.Sprintf("pool-policy-sleep/%s/%s/%s", source.key.String(), pod.UID, model.IPCName)
		if _, err := r.Runtime.Sleep(ctx, pod.Status.PodIP, DefaultRuntimePort, &SleepRequest{
			ModelName: model.ModelName, Level: 1, OperationID: operationID,
		}); err != nil {
			if restoreErr := r.annotateWarmPod(ctx, claim, pod, port); restoreErr != nil {
				klog.ErrorS(restoreErr, "pool policy could not restore route after failed sleep", "pod", klog.KObj(pod), "model", model.ModelName)
			}
			manager.releaseSleep(claim, pod.Name)
			klog.ErrorS(err, "pool policy could not sleep idle engine", "pod", klog.KObj(pod), "model", model.ModelName)
			continue
		}
		if err := r.markClaimInstanceSleeping(ctx, claim, pod.Name); err != nil {
			// Keep the reservation until the next successful status reconciliation
			// so a concurrent policy tick cannot sleep the final active replica.
			klog.ErrorS(err, "pool policy could not persist sleeping ModelClaim instance", "claim", klog.KObj(claim), "pod", klog.KObj(pod))
			continue
		}
		manager.releaseSleep(claim, pod.Name)
		r.Recorder.Eventf(claim, corev1.EventTypeNormal, "Sleeping", "model %s idle past %ds; sleeping redundant engine on pod %s", model.ModelName, source.policy.Lifecycle.SleepAfterSeconds, pod.Name)
	}
}

// hasOtherRuntimeReadyInstance validates the redundancy guard against the
// peer's authoritative runtime snapshot rather than trusting a potentially
// stale ModelClaim status entry. This is intentionally a small number of
// direct reads: it only runs for an engine that already passed its idle timer.
func (r *ModelClaimReconciler) hasOtherRuntimeReadyInstance(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	targetPod string,
) bool {
	if pm.UID == "" {
		return false
	}
	for _, instance := range pm.Status.Instances {
		if instance.Pod == targetPod || instance.Phase != modelv1alpha1.ModelClaimActive {
			continue
		}
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: instance.Pod}, pod); err != nil ||
			pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		snapshot, err := r.Runtime.Snapshot(ctx, pod.Status.PodIP, DefaultRuntimePort)
		if err != nil || snapshot == nil {
			continue
		}
		for i := range snapshot.Models {
			model := snapshot.Models[i]
			if model.ClaimRef == nil || model.ClaimRef.UID != string(pm.UID) ||
				model.ClaimRef.Namespace != pm.Namespace || model.ClaimRef.Name != pm.Name {
				continue
			}
			if model.Phase == runtimePhaseActive && model.Alive && model.Ready {
				return true
			}
		}
	}
	return false
}

func claimForRuntimeSnapshot(claims *modelv1alpha1.ModelClaimList, model RuntimeSnapshotModel) *modelv1alpha1.ModelClaim {
	if model.ClaimRef == nil || model.ClaimRef.UID == "" {
		return nil
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Namespace == model.ClaimRef.Namespace && claim.Name == model.ClaimRef.Name && string(claim.UID) == model.ClaimRef.UID {
			return claim
		}
	}
	return nil
}

func activeClaimInstancePort(pm *modelv1alpha1.ModelClaim, podName string) (int32, bool) {
	for _, instance := range pm.Status.Instances {
		if instance.Pod == podName && instance.Phase == modelv1alpha1.ModelClaimActive {
			return instance.Port, true
		}
	}
	return 0, false
}

func (r *ModelClaimReconciler) markClaimInstanceSleeping(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	podName string,
) error {
	latest := &modelv1alpha1.ModelClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name}, latest); err != nil {
		return err
	}
	changed := false
	for i := range latest.Status.Instances {
		if latest.Status.Instances[i].Pod == podName {
			if latest.Status.Instances[i].Phase != modelv1alpha1.ModelClaimSleeping {
				latest.Status.Instances[i].Phase = modelv1alpha1.ModelClaimSleeping
				changed = true
			}
			break
		}
	}
	if !changed {
		return nil
	}
	r.recomputeReadiness(latest)
	setClaimGauges(latest)
	return r.Status().Update(ctx, latest)
}

func (m *poolPolicyManager) modelsForKVPolicy(
	pod *corev1.Pod,
	snapshot *RuntimeSnapshot,
) ([]poolKVModel, bool) {
	models := make([]poolKVModel, 0, len(snapshot.Models))
	for i := range snapshot.Models {
		model := snapshot.Models[i]
		if model.Phase != runtimePhaseActive || !model.Alive || !model.Ready {
			continue
		}
		if model.KVCapacityBytes <= 0 {
			return nil, false
		}
		activity, observed := m.observe(string(pod.UID)+"/"+model.IPCName, model)
		if !observed {
			return nil, false
		}
		models = append(models, poolKVModel{
			Name:            model.ModelName,
			KVUsedBytes:     model.KVUsedBytes,
			KVCapacityBytes: model.KVCapacityBytes,
			Activity:        activity,
		})
	}
	return models, true
}

func (r *ModelClaimReconciler) poolPolicyPods(
	ctx context.Context,
	deployment *appsv1.Deployment,
) ([]corev1.Pod, error) {
	if deployment.Spec.Selector == nil {
		return nil, fmt.Errorf("deployment %s has no selector", deployment.Name)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, err
	}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return nil, err
	}
	pods := make([]corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pod := podList.Items[i]
		if pod.Labels[constants.ModelPoolLabelEnabled] != constants.ModelPoolLabelEnabledValue ||
			pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		pods = append(pods, pod)
	}
	return pods, nil
}
