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
	"errors"
	"fmt"
	"sync"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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
	mu        sync.Mutex
	now       func() time.Time
	lastRun   map[types.NamespacedName]time.Time
	activity  map[string]poolActivityRecord
	config    map[types.NamespacedName]poolConfigRecord
	lastSweep time.Time
}

type poolActivityRecord struct {
	successTotal int64
	known        bool
	lastActive   time.Time
	lastSeen     time.Time
}

const (
	// poolActivityRetention is how long an unobserved record is kept. Pods are
	// observed every DefaultRequeueDuration, so only a gone pod or engine ages out.
	poolActivityRetention = 5 * time.Minute
	// poolActivitySweepInterval bounds the full-map scan to once a minute.
	poolActivitySweepInterval = time.Minute
)

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
	m.sweepActivityLocked(now)
	if last, found := m.lastRun[pool]; found && now.Sub(last) < DefaultRequeueDuration {
		return false
	}
	m.lastRun[pool] = now
	return true
}

// sweepActivityLocked drops records not observed within poolActivityRetention.
// It scans the whole map, so it runs at most once per poolActivitySweepInterval.
func (m *poolPolicyManager) sweepActivityLocked(now time.Time) {
	if now.Sub(m.lastSweep) < poolActivitySweepInterval {
		return
	}
	m.lastSweep = now
	for key, record := range m.activity {
		if now.Sub(record.lastSeen) > poolActivityRetention {
			delete(m.activity, key)
		}
	}
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
	initialized := record.known && current >= record.successTotal
	delta := int64(0)
	if initialized {
		delta = current - record.successTotal
	}
	inFlight := max(model.RequestsRunning, int64(0)) + max(model.RequestsWaiting, int64(0))
	active := inFlight > 0 || delta > 0
	if !initialized || active {
		record.lastActive = now
	}
	record.successTotal = current
	record.known = true
	record.lastSeen = now
	m.activity[key] = record
	return poolRequestActivity{
		Active:           active,
		RequestsInFlight: inFlight,
		CompletionDelta:  delta,
		LastActive:       record.lastActive,
		Initialized:      initialized,
	}, true
}

// lastActive is when the pool policy last saw an engine busy, or first saw it
// at all. It observes nothing itself: observing moves the baseline that the
// idle timer counts completions from.
func (m *poolPolicyManager) lastActive(key string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, found := m.activity[key]
	if !found || !record.known {
		return time.Time{}, false
	}
	return record.lastActive, true
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
func (r *ModelClaimReconciler) reconcilePoolPolicies(
	ctx context.Context,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) {
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
		if err := r.reconcilePoolPolicy(ctx, source, manager, readings); err != nil {
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

// podsWithoutWakeReserve names the pods whose pool has a sleeping engine keep
// no wake reserve. A pod whose pool cannot be read, or whose policy is not
// valid, is not named. Its sleeping engines keep their reserve, so the account
// charges them more than they may hold, never less.
func (r *ModelClaimReconciler) podsWithoutWakeReserve(ctx context.Context, pods []corev1.Pod) map[string]bool {
	without := map[string]bool{}
	pools := map[types.NamespacedName]bool{}
	for i := range pods {
		deployment, err := r.poolDeploymentForPod(ctx, &pods[i])
		if err != nil || deployment == nil {
			continue
		}
		pool := types.NamespacedName{Namespace: deployment.Namespace, Name: deployment.Name}
		keepsNone, seen := pools[pool]
		if !seen {
			keepsNone = keepsNoWakeReserve(deployment)
			pools[pool] = keepsNone
		}
		if keepsNone {
			without[pods[i].Name] = true
		}
	}
	return without
}

// keepsNoWakeReserve reports whether a pool's policy has a sleeping engine keep
// no wake reserve.
func keepsNoWakeReserve(deployment *appsv1.Deployment) bool {
	lifecycle := lifecycleOf(deployment)
	return lifecycle != nil && lifecycle.NoWakeReserveWhileAsleep
}

// lifecycleOf is a pool's lifecycle policy, read without being reported on,
// which the pool policy loop does. It is nil when the policy sets none, or is
// not valid.
func lifecycleOf(deployment *appsv1.Deployment) *poolLifecyclePolicy {
	raw := deployment.Annotations[constants.ModelPoolPolicyAnnotationKey]
	if raw == "" {
		return nil
	}
	policy, err := parsePoolPolicy(raw)
	if err != nil {
		return nil
	}
	return policy.Lifecycle
}

// poolLifecycleOf is the lifecycle policy of a pod's pool, or nil.
func (r *ModelClaimReconciler) poolLifecycleOf(ctx context.Context, pod *corev1.Pod) *poolLifecyclePolicy {
	deployment, err := r.poolDeploymentForPod(ctx, pod)
	if err != nil || deployment == nil {
		return nil
	}
	return lifecycleOf(deployment)
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
		recordPolicyEvaluation(pool, policyResultSkipped, errorClass)
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
	readings *runtimeReadings,
) error {
	if source.policy.Reclaim == nil && source.policy.Lifecycle == nil {
		recordPolicyEvaluation(source.key, policyResultSkipped, policyReasonNoReclaimPolicy)
		return nil
	}
	pods, err := r.poolPolicyPods(ctx, source.deployment)
	if err != nil {
		recordPolicyEvaluation(source.key, policyResultFailed, policyReasonPodListError)
		return err
	}
	if len(pods) == 0 {
		recordPolicyEvaluation(source.key, policyResultSkipped, policyReasonNoPods)
		return nil
	}
	for i := range pods {
		pod := &pods[i]
		snapshot, err := readings.of(ctx, pod)
		if err != nil {
			recordPolicyEvaluation(source.key, policyResultFailed, policyReasonSnapshotError)
			klog.V(4).InfoS("pool policy snapshot failed", "pod", klog.KObj(pod), "err", err)
			continue
		}
		if snapshot == nil {
			recordPolicyEvaluation(source.key, policyResultSkipped, policyReasonEmptySnapshot)
			klog.V(4).InfoS("pool policy received empty runtime snapshot", "pod", klog.KObj(pod))
			continue
		}
		if len(snapshot.Accelerators) != 1 {
			// Dynamic KV limits remain held to the verified single-GPU contract
			// until multi-GPU kvcached accounting is tested.
			recordPolicyEvaluation(source.key, policyResultSkipped, policyReasonUnsupportedTopology)
			klog.V(4).InfoS("pool policy skips non-single-GPU runtime", "pod", klog.KObj(pod))
			continue
		}
		decisionTime := snapshot.ObservedAt
		if decisionTime.IsZero() {
			decisionTime = manager.now()
		}
		activities, observed := manager.observeSnapshot(pod, snapshot)
		if source.policy.Reclaim != nil && r.claimHoldsAKVLimitOn(ctx, pod) {
			// A claim that declares its per-GPU cost has its engines held to a
			// limit derived from that declaration, and the health loop writes
			// that limit back whenever it finds another one in force. Two
			// writers on one segment would only overwrite each other, so the
			// declaration wins and the annotation stands down for this Pod.
			recordPolicyEvaluation(source.key, policyResultSkipped, policyReasonClaimHeldLimits)
			klog.V(4).InfoS("pool KV policy stands down where a claim holds the limit", "pod", klog.KObj(pod))
		} else if source.policy.Reclaim != nil && !observed {
			recordPolicyEvaluation(source.key, policyResultSkipped, policyReasonIncompleteMetrics)
			klog.V(4).InfoS("pool KV policy waits for complete request observations", "pod", klog.KObj(pod))
		} else if source.policy.Reclaim != nil {
			models := modelsForKVPolicy(snapshot, activities)
			targets, err := computePoolKVTargets(
				source.policy.Reclaim.CapacityBytes,
				source.policy.Reclaim.GuaranteedFloorPercent,
				models,
			)
			if err != nil {
				recordPolicyEvaluation(source.key, policyResultSkipped, policyReasonUnsafePlan)
				klog.V(4).InfoS("pool policy did not produce a safe KV plan", "pod", klog.KObj(pod), "err", err)
			} else {
				applied, failed := 0, 0
				for _, model := range snapshot.Models {
					target, found := targets[model.ModelName]
					if !found || target == model.KVCapacityBytes {
						continue
					}
					operationID := fmt.Sprintf(
						"pool-policy-kv/%s/%s/%s/%d/%d",
						source.key.String(), pod.UID, snapshotActivityKey(model), target, decisionTime.UnixNano(),
					)
					response, err := r.Runtime.SetKVLimit(ctx, pod.Status.PodIP, DefaultRuntimePort, &SetKVLimitRequest{
						ModelName: model.ModelName, LimitBytes: target, OperationID: operationID,
					})
					switch {
					case err != nil:
						failed++
						recordPolicyAction(source.key, policyActionSetKVLimit, policyResultFailed, policyReasonRuntimeError)
						klog.ErrorS(err, "pool policy could not apply KV limit", "pod", klog.KObj(pod), "model", model.ModelName, "target", target)
					case response == nil || !response.Applied:
						recordPolicyAction(source.key, policyActionSetKVLimit, policyResultSkipped, policyReasonNoChange)
					default:
						applied++
						recordPolicyAction(source.key, policyActionSetKVLimit, policyResultApplied, policyReasonApplied)
					}
				}
				if applied > 0 || failed > 0 {
					readings.forget(pod.Name)
				}
				switch {
				case failed > 0:
					recordPolicyEvaluation(source.key, policyResultFailed, policyReasonRuntimeError)
				case applied > 0:
					recordPolicyEvaluation(source.key, policyResultApplied, policyReasonApplied)
				default:
					recordPolicyEvaluation(source.key, policyResultSkipped, policyReasonNoChange)
				}
			}
		}
		if source.policy.Lifecycle != nil && !observed {
			klog.V(4).InfoS("pool lifecycle policy waits for complete request observations", "pod", klog.KObj(pod))
		} else if source.policy.Lifecycle != nil && source.policy.Lifecycle.SleepAfterSeconds > 0 {
			// Without sleepAfterSeconds no engine is put to sleep for being
			// idle. Its activity is still observed above, so that the one
			// idle longest can be put to sleep when room is needed.
			r.reconcilePoolIdleSleep(ctx, source, manager, pod, snapshot, activities, readings)
		}
	}
	return nil
}

// observeSnapshot records the request activity of each serving engine on a
// pod, and reports whether it could read every one. The pod's policies wait for
// a round in which it could. An engine whose counters could not be read does
// not keep the engines beside it from being recorded, though, so the one idle
// longest can still be put to sleep when room is needed.
func (m *poolPolicyManager) observeSnapshot(
	pod *corev1.Pod,
	snapshot *RuntimeSnapshot,
) (map[string]poolRequestActivity, bool) {
	activities := make(map[string]poolRequestActivity, len(snapshot.Models))
	complete := true
	for i := range snapshot.Models {
		model := snapshot.Models[i]
		if model.Phase != runtimePhaseActive || !model.Alive || !model.Ready {
			continue
		}
		activity, observed := m.observe(poolActivityKey(pod, model), model)
		if !observed {
			complete = false
			continue
		}
		activities[snapshotActivityKey(model)] = activity
	}
	if !complete {
		return nil, false
	}
	return activities, true
}

func modelsForKVPolicy(
	snapshot *RuntimeSnapshot,
	activities map[string]poolRequestActivity,
) []poolKVModel {
	models := make([]poolKVModel, 0, len(snapshot.Models))
	for i := range snapshot.Models {
		model := snapshot.Models[i]
		if model.Phase != runtimePhaseActive || !model.Alive || !model.Ready {
			continue
		}
		if model.KVCapacityBytes <= 0 {
			return nil
		}
		models = append(models, poolKVModel{
			Name:            model.ModelName,
			KVUsedBytes:     model.KVUsedBytes,
			KVCapacityBytes: model.KVCapacityBytes,
			Activity:        activities[snapshotActivityKey(model)],
		})
	}
	return models
}

func (r *ModelClaimReconciler) reconcilePoolIdleSleep(
	ctx context.Context,
	source *poolPolicySource,
	manager *poolPolicyManager,
	pod *corev1.Pod,
	snapshot *RuntimeSnapshot,
	activities map[string]poolRequestActivity,
	readings *runtimeReadings,
) {
	claims := &modelv1alpha1.ModelClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(pod.Namespace)); err != nil {
		klog.ErrorS(err, "pool lifecycle policy could not list ModelClaims", "pod", klog.KObj(pod))
		return
	}
	idleAfter := time.Duration(source.policy.Lifecycle.SleepAfterSeconds) * time.Second
	for i := range snapshot.Models {
		model := snapshot.Models[i]
		if model.Phase != runtimePhaseActive || !model.Alive || !model.Ready {
			continue
		}
		activity, found := activities[snapshotActivityKey(model)]
		if !found || !activity.Initialized || activity.Active {
			continue
		}
		claim := claimForRuntimeSnapshot(claims, model)
		if claim == nil || !isVLLMModel(claim) {
			continue
		}
		idleSince := activity.LastActive
		if model.LastTransition != nil && model.LastTransition.After(idleSince) {
			idleSince = *model.LastTransition
		}
		if routed := routedSince(claim); routed.After(idleSince) {
			idleSince = routed
		}
		if idleSince.IsZero() || manager.now().Sub(idleSince) < idleAfter {
			continue
		}

		port, active := activeClaimInstancePort(claim, pod.Name)
		if !active {
			continue
		}
		operationID := fmt.Sprintf(
			"pool-policy-sleep/%s/%s/%s/%d",
			source.key.String(), pod.UID, snapshotActivityKey(model), idleSince.UnixNano(),
		)
		if err := r.putEngineToSleep(ctx, claim, pod, port, model.ModelName, operationID, readings); err != nil {
			if errors.Is(err, errEngineServing) {
				klog.V(2).InfoS("pool lifecycle policy leaves an engine awake that serves a request",
					"pod", klog.KObj(pod), "model", model.ModelName)
			} else {
				klog.ErrorS(err, "pool lifecycle policy could not sleep idle engine", "pod", klog.KObj(pod), "model", model.ModelName)
			}
			continue
		}
		asleep := r.sleptEngine(ctx, pod, claim, readings)
		r.Recorder.Eventf(
			claim, corev1.EventTypeNormal, "Sleeping",
			"model %s idle past %ds; sleeping engine on pod %s%s",
			model.ModelName, source.policy.Lifecycle.SleepAfterSeconds, pod.Name, sleepingFootprintNote(asleep),
		)
		r.warnOfAnUnmeasuredSleep(claim, pod.Name, asleep, source.policy.Lifecycle.NoWakeReserveWhileAsleep)
	}
}

// errEngineServing is why an engine found idle is left awake: a request reached
// it before its route was taken back, and a sleep would abort that request.
var errEngineServing = errors.New("the engine serves a request")

// putEngineToSleep takes a claim's engine on a pod off its route, puts it to
// sleep at level 1, and records the instance as sleeping. The route is taken
// back first, so no request is routed to an engine going to sleep. A request
// may have reached the engine after the reading that found it idle, and vLLM
// aborts what an engine serves when it sleeps. So the engine is read again once
// its route is taken back, and one that serves is left awake. A route taken
// back for a sleep that did not happen is put back.
func (r *ModelClaimReconciler) putEngineToSleep(
	ctx context.Context,
	claim *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	port int32,
	modelName, operationID string,
	readings *runtimeReadings,
) error {
	if err := r.annotateWarmPodWithState(ctx, claim, pod, 0, constants.ModelClaimRoutingStateSleeping, ""); err != nil {
		return fmt.Errorf("take the route back: %w", err)
	}
	putRouteBack := func() {
		if err := r.annotateWarmPodWithState(
			ctx, claim, pod, port, constants.ModelClaimRoutingStateActive, "",
		); err != nil {
			klog.ErrorS(err, "could not put a route back after a sleep that did not happen",
				"pod", klog.KObj(pod), "model", modelName)
		}
	}
	readings.forget(pod.Name)
	if err := r.engineIsQuiet(ctx, pod, claim, modelName, readings); err != nil {
		putRouteBack()
		return err
	}
	_, err := r.Runtime.Sleep(ctx, pod.Status.PodIP, DefaultRuntimePort, &SleepRequest{
		ModelName: modelName, Level: 1, OperationID: operationID,
	})
	readings.forget(pod.Name)
	if err != nil {
		putRouteBack()
		return fmt.Errorf("sleep: %w", err)
	}
	if err := r.markClaimInstanceSleeping(ctx, claim, pod.Name); err != nil {
		return fmt.Errorf("record the sleep: %w", err)
	}
	return nil
}

// engineIsQuiet reads an engine again and reports errEngineServing when it has
// a request running or waiting, or when its requests could not be read. An
// engine the reading does not show cannot be put to sleep either.
func (r *ModelClaimReconciler) engineIsQuiet(
	ctx context.Context,
	pod *corev1.Pod,
	claim *modelv1alpha1.ModelClaim,
	modelName string,
	readings *runtimeReadings,
) error {
	snapshot, err := readings.of(ctx, pod)
	if err != nil {
		return fmt.Errorf("read the engine again: %w", err)
	}
	model := snapshotModelForClaim(snapshot, claim, modelName)
	if model == nil {
		return fmt.Errorf("the runtime no longer lists model %s", modelName)
	}
	if !model.RequestMetricsObserved || model.RequestsRunning > 0 || model.RequestsWaiting > 0 {
		return errEngineServing
	}
	return nil
}

// sleptEngine reads a claim's engine on a pod again, after it was put to sleep,
// for what its runtime measured it to hold asleep. It is nil when the runtime
// cannot be read.
func (r *ModelClaimReconciler) sleptEngine(
	ctx context.Context,
	pod *corev1.Pod,
	claim *modelv1alpha1.ModelClaim,
	readings *runtimeReadings,
) *RuntimeSnapshotModel {
	snapshot, err := readings.of(ctx, pod)
	if err != nil {
		return nil
	}
	return snapshotModelForClaim(snapshot, claim, servedModelName(claim))
}

// sleepingFootprintNote ends an Event about a sleep with what the engine holds
// asleep, as its runtime measured it.
func sleepingFootprintNote(model *RuntimeSnapshotModel) string {
	if footprint, known := sleepingFootprintOf(model); known {
		return fmt.Sprintf("; it holds %s asleep", byteSize(footprint))
	}
	return "; what it holds asleep could not be measured"
}

// byteSize words a size: in GiB from one GiB up, in MiB from one MiB up, and in
// bytes below that. A small engine asleep then does not read as holding none.
func byteSize(n int64) string {
	switch {
	case n >= 1<<30:
		return gibibytes(n)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// sleepingFootprintOf is what a sleeping engine holds, as its runtime measured
// it after the engine went to sleep.
func sleepingFootprintOf(model *RuntimeSnapshotModel) (int64, bool) {
	if model == nil || model.SleepingFootprintBytes == nil || *model.SleepingFootprintBytes <= 0 {
		return 0, false
	}
	return *model.SleepingFootprintBytes, true
}

// warnOfAnUnmeasuredSleep says that an engine keeps its wake reserve, though
// its pool keeps none, because what it holds asleep could not be measured. It
// is said once a sleep, when the engine goes to sleep.
func (r *ModelClaimReconciler) warnOfAnUnmeasuredSleep(
	claim *modelv1alpha1.ModelClaim,
	podName string,
	model *RuntimeSnapshotModel,
	withoutWakeReserve bool,
) {
	if _, known := sleepingFootprintOf(model); !withoutWakeReserve || known {
		return
	}
	r.Recorder.Eventf(claim, corev1.EventTypeWarning, "SleepingFootprintUnknown",
		"model %s sleeps on pod %s and keeps its wake reserve: what its engine holds asleep could not be measured",
		servedModelName(claim), podName)
}

// claimHoldsAKVLimitOn reports whether any instance recorded on this Pod runs
// under a KV limit its own ClaimReconciler maintains.
//
// A listing that fails answers yes. Standing down costs a pool its automatic
// KV distribution for one round; guessing no would write a limit that the
// health loop overwrites moments later, and neither engine would settle.
//
// The claims are read from the API server, not the cache. The policy runs at
// the end of a pass, right after the pass may have recorded the first limit on
// this Pod, and the cache may not have seen that record yet.
func (r *ModelClaimReconciler) claimHoldsAKVLimitOn(ctx context.Context, pod *corev1.Pod) bool {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	claims := &modelv1alpha1.ModelClaimList{}
	if err := reader.List(ctx, claims, client.InNamespace(pod.Namespace)); err != nil {
		klog.ErrorS(err, "pool KV policy could not list ModelClaims", "pod", klog.KObj(pod))
		return true
	}
	for i := range claims.Items {
		for _, instance := range claims.Items[i].Status.Instances {
			if instance.Pod == pod.Name && instance.KVLimitBytes > 0 {
				return true
			}
		}
	}
	return false
}

// poolActivityKey names an engine in the pool policy's record of activity: the
// pod it runs on, and the engine.
func poolActivityKey(pod *corev1.Pod, model RuntimeSnapshotModel) string {
	return string(pod.UID) + "/" + snapshotActivityKey(model)
}

func snapshotActivityKey(model RuntimeSnapshotModel) string {
	if model.IPCName != "" {
		return model.IPCName
	}
	return model.ModelName
}

func claimForRuntimeSnapshot(
	claims *modelv1alpha1.ModelClaimList,
	model RuntimeSnapshotModel,
) *modelv1alpha1.ModelClaim {
	if model.ClaimRef == nil || model.ClaimRef.UID == "" {
		return nil
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Namespace == model.ClaimRef.Namespace &&
			claim.Name == model.ClaimRef.Name && string(claim.UID) == model.ClaimRef.UID {
			return claim
		}
	}
	return nil
}

// routedSince is when a claim's engine was last routed, as its Ready condition
// says, and zero when the claim is not Ready. An engine counts as idle only from
// then. Until its route is in place no request can reach it, so an engine that
// woke well before it was routed is not idle for that time.
func routedSince(pm *modelv1alpha1.ModelClaim) time.Time {
	ready := meta.FindStatusCondition(pm.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return time.Time{}
	}
	return ready.LastTransitionTime.Time
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
		if latest.Status.Instances[i].Pod != podName {
			continue
		}
		if latest.Status.Instances[i].Phase != modelv1alpha1.ModelClaimSleeping {
			latest.Status.Instances[i].Phase = modelv1alpha1.ModelClaimSleeping
			changed = true
		}
		break
	}
	if !changed {
		return nil
	}
	r.recomputeReadiness(latest)
	setClaimGauges(latest)
	return r.Status().Update(ctx, latest)
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
