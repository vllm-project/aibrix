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

// Package modelclaim implements the controller for the ModelClaim CRD: a model
// runtime claim that attaches to a selected GPU pod and is served as its own
// engine process through the aibrix-runtime sidecar. Multiple claims may share
// a GPU through kvcached.
package modelclaim

import (
	"context"
	"fmt"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	controllerName = "model-claim-controller"

	runtimePhaseActive   = "active"
	runtimePhaseFailed   = "failed"
	runtimePhaseSleeping = "sleeping"

	// ModelClaimFinalizer ensures attached engine processes are deactivated and
	// routing is deregistered before the ModelClaim object is removed.
	ModelClaimFinalizer = "model.aibrix.ai/modelclaim-finalizer"

	// ScheduledPodsAnnotationKey records the warm pods this model has been
	// bin-packed onto, mirroring the ModelAdapter scheduled-pods convention so a
	// binding survives controller restarts. N models may be recorded per pod.
	ScheduledPodsAnnotationKey = "model.aibrix.ai/scheduled-pods"

	// DefaultRequeueDuration paces periodic reconciliation (placement retries
	// and readiness checks).
	DefaultRequeueDuration = 10 * time.Second
)

// ModelClaimReconciler reconciles a ModelClaim object.
type ModelClaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Runtime drives the per-runtime sidecar (activate/deactivate). Injected so
	// the reconcile loop is testable with an in-process fake.
	Runtime RuntimeClient
	// Locality scores how cheap it is to load a model's weights on a node (the
	// Layer 2 node weight-cache signal). Phase 1 uses uniformLocality, so
	// placement is load-only until node state reporting is added back.
	Locality LocalityProvider
	// SnapshotCache stores bounded runtime observations for placement. It is
	// process-local so a leader restart naturally rehydrates from sidecars.
	SnapshotCache *runtimeSnapshotCache
	// PoolPolicy serializes controller-local policy ticks and retains only the
	// request-counter deltas needed for conservative KV allocation. It is not a
	// desired-state store; runtime snapshots remain authoritative after restart.
	PoolPolicy *poolPolicyManager
}

// Add creates a new ModelClaim controller and registers it with the Manager.
func Add(mgr manager.Manager, _ config.RuntimeConfig) error {
	r := &ModelClaimReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorderFor(controllerName),
		Runtime:    NewRuntimeClient(),
		Locality:   uniformLocality{},
		PoolPolicy: newPoolPolicyManager(time.Now),
		SnapshotCache: newRuntimeSnapshotCache(
			defaultRuntimeSnapshotTTL, time.Now,
		),
	}

	err := ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&modelv1alpha1.ModelClaim{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		// React to warm GPU pods coming and going so models pending placement can
		// attach as soon as an eligible pod appears.
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(enqueueModelClaimsForPod(mgr.GetClient())),
			builder.WithPredicates(modelPoolPodFilter())).
		Complete(r)
	if err != nil {
		return err
	}

	klog.InfoS("Finished to add model-claim-controller")
	return nil
}

//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modelclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modelclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modelclaims/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments;replicasets,verbs=get;list;watch

// Reconcile drives a ModelClaim towards its desired state: select a warm pod,
// activate a runtime engine, hold routing at port 0 until ready, and stop the
// engine on scale-down or deletion.
func (r *ModelClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pm := &modelv1alpha1.ModelClaim{}
	if err := r.Get(ctx, req.NamespacedName, pm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: deactivate attached instances, then drop the finalizer.
	if !pm.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(pm, ModelClaimFinalizer) {
			r.deactivateInstances(ctx, pm)
			clearClaimMetrics(pm.Namespace, servedModelName(pm))
			controllerutil.RemoveFinalizer(pm, ModelClaimFinalizer)
			if err := r.Update(ctx, pm); err != nil {
				return requeueOnConflict(err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present before doing any external-effecting work.
	if !controllerutil.ContainsFinalizer(pm, ModelClaimFinalizer) {
		controllerutil.AddFinalizer(pm, ModelClaimFinalizer)
		if err := r.Update(ctx, pm); err != nil {
			return requeueOnConflict(err)
		}
		// A finalizer-only update changes neither generation, labels, nor
		// annotations, so our watch predicate (GenerationChanged / LabelChanged /
		// AnnotationChanged) filters the resulting event out and would NOT
		// re-enqueue. Requeue explicitly so reconciliation proceeds to placement
		// and activation instead of stalling until an unrelated event arrives.
		return ctrl.Result{Requeue: true}, nil
	}

	if _, err := modelParallelism(pm); err != nil {
		message := fmt.Sprintf("invalid engineConfig parallelism: %v", err)
		r.Recorder.Event(pm, corev1.EventTypeWarning, "InvalidEngineConfig", message)
		meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
			Type:    string(modelv1alpha1.ModelClaimConditionReady),
			Status:  metav1.ConditionFalse,
			Reason:  "InvalidEngineConfig",
			Message: message,
		})
		pm.Status.Phase = modelv1alpha1.ModelClaimFailed
		if uerr := r.Status().Update(ctx, pm); uerr != nil {
			return requeueOnConflict(uerr)
		}
		return ctrl.Result{}, nil
	}
	candidates, err := r.listCandidateWarmPods(ctx, pm)
	if err != nil {
		klog.ErrorS(err, "failed to list candidate warm pods", "modelClaim", req.NamespacedName)
		return ctrl.Result{}, err
	}

	pruneDeadInstances(pm, candidates)
	r.setStatusFields(pm, candidates)

	// Drive the model towards its desired number of active instances by
	// bin-packing onto warm pods and asking the runtime sidecar to activate it.
	switch {
	case desiredReplicas(pm) > int32(len(pm.Status.Instances)):
		if err := r.ensureActivated(ctx, pm, candidates); err != nil {
			r.Recorder.Event(pm, corev1.EventTypeWarning, "ActivateFailed", err.Error())
			meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
				Type:    string(modelv1alpha1.ModelClaimConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "ActivateFailed",
				Message: err.Error(),
			})
			pm.Status.Phase = modelv1alpha1.ModelClaimFailed
			if uerr := r.Status().Update(ctx, pm); uerr != nil {
				return requeueOnConflict(uerr)
			}
			return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, nil
		}
	case desiredReplicas(pm) < int32(len(pm.Status.Instances)):
		r.scaleDown(ctx, pm, desiredReplicas(pm))
	}

	// Reconcile instance routability against live engine readiness (promote
	// ready Activating instances, demote Active instances that went unhealthy).
	r.reconcileInstanceHealth(ctx, pm)
	r.recomputeReadiness(pm)
	setClaimGauges(pm)
	if err := r.Status().Update(ctx, pm); err != nil {
		return requeueOnConflict(err)
	}
	// Pool policy is an optional, Deployment-scoped control loop. It runs after
	// claim status is persisted so a policy failure cannot block activation
	// or route-health convergence for this claim.
	r.reconcilePoolPolicies(ctx, candidates)
	return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, nil
}

// requeueOnConflict lets the next reconcile work from the latest API object.
// Status updates can race with deletion/finalizer updates and must not surface
// as a controller error when Kubernetes reports the expected resource-version
// conflict.
func requeueOnConflict(err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, err
}

// listCandidateWarmPods returns running pods that match the model's PodSelector
// and advertise themselves as warm GPU pool members accepting attachments.
func (r *ModelClaimReconciler) listCandidateWarmPods(ctx context.Context, pm *modelv1alpha1.ModelClaim) ([]corev1.Pod, error) {
	if pm.Spec.PodSelector == nil {
		return nil, fmt.Errorf("spec.podSelector must not be nil")
	}
	parallelism, err := modelParallelism(pm)
	if err != nil {
		return nil, fmt.Errorf("invalid engineConfig parallelism: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(pm.Spec.PodSelector)
	if err != nil {
		return nil, err
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(pm.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return nil, err
	}

	candidates := make([]corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pod := podList.Items[i]
		if pod.Labels[constants.ModelPoolLabelEnabled] != constants.ModelPoolLabelEnabledValue {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if pod.Status.PodIP == "" {
			continue
		}
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if isVLLMModel(pm) && !podSupportsVLLMParallelism(pod, parallelism) {
			continue
		}
		candidates = append(candidates, pod)
	}
	return candidates, nil
}

// pruneDeadInstances drops status instances whose warm pod is no longer a live
// candidate (deleted or replaced). Without this, a recreated warm pod would
// never be re-activated: desired == len(stale instances), so the model silently
// stops being served after pod churn. The pod is gone, so there is no runtime
// to deactivate and no annotation left to clean; dropping the record is the
// reconcile-correct move; the normal activation path then re-places the model
// (and, with the node weight cache, prefers the node already holding weights).
func pruneDeadInstances(pm *modelv1alpha1.ModelClaim, candidates []corev1.Pod) {
	alive := make(map[string]bool, len(candidates))
	for i := range candidates {
		alive[candidates[i].Name] = true
	}
	kept := pm.Status.Instances[:0]
	for _, inst := range pm.Status.Instances {
		if alive[inst.Pod] {
			kept = append(kept, inst)
		}
	}
	pm.Status.Instances = kept
}

// setStatusFields refreshes candidate/desired counts and the Initialized
// condition. It only mutates the in-memory object; the caller persists once.
func (r *ModelClaimReconciler) setStatusFields(pm *modelv1alpha1.ModelClaim, candidates []corev1.Pod) {
	pm.Status.Candidates = int32(len(candidates))
	pm.Status.DesiredReplicas = desiredReplicas(pm)
	meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
		Type:    string(modelv1alpha1.ModelClaimConditionTypeInitialized),
		Status:  metav1.ConditionTrue,
		Reason:  "ModelClaimInitialized",
		Message: "ModelClaim has entered the reconciliation loop",
	})
}

// recomputeReadiness derives ReadyReplicas, the high-level phase, and the Ready
// condition from the current instance set.
func (r *ModelClaimReconciler) recomputeReadiness(pm *modelv1alpha1.ModelClaim) {
	// Only instances whose engine is serveable (Active) count as ready. An
	// Activating instance has a spawned-but-not-serveable engine, and its
	// warm-pod annotation is still the non-routable marker (port 0), so it is NOT
	// routable and must not inflate ReadyReplicas.
	active := 0
	sleeping := 0
	failed := 0
	for i := range pm.Status.Instances {
		if pm.Status.Instances[i].Phase == modelv1alpha1.ModelClaimActive {
			active++
		}
		if pm.Status.Instances[i].Phase == modelv1alpha1.ModelClaimSleeping {
			sleeping++
		}
		if pm.Status.Instances[i].Phase == modelv1alpha1.ModelClaimFailed {
			failed++
		}
	}
	pm.Status.ReadyReplicas = int32(active)
	switch {
	case failed > 0:
		pm.Status.Phase = modelv1alpha1.ModelClaimFailed
		meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
			Type:    string(modelv1alpha1.ModelClaimConditionReady),
			Status:  metav1.ConditionFalse,
			Reason:  "EngineFailed",
			Message: "one or more model engine instances exhausted local restart attempts",
		})
	case len(pm.Status.Instances) > 0 && active == len(pm.Status.Instances):
		pm.Status.Phase = modelv1alpha1.ModelClaimActive
		meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
			Type:    string(modelv1alpha1.ModelClaimConditionReady),
			Status:  metav1.ConditionTrue,
			Reason:  "ModelClaimActive",
			Message: "model is active on at least one warm pod",
		})
	case sleeping > 0:
		pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
		meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
			Type:    string(modelv1alpha1.ModelClaimConditionReady),
			Status:  metav1.ConditionFalse,
			Reason:  "EngineSleeping",
			Message: "one or more model engine instances are sleeping and non-routable",
		})
	case len(pm.Status.Instances) > 0:
		// Engine(s) spawned but at least one is still booting/compiling. The
		// model is not yet routable.
		pm.Status.Phase = modelv1alpha1.ModelClaimActivating
		meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
			Type:    string(modelv1alpha1.ModelClaimConditionReady),
			Status:  metav1.ConditionFalse,
			Reason:  "EngineStarting",
			Message: "engine(s) spawned, waiting for readiness probe",
		})
	default:
		pm.Status.Phase = modelv1alpha1.ModelClaimPending
	}
}

// ensureActivated brings the model up to its desired replica count by selecting
// warm pods and asking the runtime sidecar to activate an engine process on
// each. Lack of an available warm pod is not an error (the model stays Pending and
// reconciles again); only runtime failures propagate.
func (r *ModelClaimReconciler) ensureActivated(ctx context.Context, pm *modelv1alpha1.ModelClaim, candidates []corev1.Pod) error {
	load := r.computePodLoad(ctx, pm.Namespace)
	parallelism, err := modelParallelism(pm)
	if err != nil {
		return fmt.Errorf("invalid engineConfig parallelism: %w", err)
	}
	placementStates := r.collectPlacementStates(ctx, candidates, pm.Spec.ArtifactURL, parallelism)

	for desiredReplicas(pm) > int32(len(pm.Status.Instances)) {
		pod, selectErr := selectPodForActivationWithState(
			candidates, instancePods(pm), load, servedModelName(pm), r.Locality, placementStates,
		)
		if selectErr != nil {
			// No available warm pod right now; remain Pending and retry on requeue.
			r.Recorder.Event(pm, corev1.EventTypeWarning, "NoMatchingPods", selectErr.Error())
			meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
				Type:    string(modelv1alpha1.ModelClaimConditionTypeScheduled),
				Status:  metav1.ConditionFalse,
				Reason:  "NoMatchingPods",
				Message: selectErr.Error(),
			})
			return nil
		}

		resp, aerr := r.Runtime.Activate(ctx, pod.Status.PodIP, DefaultRuntimePort, &ActivateRequest{
			ModelName:    servedModelName(pm),
			ArtifactURL:  pm.Spec.ArtifactURL,
			Engine:       pm.Spec.Engine,
			IPCName:      ipcNameFor(pm),
			EngineConfig: pm.Spec.EngineConfig,
			ClaimRef: &ModelClaimRef{
				Namespace: pm.Namespace,
				Name:      pm.Name,
				UID:       string(pm.UID),
			},
		})
		if aerr != nil {
			recordActivation(pm.Namespace, servedModelName(pm), false)
			return aerr
		}

		// The engine is spawned but not yet serveable (boot/compile). Keep the
		// model NOT routable — stamp the non-routable marker (port 0), record the
		// instance as Activating with its real port — until reconcileInstanceHealth
		// confirms the engine is ready, then it flips the annotation to the real
		// port. This means the gateway never routes to a still-booting engine.
		if err := r.annotateWarmPod(ctx, pm, pod, 0); err != nil {
			return err
		}

		pm.Status.Instances = append(pm.Status.Instances, modelv1alpha1.ModelClaimInstance{
			Pod:   pod.Name,
			Port:  resp.Port,
			Phase: modelv1alpha1.ModelClaimActivating,
		})
		load[pod.Name]++
		r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Activating",
			"model %s engine starting on pod %s:%d", servedModelName(pm), pod.Name, resp.Port)
	}
	return nil
}

func (r *ModelClaimReconciler) collectPlacementStates(
	ctx context.Context,
	candidates []corev1.Pod,
	artifactURL string,
	parallelism int64,
) map[string]PodPlacementState {
	states := make(map[string]PodPlacementState, len(candidates))
	if r.SnapshotCache == nil {
		return states
	}
	for i := range candidates {
		pod := &candidates[i]
		key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		snapshot, ok := r.SnapshotCache.Get(key, pod.UID, func() (*RuntimeSnapshot, error) {
			return r.Runtime.Snapshot(ctx, pod.Status.PodIP, DefaultRuntimePort)
		})
		if !ok {
			continue
		}
		state := placementStateFromSnapshot(snapshot, artifactURL, parallelism)
		states[pod.Name] = state
	}
	return states
}

// reconcileInstanceHealth reconciles routing from fresh runtime snapshot data.
// Snapshot state is authoritative for the engine port and readiness: the
// controller never promotes an engine merely because its old status entry was
// Active. A snapshot failure leaves the last known routing in place rather
// than guessing that a live engine has disappeared.
func (r *ModelClaimReconciler) reconcileInstanceHealth(ctx context.Context, pm *modelv1alpha1.ModelClaim) {
	served := servedModelName(pm)
	for i := range pm.Status.Instances {
		inst := &pm.Status.Instances[i]
		if inst.Phase != modelv1alpha1.ModelClaimActivating &&
			inst.Phase != modelv1alpha1.ModelClaimActive &&
			inst.Phase != modelv1alpha1.ModelClaimSleeping &&
			inst.Phase != modelv1alpha1.ModelClaimFailed {
			continue
		}
		ip := r.podIP(ctx, pm.Namespace, inst.Pod)
		if ip == "" {
			continue // pod gone; pruneDeadInstances will drop it
		}
		snapshot, err := r.Runtime.Snapshot(ctx, ip, DefaultRuntimePort)
		if err != nil {
			klog.V(4).InfoS("runtime snapshot failed", "model", pm.Name, "pod", inst.Pod, "phase", inst.Phase, "err", err)
			continue
		}
		observed := snapshotModelForClaim(snapshot, pm, served)
		observedPort := inst.Port
		if observed != nil {
			observedPort = observed.Port
		}

		desiredPhase := modelv1alpha1.ModelClaimActivating
		routingPort := int32(0)
		switch {
		case inst.Phase == modelv1alpha1.ModelClaimFailed:
			desiredPhase = modelv1alpha1.ModelClaimFailed
		case observed != nil && observed.Phase == runtimePhaseFailed:
			desiredPhase = modelv1alpha1.ModelClaimFailed
		case observed != nil && observed.Phase == runtimePhaseSleeping:
			desiredPhase = modelv1alpha1.ModelClaimSleeping
		case observed != nil && observed.Ready && observedPort > 0:
			desiredPhase = modelv1alpha1.ModelClaimActive
			routingPort = observedPort
		}

		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: inst.Pod}, pod); err != nil {
			continue
		}
		if err := r.annotateWarmPod(ctx, pm, pod, routingPort); err != nil {
			klog.ErrorS(err, "routability annotation failed", "model", pm.Name, "pod", inst.Pod, "ready", desiredPhase == modelv1alpha1.ModelClaimActive)
			continue
		}
		inst.Port = observedPort
		previousPhase := inst.Phase
		if previousPhase == desiredPhase {
			continue
		}
		inst.Phase = desiredPhase
		switch desiredPhase {
		case modelv1alpha1.ModelClaimFailed:
			message := "runtime reported terminal engine failure"
			if observed != nil && observed.LastError != "" {
				message = observed.LastError
			}
			r.Recorder.Eventf(pm, corev1.EventTypeWarning, "EngineFailed",
				"model %s failed on pod %s: %s", served, inst.Pod, message)
		case modelv1alpha1.ModelClaimActive:
			if previousPhase == modelv1alpha1.ModelClaimActivating {
				recordActivation(pm.Namespace, served, true)
				r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Activated",
					"model %s ready and routable on pod %s:%d", served, inst.Pod, inst.Port)
			} else {
				r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Woken",
					"model %s woke and is routable on pod %s:%d", served, inst.Pod, inst.Port)
			}
		case modelv1alpha1.ModelClaimSleeping:
			r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Sleeping",
				"model %s is sleeping on pod %s and marked non-routable", served, inst.Pod)
		case modelv1alpha1.ModelClaimActivating:
			if previousPhase != modelv1alpha1.ModelClaimActivating {
				r.Recorder.Eventf(pm, corev1.EventTypeWarning, "Unhealthy",
					"model %s no longer ready on pod %s; marked non-routable", served, inst.Pod)
			}
		}
	}
}

// snapshotModelForClaim resolves runtime state by ClaimRef UID when the
// sidecar provides one. The served-name fallback keeps old runtime images
// interoperable while avoiding a match to a snapshot explicitly owned by a
// different ModelClaim.
func snapshotModelForClaim(snapshot *RuntimeSnapshot, pm *modelv1alpha1.ModelClaim, served string) *RuntimeSnapshotModel {
	if snapshot == nil {
		return nil
	}
	claimUID := string(pm.UID)
	var legacy *RuntimeSnapshotModel
	for i := range snapshot.Models {
		model := &snapshot.Models[i]
		if claimUID != "" && model.ClaimRef != nil && model.ClaimRef.UID != "" {
			if model.ClaimRef.UID == claimUID {
				return model
			}
			continue
		}
		if model.ModelName == served {
			legacy = model
		}
	}
	return legacy
}

// annotateWarmPod records the served-model -> port binding for this ModelClaim
// on the warm pod (key modelclaim.aibrix.ai/<name>), which the gateway cache
// reads to route the served model. One key per ModelClaim avoids races between
// the controllers of different models sharing the pod.
func (r *ModelClaimReconciler) annotateWarmPod(ctx context.Context, pm *modelv1alpha1.ModelClaim, pod *corev1.Pod, port int32) error {
	key := constants.ModelClaimPodAnnotationPrefix + pm.Name
	value := fmt.Sprintf(`{"model":%q,"port":%d}`, servedModelName(pm), port)
	if pod.Annotations[key] == value {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[key] = value
	return r.Patch(ctx, pod, patch)
}

// deannotateWarmPod removes this ModelClaim's routing annotation from a warm
// pod (best-effort), used on deactivation/deletion so the gateway stops routing.
func (r *ModelClaimReconciler) deannotateWarmPod(ctx context.Context, namespace, podName, pmName string) {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		return // pod already gone
	}
	key := constants.ModelClaimPodAnnotationPrefix + pmName
	if _, ok := pod.Annotations[key]; !ok {
		return
	}
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Annotations, key)
	if err := r.Patch(ctx, pod, patch); err != nil {
		klog.ErrorS(err, "failed to remove model-claim routing annotation", "pod", podName, "model", pmName)
	}
}

// scaleDown deactivates surplus instances until the desired count is met.
func (r *ModelClaimReconciler) scaleDown(ctx context.Context, pm *modelv1alpha1.ModelClaim, target int32) {
	for int32(len(pm.Status.Instances)) > target && len(pm.Status.Instances) > 0 {
		idx := len(pm.Status.Instances) - 1
		inst := pm.Status.Instances[idx]
		r.deannotateWarmPod(ctx, pm.Namespace, inst.Pod, pm.Name)
		if ip := r.podIP(ctx, pm.Namespace, inst.Pod); ip != "" {
			if err := r.Runtime.Deactivate(ctx, ip, DefaultRuntimePort, &DeactivateRequest{
				ModelName: servedModelName(pm),
				Mode:      DeactivateStop,
			}); err != nil {
				klog.ErrorS(err, "scale-down deactivate failed", "pod", inst.Pod, "model", pm.Name)
			}
		}
		pm.Status.Instances = pm.Status.Instances[:idx]
	}
}

// deactivateInstances best-effort stops every engine instance of the model,
// used on deletion before the finalizer is removed.
func (r *ModelClaimReconciler) deactivateInstances(ctx context.Context, pm *modelv1alpha1.ModelClaim) {
	for _, inst := range pm.Status.Instances {
		r.deannotateWarmPod(ctx, pm.Namespace, inst.Pod, pm.Name)
		ip := r.podIP(ctx, pm.Namespace, inst.Pod)
		if ip == "" {
			continue // pod already gone; nothing to stop
		}
		if err := r.Runtime.Deactivate(ctx, ip, DefaultRuntimePort, &DeactivateRequest{
			ModelName: servedModelName(pm),
			Mode:      DeactivateStop,
		}); err != nil {
			klog.ErrorS(err, "deactivate on delete failed", "pod", inst.Pod, "model", pm.Name)
		}
	}
}

// computePodLoad tallies how many model instances each warm pod currently hosts,
// across all ModelClaims in the namespace, for least-loaded bin-packing.
func (r *ModelClaimReconciler) computePodLoad(ctx context.Context, namespace string) map[string]int {
	load := map[string]int{}
	list := &modelv1alpha1.ModelClaimList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		klog.ErrorS(err, "compute pod load: list model claims", "namespace", namespace)
		return load
	}
	for i := range list.Items {
		for _, inst := range list.Items[i].Status.Instances {
			load[inst.Pod]++
		}
	}
	return load
}

// podIP resolves a pod's IP by name, returning "" if the pod is missing or has
// no IP yet.
func (r *ModelClaimReconciler) podIP(ctx context.Context, namespace, name string) string {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod); err != nil {
		return ""
	}
	return pod.Status.PodIP
}

// instancePods returns the set of pod names this model is already attached to.
func instancePods(pm *modelv1alpha1.ModelClaim) map[string]bool {
	pods := make(map[string]bool, len(pm.Status.Instances))
	for _, inst := range pm.Status.Instances {
		pods[inst.Pod] = true
	}
	return pods
}

// modelPoolPodFilter restricts pod events to GPU pool members so the
// controller only reacts to pods that can host ModelClaims.
func modelPoolPodFilter() predicate.Predicate {
	isModelPoolPod := func(labels map[string]string) bool {
		if labels == nil {
			return false
		}
		if _, ok := labels[constants.ModelPoolLabelName]; !ok {
			return false
		}
		return labels[constants.ModelPoolLabelEnabled] == constants.ModelPoolLabelEnabledValue
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isModelPoolPod(e.Object.GetLabels()) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isModelPoolPod(e.ObjectNew.GetLabels()) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return isModelPoolPod(e.Object.GetLabels()) },
		GenericFunc: func(e event.GenericEvent) bool { return isModelPoolPod(e.Object.GetLabels()) },
	}
}

// enqueueModelClaimsForPod re-reconciles every ModelClaim in a pod's namespace
// when warm-pool membership changes. A pod add may let a pending model attach; a
// pod delete may strand instances that must be rescheduled.
func enqueueModelClaimsForPod(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		list := &modelv1alpha1.ModelClaimList{}
		if err := c.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
			klog.ErrorS(err, "unable to list model claims in namespace", "namespace", obj.GetNamespace())
			return nil
		}
		requests := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: list.Items[i].Namespace,
					Name:      list.Items[i].Name,
				},
			})
		}
		return requests
	}
}
