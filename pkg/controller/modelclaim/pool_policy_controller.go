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
	config   map[types.NamespacedName]poolConfigRecord
}

type poolActivityRecord struct {
	successTotal int64
	known        bool
}

type poolConfigRecord struct {
	raw        string
	errorClass string
}

func newPoolPolicyManager(now func() time.Time) *poolPolicyManager {
	if now == nil {
		now = time.Now
	}
	return &poolPolicyManager{
		now:      now,
		lastRun:  make(map[types.NamespacedName]time.Time),
		activity: make(map[string]poolActivityRecord),
		config:   make(map[types.NamespacedName]poolConfigRecord),
	}
}

// observeConfig records the latest annotation parse outcome and reports
// whether a warning or recovery Event is due. Deduplication keys on the
// annotation content because metadata edits do not bump the generation.
func (m *poolPolicyManager) observeConfig(
	pool types.NamespacedName,
	raw string,
	errorClass string,
) (warn, recovered bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := poolConfigRecord{raw: raw, errorClass: errorClass}
	previous, known := m.config[pool]
	m.config[pool] = next
	if next.errorClass != "" {
		return !known || previous != next, false
	}
	return false, known && previous.errorClass != ""
}

// forgetConfig clears tracking once the annotation is removed, so a re-added
// policy is treated as fresh configuration.
func (m *poolPolicyManager) forgetConfig(pool types.NamespacedName) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, known := m.config[pool]
	delete(m.config, pool)
	return known
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
	current := *model.RequestSuccessTotal
	record := m.activity[key]
	delta := int64(0)
	if record.known && current >= record.successTotal {
		delta = current - record.successTotal
	}
	inFlight := max(model.RequestsRunning, int64(0)) + max(model.RequestsWaiting, int64(0))
	active := inFlight > 0 || delta > 0
	record.successTotal = current
	record.known = true
	m.activity[key] = record
	return poolRequestActivity{
		Active:           active,
		RequestsInFlight: inFlight,
		CompletionDelta:  delta,
	}, true
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
// and runs an optional policy once per pool. ModelClaim reconciliation remains
// independent: any policy issue is logged and retried next tick rather than
// failing an otherwise healthy ModelClaim.
func (r *ModelClaimReconciler) reconcilePoolPolicies(ctx context.Context, candidates []corev1.Pod) {
	seen := make(map[types.NamespacedName]struct{}, len(candidates))
	manager := r.poolPolicyManager()
	for i := range candidates {
		deployment, err := r.poolDeploymentForPod(ctx, &candidates[i])
		if err != nil {
			klog.ErrorS(err, "unable to resolve ModelClaim pool deployment", "pod", klog.KObj(&candidates[i]))
			continue
		}
		if deployment == nil {
			continue
		}
		key := types.NamespacedName{Namespace: deployment.Namespace, Name: deployment.Name}
		if _, done := seen[key]; done {
			continue
		}
		seen[key] = struct{}{}
		policy := r.resolvePoolPolicy(deployment, manager)
		if policy == nil {
			continue
		}
		if !manager.begin(key) {
			continue
		}
		source := &poolPolicySource{key: key, deployment: deployment, policy: policy}
		if err := r.reconcilePoolPolicy(ctx, source, manager); err != nil {
			klog.ErrorS(err, "ModelClaim pool policy tick failed", "deployment", klog.KObj(deployment))
		}
	}
}

func (r *ModelClaimReconciler) poolDeploymentForPod(
	ctx context.Context,
	pod *corev1.Pod,
) (*appsv1.Deployment, error) {
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
	return deployment, nil
}

// resolvePoolPolicy parses the Deployment policy annotation and surfaces the
// outcome to operators through Deployment Events and the policy-valid gauge.
// Invalid configuration stays fail-closed: it returns nil so no KV plan runs.
func (r *ModelClaimReconciler) resolvePoolPolicy(
	deployment *appsv1.Deployment,
	manager *poolPolicyManager,
) *poolPolicy {
	pool := types.NamespacedName{Namespace: deployment.Namespace, Name: deployment.Name}
	raw := deployment.Annotations[constants.ModelPoolPolicyAnnotationKey]
	if raw == "" {
		if manager.forgetConfig(pool) {
			clearPoolPolicyMetrics(pool)
		}
		return nil
	}
	policy, err := parsePoolPolicy(raw)
	if err != nil {
		errorClass := poolPolicyErrorClass(err)
		setPoolPolicyValid(pool, false)
		if warn, _ := manager.observeConfig(pool, raw, errorClass); warn {
			r.Recorder.Eventf(deployment, corev1.EventTypeWarning, "InvalidPoolPolicy",
				"invalid %s annotation (%s), pool policy disabled: %v",
				constants.ModelPoolPolicyAnnotationKey, errorClass, err)
			klog.ErrorS(err, "invalid ModelClaim pool policy annotation",
				"deployment", klog.KObj(deployment), "errorClass", errorClass)
		}
		return nil
	}
	setPoolPolicyValid(pool, true)
	if _, recovered := manager.observeConfig(pool, raw, ""); recovered {
		r.Recorder.Eventf(deployment, corev1.EventTypeNormal, "PoolPolicyValid",
			"%s annotation is valid again, pool policy execution resumed",
			constants.ModelPoolPolicyAnnotationKey)
	}
	return policy
}

func (r *ModelClaimReconciler) reconcilePoolPolicy(
	ctx context.Context,
	source *poolPolicySource,
	manager *poolPolicyManager,
) error {
	if source.policy.Reclaim == nil {
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
			// Dynamic KV limits remain held to the verified single-GPU contract
			// until multi-GPU kvcached accounting is tested.
			klog.V(4).InfoS("pool policy skips non-single-GPU runtime", "pod", klog.KObj(pod))
			continue
		}
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
	return nil
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
