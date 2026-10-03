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
	"errors"
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
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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
	runtimePhaseStopping = "stopping"

	// ModelClaimFinalizer ensures attached engine processes are deactivated and
	// routing is deregistered before the ModelClaim object is removed.
	ModelClaimFinalizer = "model.aibrix.ai/modelclaim-finalizer"

	// ScheduledPodsAnnotationKey records the warm pods this model has been
	// bin-packed onto, mirroring the ModelAdapter scheduled-pods convention so a
	// binding survives controller restarts. N models may be recorded per pod.
	ScheduledPodsAnnotationKey = "model.aibrix.ai/scheduled-pods"

	// DefaultRequeueDuration paces periodic reconciliation (placement retries
	// and health checks).
	DefaultRequeueDuration = 10 * time.Second

	// ActivatingRequeueDuration paces a claim while an engine of it is
	// booting, so the engine is routed soon after it is ready, and not a
	// whole period later.
	ActivatingRequeueDuration = 2 * time.Second

	// ActivatingRequeueWindow is how long into a boot that faster pace lasts.
	// An engine still booting after this long is looked at every period.
	ActivatingRequeueWindow = 5 * time.Minute
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
	// PoolPolicy serializes controller-local policy ticks and retains only the
	// request-counter deltas needed for conservative KV allocation. It is not a
	// desired-state store; runtime snapshots remain authoritative after restart.
	PoolPolicy *poolPolicyManager
	// Divisions remembers when each card was last divided to follow its load,
	// so that a card is divided once per round however many claims sit on it.
	Divisions *cardDivisionState
	// Backoff spaces out the tries of claims no card in the pool can hold. A
	// model waiting for room then does not have every runtime read for it every
	// round.
	Backoff *placementBackoff
	// APIReader reads ModelClaims straight from the API server for the GPU
	// memory account, where an instance recorded moments ago and not yet in the
	// informer would read as free memory. Falls back to the cached client when
	// unset, which is how the unit tests run.
	APIReader client.Reader
	// Now is the controller's clock, for how long a wake request has waited.
	// It falls back to time.Now when unset.
	Now func() time.Time
}

func (r *ModelClaimReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
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
		Divisions:  newCardDivisionState(time.Now),
		Backoff:    newPlacementBackoff(time.Now),
		APIReader:  mgr.GetAPIReader(),
	}

	err := ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		// Passes run one at a time, which is also the default. The account of
		// a card and its division read what earlier passes wrote, so two passes
		// at once could promise the same memory twice, or divide one card twice.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		For(&modelv1alpha1.ModelClaim{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		// React to warm GPU pods coming and going so models pending placement can
		// attach as soon as an eligible pod appears.
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(enqueueModelClaimsForPod(mgr.GetClient())),
			builder.WithPredicates(modelPoolPodFilter(), notOnlyWakeRequests())).
		// A request to wake a sleeping engine concerns its claim alone.
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(enqueueRequestedWakes),
			builder.WithPredicates(modelPoolPodFilter(), wakeRequestsChanged())).
		// Wake the claims waiting for a card when another claim may have freed
		// one, rather than leave them to sleep through their wait.
		Watches(&modelv1alpha1.ModelClaim{},
			handler.EnqueueRequestsFromMapFunc(enqueueWaitingClaims(mgr.GetClient())),
			builder.WithPredicates(roomMayHaveFreed())).
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
		if apierrors.IsNotFound(err) {
			// Deleted without this controller seeing it go, as when someone
			// else removed its finalizer. Nothing is left to wait for.
			r.backoff().placed(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: deactivate attached instances, then drop the finalizer.
	if !pm.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(pm, ModelClaimFinalizer) {
			r.deactivateInstances(ctx, pm)
			clearClaimMetrics(pm.Namespace, servedModelName(pm))
			r.backoff().placed(req.NamespacedName)
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
		setClaimGauges(pm)
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

	recorded := len(pm.Status.Instances)
	pruneDeadInstances(pm, candidates)
	if len(pm.Status.Instances) < recorded {
		// A wait is for the instance that could not be placed. A claim that
		// has lost an instance needs another one, so it starts over.
		r.backoff().startOver(req.NamespacedName)
	}
	r.setStatusFields(pm, candidates)
	// Every step below reads a runtime through this, so each runtime is read
	// once in this pass unless a step changes it.
	readings := newRuntimeReadings(r.Runtime)

	// Drive the model towards its desired number of active instances by
	// bin-packing onto warm pods and asking the runtime sidecar to activate it.
	requeueAfter := DefaultRequeueDuration
	switch {
	case desiredReplicas(pm) > int32(len(pm.Status.Instances)):
		wait, err := r.ensureActivated(ctx, pm, candidates, readings)
		if err != nil {
			if apierrors.IsConflict(err) {
				// The claim was read a moment too early to be written. No engine
				// was asked for, so nothing failed.
				return requeueOnConflict(err)
			}
			r.Recorder.Event(pm, corev1.EventTypeWarning, "ActivateFailed", err.Error())
			meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
				Type:    string(modelv1alpha1.ModelClaimConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  "ActivateFailed",
				Message: err.Error(),
			})
			pm.Status.Phase = modelv1alpha1.ModelClaimFailed
			setClaimGauges(pm)
			if uerr := r.Status().Update(ctx, pm); uerr != nil {
				return requeueOnConflict(uerr)
			}
			// A card divided for an engine that then did not start has lost that
			// engine again. Divide it now, so its neighbours get back the room
			// they gave up for it. The cache may still show the record that
			// was just taken back, so the API server is asked.
			r.divideCardsAsListed(ctx, candidates, readings)
			// A start that failed is tried again as a refusal is, less and
			// less often. The pool is remembered as it stands, since a pod
			// that joins or turns ready may be able to start the engine. A
			// claim with an instance left comes back every round, to check
			// that instance's engine.
			wait := r.backoff().failedToStart(req.NamespacedName, pm.Generation,
				r.roomAsCached(ctx, pm.Namespace, candidates))
			if len(pm.Status.Instances) > 0 {
				wait = DefaultRequeueDuration
			}
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		// A claim with nothing placed has nothing else to do each round, so it
		// comes back when its wait is up. One with an instance keeps coming
		// back every round, to check that instance's engine.
		if wait > 0 && len(pm.Status.Instances) == 0 {
			requeueAfter = wait
		}
	case desiredReplicas(pm) < int32(len(pm.Status.Instances)):
		r.scaleDown(ctx, pm, desiredReplicas(pm), readings)
		r.backoff().placed(req.NamespacedName)
	default:
		// Nothing is left to place, so there is no wait to keep. An instance
		// recorded some other way, or a placement whose last step failed,
		// would otherwise leave the claim's refusals behind for good. An
		// instance whose engine failed for good is still to be replaced, and
		// its replacement keeps the wait of its last try.
		if len(failedInstanceSlots(pm)) == 0 {
			r.backoff().placed(req.NamespacedName)
		}
	}

	// Reconcile instance routability against live engine readiness (promote
	// ready Activating instances, demote Active instances that went unhealthy),
	// and wake the sleeping engines a request has asked for.
	booting, err := r.reconcileHealthAndWakes(ctx, pm, candidates, readings)
	if err != nil {
		// A move is written before its engine is touched. This one could not
		// be, so the engine is as it was, and the next pass decides again.
		return requeueOnConflict(err)
	}
	replacementFailed := false
	failed := len(failedInstanceSlots(pm))
	if err := r.rescheduleFailedInstances(ctx, pm, candidates, readings); err != nil {
		if apierrors.IsConflict(err) {
			// As above: no replacement was asked for, so nothing failed.
			return requeueOnConflict(err)
		}
		r.Recorder.Event(pm, corev1.EventTypeWarning, "RescheduleFailed", err.Error())
		replacementFailed = true
	}
	// A replacement started in this pass boots from now on. The health check
	// above ran before it, so it is looked at again as soon as a new engine is.
	if len(failedInstanceSlots(pm)) < failed {
		booting = true
	}
	r.recomputeReadiness(pm)
	if len(pm.Status.Instances) == 0 && r.backoff().waitsAfterAFailedStart(req.NamespacedName) {
		// A pass inside the wait tried nothing, so the claim still stands as
		// its last try left it.
		pm.Status.Phase = modelv1alpha1.ModelClaimFailed
	}
	setClaimGauges(pm)
	if err := r.Status().Update(ctx, pm); err != nil {
		return requeueOnConflict(err)
	}
	if replacementFailed {
		// A replacement that did not start is tried again as a first start
		// that failed is, less and less often. The wait is recorded once the
		// claim says why it waits. The claim keeps its failed instance, so it
		// still comes back every round.
		r.backoff().failedToStart(req.NamespacedName, pm.Generation,
			r.roomAsCached(ctx, pm.Namespace, candidates))
	}
	r.backoff().statusWritten(req.NamespacedName)
	// Pool policy is an optional, Deployment-scoped control loop. It runs after
	// claim status is persisted so a policy failure cannot block activation
	// or route-health convergence for this claim.
	r.reconcilePoolPolicies(ctx, candidates, readings)
	// Cards whose engines all declare what they cost are divided again by the
	// planner that placement uses. Each share then follows load, and does not
	// stay what it was when the last model landed. It runs last, after anything
	// this pass changed on the cards.
	r.divideCards(ctx, candidates, readings)
	// A booting engine is looked at again soon, so it is routed within a few
	// seconds of being ready. A pass that ended before the health check, as
	// after a start that failed, keeps its own pace.
	if booting {
		requeueAfter = min(requeueAfter, ActivatingRequeueDuration)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileHealthAndWakes checks each instance's engine against what its
// runtime reports, and then wakes the sleeping engines that a request has asked
// for. It reports whether an engine boots, so that the claim is looked at again
// soon. An error is a move that could not be written, and nothing was done to
// its engine.
func (r *ModelClaimReconciler) reconcileHealthAndWakes(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) (bool, error) {
	booting := r.reconcileInstanceHealth(ctx, pm, readings)
	// An engine woken in this pass boots from now on, so the claim is looked
	// at again as soon as a booting engine is.
	woke, err := r.wakeRequested(ctx, pm, candidates, readings)
	return booting || woke, err
}

// roomAsCached describes what the candidates carry, from the cached listing
// of the claims. Without a listing, the candidates are unlisted.
func (r *ModelClaimReconciler) roomAsCached(
	ctx context.Context,
	namespace string,
	candidates []corev1.Pod,
) roomSignature {
	cached := &modelv1alpha1.ModelClaimList{}
	if err := r.List(ctx, cached, client.InNamespace(namespace)); err != nil {
		klog.ErrorS(err, "list model claims", "namespace", namespace)
		return roomSignatureOf(candidates, nil)
	}
	return roomSignatureOf(candidates, cached)
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
	moving := 0
	waiting := 0
	for i := range pm.Status.Instances {
		inst := &pm.Status.Instances[i]
		if inst.Phase == modelv1alpha1.ModelClaimActive {
			active++
		}
		if inst.Phase == modelv1alpha1.ModelClaimSleeping {
			sleeping++
			if inst.Reason == instanceReasonWaitingForRoom {
				waiting++
			}
		}
		if inst.Phase == modelv1alpha1.ModelClaimFailed {
			failed++
			if movingReason(inst.Reason) {
				moving++
			}
		}
	}
	pm.Status.ReadyReplicas = int32(active)
	switch {
	case failed > 0 && moving == failed:
		// Failed where it was, but only because it could not wake there. The
		// claim is moved to another pod once one can take it.
		pm.Status.Phase = modelv1alpha1.ModelClaimFailed
		meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
			Type:    string(modelv1alpha1.ModelClaimConditionReady),
			Status:  metav1.ConditionFalse,
			Reason:  readyReasonMoving,
			Message: "the model could not wake on its pod, and is moving to another pod",
		})
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
	case sleeping > 0 && waiting > 0:
		pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
		meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
			Type:    string(modelv1alpha1.ModelClaimConditionReady),
			Status:  metav1.ConditionFalse,
			Reason:  readyReasonWaitingForRoom,
			Message: "a request asked for the model, and its card cannot take it back yet",
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
//
// An instance whose engine failed for good is replaced where it stands, and its
// replacement is placed as any instance is.
//
// A claim no card could hold backs off, and the duration returned is how long
// it waits before its next try. It is zero when there is nothing to wait for.
func (r *ModelClaimReconciler) ensureActivated(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) (time.Duration, error) {
	claim := types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name}
	backoff := r.backoff()
	// The cached listing is enough to count each pod's load and to tell
	// whether room may have appeared. The account is built from a fresh one.
	cached := &modelv1alpha1.ModelClaimList{}
	if err := r.List(ctx, cached, client.InNamespace(pm.Namespace)); err != nil {
		klog.ErrorS(err, "list model claims", "namespace", pm.Namespace)
		cached = nil
	}
	room := roomSignatureOf(candidates, cached)
	if due, left := backoff.due(claim, pm.Generation, room); !due {
		// No card could hold this model a moment ago, and nothing that could
		// make room has happened since. Reading every runtime in the pool
		// again changes nothing until the pool does.
		return left, nil
	}
	parallelism, err := modelParallelism(pm)
	if err != nil {
		return 0, fmt.Errorf("invalid engineConfig parallelism: %w", err)
	}

	// A claim is only placed where a card's account shows the room for it, so
	// a claim that does not say what it costs is not placed anywhere. Placed
	// without a cost, it would leave its card unaccountable to every claim
	// after it. No runtime is read for such a claim, since no reading could
	// place it.
	perGPU, err := perGPUBytesOf(pm)
	if err != nil {
		message := fmt.Sprintf("%s is not placed: %v", servedModelName(pm), err)
		if meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
			Type:    string(modelv1alpha1.ModelClaimConditionTypeScheduled),
			Status:  metav1.ConditionFalse,
			Reason:  "InvalidPerGPU",
			Message: message,
		}) {
			r.Recorder.Event(pm, corev1.EventTypeWarning, "InvalidPerGPU", message)
		}
		return 0, nil
	}
	load := podLoadFrom(cached)
	// The account and the ranking are made from the same reading of each
	// runtime. So two admitted pods are never ordered by numbers that
	// contradict the gate they just passed.
	snapshots := readings.ofPods(ctx, candidates)
	placementStates := placementStatesFrom(snapshots, candidates, pm.Spec.ArtifactURL, parallelism)
	claims, listErr := r.listClaimsForAccount(ctx, pm.Namespace)
	ledgers := podLedgersFrom(claims, listErr, candidates, snapshots)
	admissible, refusals := admissibleCandidates(candidates, ledgers, perGPU.minimumReserveBytes(), instanceGPUCount(pm))
	rankByRoom(placementStates, ledgers)

	// Every pod the claim is on stays out of the ranking, the pods where its
	// engines failed included, so a replacement never lands where the engine
	// it replaces failed. The set is taken once: a replaced instance leaves the
	// claim's list, and its pod has to stay out for the next replacement too.
	alreadyOn := instancePods(pm)
	failed := failedInstanceSlots(pm)
	// A failed engine is stopped once per pass, however many pods its
	// replacement has to try.
	stopped := map[string]bool{}
	for desiredReplicas(pm) > int32(len(pm.Status.Instances)-len(failed)) {
		pod, selectErr := selectPodForActivationWithState(
			admissible, alreadyOn, load, servedModelName(pm), r.Locality, placementStates,
		)
		if selectErr != nil {
			// No available warm pod right now; remain Pending and try again
			// once the wait is up. The refusal is raised as an Event only when
			// it changes, as InvalidPerGPU is. The same refusal on each try is
			// not news; the condition always carries the current one.
			//
			// A failed instance that cannot be replaced stays as it is, so the
			// claim stays Failed.
			reason := "NoMatchingPods"
			message := noPlacementMessage(selectErr, admissible, refusals, perGPU.minimumReserveBytes())
			// A model bigger than every card would otherwise read as one that
			// waits for room, and nobody would learn it can never be placed.
			if largest, never := tooLargeForEveryCard(candidates, ledgers, perGPU.minimumReserveBytes()); never &&
				len(admissible) == 0 {
				reason = "TooLargeForAnyCard"
				message = fmt.Sprintf("no candidate pod can hold this model, which needs %s on a card; "+
					"the best of them offers %s on a card", gibibytes(perGPU.minimumReserveBytes()), gibibytes(largest))
			}
			eventReason := reason
			if len(failed) > 0 {
				from := pm.Status.Instances[failed[0]]
				if movingReason(from.Reason) {
					message = fmt.Sprintf("model %s cannot move from pod %s, where it could not wake: %s",
						servedModelName(pm), from.Pod, message)
				} else {
					message = fmt.Sprintf("model %s cannot move from failed pod %s: %s",
						servedModelName(pm), from.Pod, message)
				}
				eventReason = "ReschedulePending"
			}
			if meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
				Type:    string(modelv1alpha1.ModelClaimConditionTypeScheduled),
				Status:  metav1.ConditionFalse,
				Reason:  reason,
				Message: message,
			}) {
				r.Recorder.Event(pm, corev1.EventTypeWarning, eventReason, message)
			}
			// It still waits with backoff: a larger pod may join, or the
			// claim's declaration may shrink, and either wakes it.
			// The account was built from the claims as the API server has
			// them, so the room the claim was refused on is remembered as that
			// listing describes it. A cache a moment behind could miss an
			// instance recorded just before, whose leaving would then wake
			// nobody.
			refusedOn := room
			if listErr == nil {
				refusedOn = roomSignatureOf(candidates, claims)
			}
			if reason == "TooLargeForAnyCard" {
				return backoff.refusedAsTooLarge(claim, pm.Generation, refusedOn), nil
			}
			// The API server shows less on a card than the cache did when this
			// try began. The cache had not caught up with a change, most often
			// a neighbour that has just gone, whose engine may still be
			// exiting. The event of its going will find the pool as it is
			// remembered here, and wake nobody. So the claim starts over from
			// the shortest wait. A record that was taken back after a start
			// that failed does not show here, since a record without a port is
			// not counted.
			if roomMayHaveAppeared(room, refusedOn, anyRoom) {
				backoff.startOver(claim)
			}
			return backoff.refused(claim, pm.Generation, refusedOn), nil
		}

		// Divide the card between the engines on it and this one, and hold
		// every engine already there to its new share before this engine has a
		// chance to start. Until that is done and confirmed, the room this
		// model was admitted against is still the neighbours' to take.
		//
		// On a pod without a card nothing is divided, so no limit is recorded:
		// a recorded limit always means that a card was divided for it.
		kvLimitBytes := int64(0)
		if podHasGPUs(*pod, ledgers[pod.Name].accelerators) {
			planned, roomErr := r.makeRoomOnPod(ctx, pm, perGPU, pod, ledgers[pod.Name], readings)
			if roomErr != nil {
				// The card had room for this model and could not be divided to
				// make it, most often because an engine on it did not take its new
				// limit. That says nothing about the other cards, so try the next
				// one rather than give up on the claim for this round.
				r.Recorder.Event(pm, corev1.EventTypeWarning, "KVLimitFailed", fmt.Sprintf(
					"%s could not be held to its share of %s: %v", servedModelName(pm), pod.Name, roomErr))
				refusals = append(refusals, podRefusal{
					pod:       pod.Name,
					roomBytes: ledgers[pod.Name].maximumRoomBytes(),
					known:     true,
					couldHold: true,
					reason:    fmt.Sprintf("%s has room, but its card could not be divided: %v", pod.Name, roomErr),
				})
				admissible = withoutPod(admissible, pod.Name)
				continue
			}
			kvLimitBytes = planned
			// The card is now divided for this engine too, so the next pass must
			// not take its arrival for a change to divide the card for again.
			r.divisions().divided(cardOf(pod), cardComposition(claims, pod.Name,
				compositionEntry(pm, modelv1alpha1.ModelClaimActivating)))
		}

		// Record the instance before the engine exists. The record is what the
		// account reads, so writing it first is what stops a second claim from
		// being placed against the same memory while this engine loads. It also
		// carries the KV limit the engine will be held to.
		//
		// A replacement takes the place of the failed instance. Its engine is
		// stopped and its route taken back first; the other engines on that
		// pod are left as they are.
		record := modelv1alpha1.ModelClaimInstance{
			Pod:          pod.Name,
			Phase:        modelv1alpha1.ModelClaimActivating,
			KVLimitBytes: kvLimitBytes,
		}
		slot := len(pm.Status.Instances)
		var replaced *modelv1alpha1.ModelClaimInstance
		if len(failed) > 0 {
			slot = failed[0]
			previous := pm.Status.Instances[slot]
			replaced = &previous
			if !stopped[previous.Pod] {
				r.stopFailedEngine(ctx, pm, previous.Pod, readings)
				stopped[previous.Pod] = true
			}
			pm.Status.Instances[slot] = record
		} else {
			pm.Status.Instances = append(pm.Status.Instances, record)
		}
		if err := r.Status().Update(ctx, pm); err != nil {
			return 0, fmt.Errorf("reserve %s on %s: %w", servedModelName(pm), pod.Name, err)
		}

		resp, aerr := r.Runtime.Activate(ctx, pod.Status.PodIP, DefaultRuntimePort, activateRequest(pm))
		readings.forget(pod.Name)
		if errors.Is(aerr, errRuntimeSilent) {
			// The runtime is left alone for now, so the call was not sent and
			// nothing failed to start. The record is taken back, as for any
			// start known not to have happened, and this pod is passed over for
			// the rest of the pass, as a card that could not be divided is.
			// Ranking cannot tell such a pod from the others, so without this
			// the same pod could be picked on every pass.
			if replaced != nil {
				pm.Status.Instances[slot] = *replaced
			} else {
				pm.Status.Instances = pm.Status.Instances[:slot]
			}
			// The shorter list is written at once. Until then, an account
			// read from the API server would charge the card for a start
			// that was never asked for.
			if err := r.Status().Update(ctx, pm); err != nil {
				return 0, fmt.Errorf("take back %s on %s: %w", servedModelName(pm), pod.Name, err)
			}
			refusals = append(refusals, podRefusal{
				pod:       pod.Name,
				roomBytes: ledgers[pod.Name].maximumRoomBytes(),
				known:     true,
				couldHold: true,
				reason: fmt.Sprintf("%s cannot be asked to start %s yet: %v",
					pod.Name, servedModelName(pm), aerr),
			})
			admissible = withoutPod(admissible, pod.Name)
			continue
		}
		if aerr != nil {
			recordActivation(pm.Namespace, servedModelName(pm), false)
			// The record was written first to guard against a crash between
			// these two steps, where it would be all that remained. A start
			// known not to have happened is undone here, so the card is given
			// back: the caller's status update persists the shorter list. The
			// next pass ranks the pods again, and may ask the same one.
			//
			// After any other failure the engine may have started, and only
			// the answer was lost. The record then stays, and the claim is
			// placed. Taken back, the record would leave that engine answering
			// to no claim, and its card out of use. The health check settles
			// it: it goes on with the engine if the runtime has one, and starts
			// it again if not.
			//
			// A replacement known not to have started puts the failed instance
			// back, so the claim still reads as failed, and its pod stays out of
			// the next pass's ranking.
			switch {
			case !callNotDone(aerr):
				markPlaced(pm, pod)
			case replaced != nil:
				pm.Status.Instances[slot] = *replaced
			default:
				pm.Status.Instances = pm.Status.Instances[:slot]
			}
			if replaced != nil {
				return 0, fmt.Errorf("activate replacement on pod %s: %w", pod.Name, aerr)
			}
			return 0, aerr
		}
		// The engine was asked for, and the runtime did not refuse. That is
		// what Placed says, so it is said now, and the claim waits no more: a
		// later step that fails here leaves the instance recorded, and the next
		// pass places nothing again.
		markPlaced(pm, pod)
		backoff.placed(claim)
		pm.Status.Instances[slot].Port = resp.Port

		// The engine is spawned but not yet serveable (boot/compile). Keep the
		// model NOT routable — stamp the non-routable marker (port 0), record the
		// instance as Activating with its real port — until reconcileInstanceHealth
		// confirms the engine is ready, then it flips the annotation to the real
		// port. This means the gateway never routes to a still-booting engine.
		if err := r.annotateWarmPod(ctx, pm, pod, 0); err != nil {
			return 0, err
		}

		alreadyOn[pod.Name] = true
		load[pod.Name]++
		if replaced != nil {
			failed = failed[1:]
			r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Rescheduled",
				"model %s moved %s from pod %s to pod %s",
				servedModelName(pm), whyMoved(replaced.Reason), replaced.Pod, pod.Name)
			continue
		}
		r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Activating",
			"model %s engine starting on pod %s:%d", servedModelName(pm), pod.Name, resp.Port)
	}
	return 0, nil
}

// rescheduleFailedInstances moves only instances whose runtime has reported a
// terminal engine failure, in the same pass that found them. ensureActivated
// replaces each one, so a replacement goes through the same account, division
// and record as any placement. The failed pod remains excluded from placement,
// while other claims and engines on that pod are left untouched.
func (r *ModelClaimReconciler) rescheduleFailedInstances(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) error {
	if len(failedInstanceSlots(pm)) == 0 {
		return nil
	}
	_, err := r.ensureActivated(ctx, pm, candidates, readings)
	return err
}

// failedInstanceSlots returns the positions of the instances whose engine
// failed for good, in order.
func failedInstanceSlots(pm *modelv1alpha1.ModelClaim) []int {
	var slots []int
	for i := range pm.Status.Instances {
		if pm.Status.Instances[i].Phase == modelv1alpha1.ModelClaimFailed {
			slots = append(slots, i)
		}
	}
	return slots
}

// stopFailedEngine takes back this claim's route on a pod and stops its engine
// there. The engine has already failed for good. Co-resident engines on that
// pod remain untouched.
func (r *ModelClaimReconciler) stopFailedEngine(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	podName string,
	readings *runtimeReadings,
) {
	r.deannotateWarmPod(ctx, pm.Namespace, podName, pm.Name)
	if ip := r.podIP(ctx, pm.Namespace, podName); ip != "" {
		if err := r.Runtime.Deactivate(ctx, ip, DefaultRuntimePort, &DeactivateRequest{
			ModelName: servedModelName(pm),
			Mode:      DeactivateStop,
		}); err != nil {
			klog.ErrorS(err, "failed engine cleanup before reschedule",
				"pod", podName, "model", pm.Name)
		}
		readings.forget(podName)
	}
}

// markPlaced says on the Scheduled condition that a claim found a card. Being
// turned away is an ordinary step rather than a dead end, so a refusal that has
// since been resolved must not be left standing as the claim's answer to
// whether it found one.
func markPlaced(pm *modelv1alpha1.ModelClaim, pod *corev1.Pod) {
	meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
		Type:    string(modelv1alpha1.ModelClaimConditionTypeScheduled),
		Status:  metav1.ConditionTrue,
		Reason:  "Placed",
		Message: fmt.Sprintf("placed on pod %s", pod.Name),
	})
}

// activateRequest is what the runtime is asked to start for a claim.
func activateRequest(pm *modelv1alpha1.ModelClaim) *ActivateRequest {
	return &ActivateRequest{
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
	}
}

// makeRoomOnPod divides a card between the engines on it and the one about to
// join them, and returns the KV limit the newcomer is to run under.
//
// Returning an error means this model is not placed on this card this round. A
// division that failed at its shrinks takes them back, as far as the runtime
// takes the writes. One that failed at its records leaves its shrinks in force.
// A neighbour that keeps a smaller limit loses room until the card is divided
// again, and correctness loses nothing.
func (r *ModelClaimReconciler) makeRoomOnPod(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	perGPU perGPUBytes,
	pod *corev1.Pod,
	ledger podLedger,
	readings *runtimeReadings,
) (int64, error) {
	newcomer := engineOnPod{
		claimName:       pm.Name,
		modelName:       servedModelName(pm),
		perGPUBytes:     perGPU,
		kvCapacityBytes: kvLimitUnknown,
	}
	engines := append(append([]engineOnPod(nil), ledger.engines...), newcomer)
	limits, err := r.arrangeCard(ctx, pod, ledger, engines, placementDivision, readings)
	var incomplete growthIncompleteError
	switch {
	case errors.As(err, &incomplete):
		// The room is made and the model recorded; the neighbours not yet
		// grown are below their records, and the round grows them.
		klog.ErrorS(err, "placed a model, and could not grow every engine beside it", "pod", klog.KObj(pod))
	case err != nil:
		return 0, err
	}
	for _, limit := range limits {
		if limit.claimName == pm.Name {
			return limit.kvLimitBytes, nil
		}
	}
	return 0, fmt.Errorf("%s was left out of the plan for %s", pm.Name, pod.Name)
}

// growthIncompleteError says that a card's room was made and every new limit
// recorded, but not every engine could be grown into its share. Those engines
// sit below their records, which is safe and keeps their routes, and the next
// round grows them.
type growthIncompleteError struct{ err error }

func (e growthIncompleteError) Error() string {
	return "not every engine could be grown into its share: " + e.err.Error()
}

func (e growthIncompleteError) Unwrap() error { return e.err }

// cardLeftAlone says that a round found no need to divide a card, or nothing
// worth writing, so nothing was tried. It is not a division that worked. It
// names the engines that are short of KV and that the plan would have given
// more, which the card's next round asks about.
type cardLeftAlone struct{ owed []string }

func (cardLeftAlone) Error() string { return "the card needs no division" }

// arrangeCard plans one card and carries the plan out, returning the plan.
//
// The work is done in an order that never leaves two engines entitled to the
// same byte, and never leaves an engine held to more than its record. The
// limits that shrink an engine are written first, and a fresh reading has to
// confirm them before anything else happens. A lower limit evicts nothing, so
// the room a shrink makes is not there until the engine is seen inside its new
// limit. Every new limit is then recorded on its own claim, the ones that go
// down first. The limits that grow an engine are written last, and read back in
// the same way. A write that reached no segment is reported as a success either
// way.
//
// A shrink that fails leaves every record as it was. The engines it wrote are
// then held to what they were held to before, and to no more than their
// records. That goes as far as the runtime takes the writes. A record that
// cannot be written leaves records that come to no more than the card, and the
// shrinks stay in force. A grow that fails comes after the records, so the
// engines it did not reach sit below their new records, which is safe and keeps
// their routes. The arrangement is made, and the error says only that some
// engine is still to grow.
func (r *ModelClaimReconciler) arrangeCard(
	ctx context.Context,
	pod *corev1.Pod,
	ledger podLedger,
	engines []engineOnPod,
	why division,
	readings *runtimeReadings,
) ([]plannedKVLimit, error) {
	limits, err := planKVLimits(ledger.hbmUsableBytes, engines)
	if err != nil {
		return nil, err
	}
	needed, owed := needsDividing(engines, limits, why.minimumChangeBytes, why.owedBefore)
	if why.onlyWhenNeeded && !needed {
		return limits, cardLeftAlone{owed: owed}
	}
	if why.minimumChangeBytes > 0 && !worthWriting(limits, why.minimumChangeBytes) {
		return limits, cardLeftAlone{owed: owed}
	}

	shrinks, grows := shrinksAndGrows(limits)
	if written, err := r.writeAndConfirmKVLimits(ctx, pod, ledger, shrinks, readings); err != nil {
		r.takeBackKVLimits(ctx, pod, ledger, written)
		readings.forget(pod.Name)
		return nil, err
	}

	// Every limit is recorded, not only the written ones. An engine that has
	// no KV segment yet is held to its record once it builds one.
	//
	// The records that go down are written before the ones that go up. A
	// record is what an engine is raised to when it next comes up, so records
	// written part way must not come to more than the card.
	held := make(map[string]*modelv1alpha1.ModelClaim, len(limits))
	for _, lowering := range []bool{true, false} {
		for _, limit := range limits {
			if limit.lowersRecord() != lowering {
				continue
			}
			claim, err := r.recordKVLimit(ctx, pod.Namespace, limit.claimName, pod.Name, limit.kvLimitBytes)
			if err != nil {
				return nil, fmt.Errorf("record %s at %s: %w", limit.claimName, gibibytes(limit.kvLimitBytes), err)
			}
			held[limit.claimName] = claim
		}
	}

	var growErr error
	if _, err := r.writeAndConfirmKVLimits(ctx, pod, ledger, grows, readings); err != nil {
		growErr = growthIncompleteError{err: err}
		grows = nil
	}
	written := append(append([]plannedKVLimit(nil), shrinks...), grows...)
	if !why.announce {
		if len(written) > 0 {
			klog.V(2).InfoS("divided a card again", "pod", klog.KObj(pod),
				"engines", len(limits), "moved", len(written))
		}
		return limits, growErr
	}
	// Say so on each claim whose engine was moved. A limit written by the
	// arrangement of a card is a limit its owner did not ask for, and looking
	// at the claim is the first thing anyone does when a model's KV changes
	// under it.
	for _, limit := range written {
		claim := held[limit.claimName]
		if claim == nil {
			continue
		}
		r.Recorder.Eventf(claim, corev1.EventTypeNormal, "KVLimitSet",
			"model %s on pod %s: KV limit set to %s, from %s, dividing the card between %d engine(s)",
			limit.modelName, pod.Name, gibibytes(limit.kvLimitBytes), gibibytes(limit.kvCapacityBytes),
			len(limits))
	}
	return limits, growErr
}

// writeAndConfirmKVLimits writes one step of a card's division and reads the
// card back to confirm it. A step with nothing to write reads nothing. The
// reading taken to confirm the step becomes the pass's reading of the card. It
// returns the limits it wrote. When a write failed, these are only a part of
// the step.
func (r *ModelClaimReconciler) writeAndConfirmKVLimits(
	ctx context.Context,
	pod *corev1.Pod,
	ledger podLedger,
	limits []plannedKVLimit,
	readings *runtimeReadings,
) ([]plannedKVLimit, error) {
	if len(limits) == 0 {
		return nil, nil
	}
	for i, limit := range limits {
		// The moment the card was read is part of the operation, not only the
		// value. The runtime runs each operation once, and an engine that
		// restarted needs the same value written again. Without the moment,
		// that second write is taken for the first one, and never reaches the
		// segment. The card would stay a round behind for good.
		operationID := fmt.Sprintf("kv-plan/%s/%s/%s/%d/%d",
			pod.Namespace, pod.UID, limit.claimName, limit.kvLimitBytes,
			ledger.observedAt.UnixNano())
		if _, err := r.Runtime.SetKVLimit(ctx, pod.Status.PodIP, DefaultRuntimePort, &SetKVLimitRequest{
			ModelName:   limit.modelName,
			LimitBytes:  limit.kvLimitBytes,
			OperationID: operationID,
		}); err != nil {
			readings.forget(pod.Name)
			return limits[:i], fmt.Errorf("set %s to %s: %w", limit.modelName, gibibytes(limit.kvLimitBytes), err)
		}
	}
	readings.forget(pod.Name)
	snapshot, err := r.Runtime.Snapshot(ctx, pod.Status.PodIP, DefaultRuntimePort)
	if err != nil {
		return limits, fmt.Errorf("read back the limits on %s: %w", pod.Name, err)
	}
	if snapshot != nil {
		readings.replace(pod, snapshot)
	}
	return limits, confirmKVLimits(snapshot, limits)
}

// takeBackKVLimits holds each engine that a shrink step wrote to what it was
// held to before, and to no more than its record.
//
// A shrink that is not confirmed changes no record, and it must not leave an
// engine held to less than before either. A division that fails is tried again
// by the next round. One that kept failing would shrink the engines beside the
// one at fault again, round after round. An engine not routed yet would be
// shrunk by each division and raised by its own pass in turn, and would never
// be found holding its record.
//
// The shrinks are the first step of a division, so nothing has grown yet, and
// raising them again gives no engine room that another was given. Only what
// the step wrote is taken back. The first call that fails ends it, since a
// runtime that does not answer would hold the worker once for each engine.
//
// It is a best effort, and it is not read back. An engine it does not reach
// stays held to what the shrink wrote, which is less than it was held to
// before.
func (r *ModelClaimReconciler) takeBackKVLimits(
	ctx context.Context,
	pod *corev1.Pod,
	ledger podLedger,
	written []plannedKVLimit,
) {
	for _, limit := range written {
		before := limit.kvCapacityBytes
		if limit.kvRecordedBytes > 0 && limit.kvRecordedBytes < before {
			before = limit.kvRecordedBytes
		}
		if before <= limit.kvLimitBytes {
			continue
		}
		operationID := fmt.Sprintf("kv-plan-back/%s/%s/%s/%d/%d",
			pod.Namespace, pod.UID, limit.claimName, before, ledger.observedAt.UnixNano())
		if _, err := r.Runtime.SetKVLimit(ctx, pod.Status.PodIP, DefaultRuntimePort, &SetKVLimitRequest{
			ModelName:   limit.modelName,
			LimitBytes:  before,
			OperationID: operationID,
		}); err != nil {
			klog.ErrorS(err, "could not take a KV limit back", "pod", klog.KObj(pod), "model", limit.modelName)
			return
		}
	}
}

// recordKVLimit writes the limit an instance is to run under into its own
// claim's status, which is where every loop that holds an engine reads it, and
// returns the claim so an Event can be raised on it afterwards.
//
// A claim with no instance on this pod is the model being placed: its record
// is written with the rest of its instance, once the card has been arranged.
//
// The claim is read from the API server rather than the cache, and the write
// is tried again on a conflict. The claim's own reconcile may have written its
// status moments before, and a copy from a cache that has not caught up would
// only fail the division on a conflict, leaving the card to the next pass.
func (r *ModelClaimReconciler) recordKVLimit(
	ctx context.Context,
	namespace, claimName, podName string,
	kvLimitBytes int64,
) (*modelv1alpha1.ModelClaim, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var claim *modelv1alpha1.ModelClaim
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &modelv1alpha1.ModelClaim{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claimName}, fresh); err != nil {
			return err
		}
		claim = fresh
		changed := false
		for i := range fresh.Status.Instances {
			instance := &fresh.Status.Instances[i]
			if instance.Pod == podName && instance.KVLimitBytes != kvLimitBytes {
				instance.KVLimitBytes = kvLimitBytes
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return r.Status().Update(ctx, fresh)
	})
	if err != nil {
		return nil, err
	}
	return claim, nil
}

// placementStatesFrom summarises each candidate's reading for ranking. A pod
// whose runtime did not answer has no state, and ranks as unknown.
func placementStatesFrom(
	snapshots map[string]*RuntimeSnapshot,
	candidates []corev1.Pod,
	artifactURL string,
	parallelism int64,
) map[string]PodPlacementState {
	states := make(map[string]PodPlacementState, len(candidates))
	for i := range candidates {
		snapshot, found := snapshots[candidates[i].Name]
		if !found {
			continue
		}
		states[candidates[i].Name] = placementStateFromSnapshot(snapshot, artifactURL, parallelism)
	}
	return states
}

// reconcileInstanceHealth reconciles routing from fresh runtime snapshot data.
// Snapshot state is authoritative for the engine port and readiness: the
// controller never promotes an engine merely because its old status entry was
// Active. A snapshot failure leaves the last known routing in place rather
// than guessing that a live engine has disappeared. It reports whether it left
// an instance Activating on an engine that is booting.
//
// An engine that is taken off the route needs no such pace. Its pod's
// annotation changes then. On a pod that carries both labels of its pool, that
// change starts the next pass at once.
func (r *ModelClaimReconciler) reconcileInstanceHealth(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	readings *runtimeReadings,
) (booting bool) {
	served := servedModelName(pm)
	dropped := map[string]bool{}
	for i := range pm.Status.Instances {
		inst := &pm.Status.Instances[i]
		if inst.Phase != modelv1alpha1.ModelClaimActivating &&
			inst.Phase != modelv1alpha1.ModelClaimActive &&
			inst.Phase != modelv1alpha1.ModelClaimSleeping &&
			inst.Phase != modelv1alpha1.ModelClaimFailed {
			continue
		}
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: inst.Pod}, pod); err != nil ||
			pod.Status.PodIP == "" {
			continue // pod gone; pruneDeadInstances will drop it
		}
		ip := pod.Status.PodIP
		snapshot, err := readings.of(ctx, pod)
		if err != nil {
			klog.V(4).InfoS("runtime snapshot failed", "model", pm.Name, "pod", inst.Pod, "phase", inst.Phase, "err", err)
			continue
		}
		observed := snapshotModelForClaim(snapshot, pm, served)

		if engineMissing(inst, snapshot, observed) {
			stays, started := r.startMissingEngine(ctx, pm, inst, ip)
			dropped[inst.Pod] = !stays
			// An engine started again boots from now on, and the runtime may
			// not date its boot yet. It is looked at again soon, as a
			// replacement is.
			booting = booting || started
			readings.forget(pod.Name)
			continue
		}

		state := r.judgeEngine(ctx, pm, inst, snapshot, observed)
		// Pull an engine down to its record whenever it is held to more. Raise
		// it to its record only before it has been routed: an instance is
		// recorded after its card was divided to make the room, so that room is
		// its own. A larger record for an engine already serving comes from a
		// division that has not been carried out yet, and growing the engine
		// here, outside that division's order, could hand it memory a
		// neighbour has not given back.
		if state.serving && !state.limitInForce &&
			(!state.limitWithinRecord || inst.Phase != modelv1alpha1.ModelClaimActive) {
			state, observed = r.holdToKVLimit(ctx, pm, inst, pod, snapshot, observed, state, readings)
		}
		booting = booting || state.booting
		desiredPhase, routingPort := state.phase, state.routingPort
		serving := state.serving

		reason := ""
		if desiredPhase == inst.Phase {
			reason = bindingReason(inst)
		}
		if err := r.annotateWarmPodWithState(
			ctx, pm, pod, routingPort, routingStateForPhase(desiredPhase), reason,
		); err != nil {
			klog.ErrorS(err, "routability annotation failed", "model", pm.Name, "pod", inst.Pod, "ready", desiredPhase == modelv1alpha1.ModelClaimActive)
			continue
		}
		inst.Port = state.observedPort
		previousPhase := inst.Phase
		if previousPhase == desiredPhase {
			continue
		}
		inst.Phase = desiredPhase
		// A reason belongs to the phase it was given in.
		inst.Reason = ""
		r.announcePhase(pm, inst, previousPhase, observed, serving)
	}
	if r.dropInstances(ctx, pm, dropped) > 0 {
		// The claim needs another instance now, so its wait starts over.
		r.backoff().startOver(types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name})
	}
	return booting
}

// announcePhase raises the Event for an instance that has just changed phase.
func (r *ModelClaimReconciler) announcePhase(
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	previousPhase modelv1alpha1.ModelClaimPhase,
	observed *RuntimeSnapshotModel,
	serving bool,
) {
	served := servedModelName(pm)
	switch inst.Phase {
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
		switch {
		case previousPhase == modelv1alpha1.ModelClaimActive && serving:
			// Still serving, so it is the limit and not the engine that went
			// wrong. Saying "no longer ready" would send an operator to look at
			// a healthy process.
			r.Recorder.Eventf(pm, corev1.EventTypeWarning, "KVLimitNotHeld",
				"model %s on pod %s is held to more KV than its limit of %s; marked non-routable until the limit is written again",
				served, inst.Pod, gibibytes(inst.KVLimitBytes))
		case serving:
			// An engine that woke and waits for its limit is ready. The Event
			// of the limit says what it waits for.
		case previousPhase != modelv1alpha1.ModelClaimActivating:
			r.Recorder.Eventf(pm, corev1.EventTypeWarning, "Unhealthy",
				"model %s no longer ready on pod %s; marked non-routable", served, inst.Pod)
		}
	}
}

// holdToKVLimit writes the limit an instance records, and returns how the
// engine stands after it.
//
// An engine that is not on the route is read back at once, and judged again
// from that reading. That is an engine coming up, and one that slept and
// serves again. Held to its limit now, it is routed in this pass rather than
// the next. The engine of a failed instance is read back as well, and its
// instance stays failed. An engine that was routed and lost its limit is not
// read back: it leaves the route first, so that the loss is seen. Its Event
// says that the limit was written, which is all that is known of it in this
// pass.
//
// What the pass had read of the runtime is dropped, whether the write was
// taken or not. A write changes the runtime, or may have. A read-back that
// fails is kept as the reading of the pass, since a runtime that did not
// answer is not asked again in the same pass. The later steps of that pass
// pay for it. The division finds the card unread, and leaves it to its next
// round. The pool policy passes over the pod, and counts a failed evaluation.
func (r *ModelClaimReconciler) holdToKVLimit(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
	snapshot *RuntimeSnapshot,
	observed *RuntimeSnapshotModel,
	state engineState,
	readings *runtimeReadings,
) (engineState, *RuntimeSnapshotModel) {
	served := servedModelName(pm)
	written := r.writeKVLimit(ctx, pm, inst, pod, pod.Status.PodIP, snapshot, observed)
	readings.forget(pod.Name)
	if !written {
		return state, observed
	}
	if inst.Phase == modelv1alpha1.ModelClaimActive {
		r.Recorder.Eventf(pm, corev1.EventTypeNormal, "KVLimitSet",
			"model %s on pod %s: KV limit of %s written over %s",
			served, inst.Pod, gibibytes(inst.KVLimitBytes), gibibytes(observed.KVCapacityBytes))
		return state, observed
	}
	confirmed, err := readings.of(ctx, pod)
	if err != nil {
		klog.V(4).InfoS("runtime snapshot failed after a KV limit was written",
			"model", pm.Name, "pod", inst.Pod, "err", err)
		return state, observed
	}
	writtenOver := observed.KVCapacityBytes
	observed = snapshotModelForClaim(confirmed, pm, served)
	r.reportKVLimit(pm, inst, writtenOver, observed)
	return r.judgeEngine(ctx, pm, inst, confirmed, observed), observed
}

// reportKVLimit says what the reading taken after a write shows of the limit
// that was written over writtenOverBytes. A reading with no engine in it, or
// no segment, shows neither that the limit is in force nor that it is not.
func (r *ModelClaimReconciler) reportKVLimit(
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	writtenOverBytes int64,
	observed *RuntimeSnapshotModel,
) {
	served := servedModelName(pm)
	switch {
	case observed == nil || observed.KVCapacityBytes < 0:
	case kvLimitInForce(inst, observed):
		r.Recorder.Eventf(pm, corev1.EventTypeNormal, "KVLimitSet",
			"model %s on pod %s: KV limit set to %s, from %s",
			served, inst.Pod, gibibytes(inst.KVLimitBytes), gibibytes(writtenOverBytes))
	default:
		r.Recorder.Eventf(pm, corev1.EventTypeWarning, "KVLimitFailed",
			"model %s on pod %s: KV limit %s was written, and the engine still reports %s",
			served, inst.Pod, gibibytes(inst.KVLimitBytes), gibibytes(observed.KVCapacityBytes))
	}
}

// judgeKVLimit says whether an engine is held to the limit its instance
// records, and whether it is held to no more than that.
//
// A limit not in force is about to be written, or the engine taken off its
// route. The record may have just been changed by a division in another
// claim's pass, before the cache caught up. Acting on a stale record would pull
// an engine back from a share it was just given, or grow it into one it just
// gave up. So in that case the record is read fresh first, and the instance
// takes it.
func (r *ModelClaimReconciler) judgeKVLimit(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	observed *RuntimeSnapshotModel,
	serving bool,
) (inForce, withinRecord bool) {
	inForce = kvLimitInForce(inst, observed)
	if serving && !inForce {
		if fresh, found := r.freshKVLimitRecord(ctx, pm, inst.Pod); found && fresh != inst.KVLimitBytes {
			inst.KVLimitBytes = fresh
			inForce = kvLimitInForce(inst, observed)
		}
	}
	return inForce, kvLimitWithinRecord(inst, observed)
}

// freshKVLimitRecord reads the limit a claim's instance on a pod records
// straight from the API server, around the cache.
func (r *ModelClaimReconciler) freshKVLimitRecord(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	pod string,
) (int64, bool) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	fresh := &modelv1alpha1.ModelClaim{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(pm), fresh); err != nil {
		klog.V(4).InfoS("could not read a claim's record fresh", "model", pm.Name, "err", err)
		return 0, false
	}
	for _, instance := range fresh.Status.Instances {
		if instance.Pod == pod {
			return instance.KVLimitBytes, true
		}
	}
	return 0, false
}

// engineState is what one reading of a runtime says about an instance's
// engine, and so where the instance should stand.
type engineState struct {
	observedPort      int32
	serving           bool
	limitInForce      bool
	limitWithinRecord bool
	phase             modelv1alpha1.ModelClaimPhase
	routingPort       int32
	// booting is set for an instance left Activating while its engine boots,
	// which is worth looking at again soon.
	booting bool
}

// judgeEngine reads an instance's engine from one reading of its runtime.
//
// An engine on a GPU becomes routable only once it holds the KV limit this
// instance records. Until then it runs under its allocator's own default,
// which is most of the card, and traffic would let it grow that far. It stays
// routable only while it is held to no more than that record.
func (r *ModelClaimReconciler) judgeEngine(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	snapshot *RuntimeSnapshot,
	observed *RuntimeSnapshotModel,
) engineState {
	state := engineState{observedPort: inst.Port}
	if observed != nil {
		state.observedPort = observed.Port
	}
	state.serving = observed != nil && observed.Ready && state.observedPort > 0
	state.limitInForce, state.limitWithinRecord = r.judgeKVLimit(ctx, pm, inst, observed, state.serving)
	state.phase, state.routingPort = desiredInstanceState(
		inst, observed, state.observedPort, state.serving, state.limitInForce, state.limitWithinRecord)
	state.booting = state.phase == modelv1alpha1.ModelClaimActivating && engineBooting(snapshot, observed)
	return state
}

// engineBooting reports whether a runtime reading shows an engine booting:
// alive but not yet ready, and for less than ActivatingRequeueWindow. The
// time is taken on the runtime's clock, which also dates the boot, so the
// controller's clock does not have to agree with it. An engine that is ready
// but not yet routable is not booting. Neither is one whose boot the runtime
// does not date, since nothing would then bound it. An engine that is
// stopping counts while the runtime reports it alive. Its instance gets a new
// engine as soon as it has gone.
//
// An engine whose stop fails does not count. The runtime dates such an engine
// again at every try. It would then read as one that has just begun to stop,
// for as long as the stop fails. It carries the error of the stop.
func engineBooting(snapshot *RuntimeSnapshot, observed *RuntimeSnapshotModel) bool {
	if snapshot == nil || observed == nil || !observed.Alive || observed.Ready ||
		observed.LastTransition == nil || snapshot.ObservedAt.IsZero() {
		return false
	}
	if observed.Phase == runtimePhaseStopping && observed.LastError != "" {
		return false
	}
	// A boot dated after the reading is not timed at all: the runtime's
	// clock was set back, and the pace would last until it caught up.
	age := snapshot.ObservedAt.Sub(*observed.LastTransition)
	return age >= 0 && age < ActivatingRequeueWindow
}

// engineMissing reports whether an activating instance has no engine behind
// it, going by a runtime that answered.
//
// An instance is recorded before its engine is started, so that the account
// charges it from the start. If the controller stopped between the two, or a
// failed start was never taken back from the record, the runtime knows no
// engine for the instance. Nothing else would start one, since the claim has
// its instance and placement does not run again, while the account goes on
// charging the card for it.
func engineMissing(inst *modelv1alpha1.ModelClaimInstance, snapshot *RuntimeSnapshot, observed *RuntimeSnapshotModel) bool {
	return inst.Phase == modelv1alpha1.ModelClaimActivating && snapshot != nil && observed == nil
}

// startMissingEngine asks the runtime to start the engine an activating
// instance should have. It reports whether the instance is to stay, and
// whether the runtime started the engine. The runtime starts a model once and
// returns the running one after that, so asking again is safe.
func (r *ModelClaimReconciler) startMissingEngine(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	podIP string,
) (stays, started bool) {
	served := servedModelName(pm)
	resp, err := r.Runtime.Activate(ctx, podIP, DefaultRuntimePort, activateRequest(pm))
	if errors.Is(err, errRuntimeSilent) {
		// The runtime is left alone for now, so the call was not sent and
		// nothing failed to start. The instance stays as it is, as it does
		// when its runtime cannot be read, and a later pass asks again.
		klog.V(4).InfoS("engine not started again yet", "model", pm.Name, "pod", inst.Pod, "err", err)
		return true, false
	}
	if err != nil {
		recordActivation(pm.Namespace, served, false)
		r.Recorder.Eventf(pm, corev1.EventTypeWarning, "ActivateFailed",
			"model %s had no engine on pod %s, and starting one failed: %v", served, inst.Pod, err)
		// Unless the start is known not to have happened, the engine may be
		// there, so the instance stays, and the next pass looks again.
		return !callNotDone(err), false
	}
	inst.Port = resp.Port
	r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Activating",
		"model %s had no engine on pod %s; engine starting again on port %d", served, inst.Pod, resp.Port)
	return true, true
}

// dropInstances removes the instances on the given pods from a claim, and
// takes their routing annotations back, which gives their cards back. It
// returns how many it removed.
//
// The caller's status update persists the shorter list, and the next pass
// places the claim again. Should that update be lost, the next pass finds the
// same instance with no engine and tries again.
func (r *ModelClaimReconciler) dropInstances(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	dropped map[string]bool,
) int {
	recorded := len(pm.Status.Instances)
	kept := pm.Status.Instances[:0]
	for _, inst := range pm.Status.Instances {
		if dropped[inst.Pod] {
			r.deannotateWarmPod(ctx, pm.Namespace, inst.Pod, pm.Name)
			continue
		}
		kept = append(kept, inst)
	}
	pm.Status.Instances = kept
	return recorded - len(kept)
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

// annotateWarmPod records the active or activating served-model binding for
// this ModelClaim. Lifecycle reconciliation uses annotateWarmPodWithState for
// sleeping and failed observations.
func (r *ModelClaimReconciler) annotateWarmPod(ctx context.Context, pm *modelv1alpha1.ModelClaim, pod *corev1.Pod, port int32) error {
	state := constants.ModelClaimRoutingStateActive
	if port == 0 {
		state = constants.ModelClaimRoutingStateActivating
	}
	return r.annotateWarmPodWithState(ctx, pm, pod, port, state, "")
}

func (r *ModelClaimReconciler) annotateWarmPodWithState(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	port int32,
	state string,
	reason string,
) error {
	key := constants.ModelClaimPodAnnotationPrefix + pm.Name
	// wakeByRequest tells the gateway that this controller wakes the engine:
	// a request for it while it sleeps is written on the pod, and the
	// controller decides when its card can take it.
	value := fmt.Sprintf(`{"model":%q,"port":%d,"state":%q,"wakeByRequest":true}`, servedModelName(pm), port, state)
	// A reason says more than the state, so the gateway can tell its client
	// how long to wait.
	if reason != "" {
		value = fmt.Sprintf(`{"model":%q,"port":%d,"state":%q,"wakeByRequest":true,"reason":%q}`,
			servedModelName(pm), port, state, reason)
	}
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

// desiredInstanceState is how one instance should be routed, given what the
// runtime just reported about it. An engine is routable only once it is ready,
// has a port, and is held to the KV limit its instance records, and it stays
// routable only while it is held to no more than that.
func desiredInstanceState(
	inst *modelv1alpha1.ModelClaimInstance,
	observed *RuntimeSnapshotModel,
	observedPort int32,
	serving bool,
	limitInForce bool,
	limitWithinRecord bool,
) (modelv1alpha1.ModelClaimPhase, int32) {
	switch {
	case inst.Phase == modelv1alpha1.ModelClaimFailed:
		return modelv1alpha1.ModelClaimFailed, 0
	case observed != nil && observed.Phase == runtimePhaseFailed:
		return modelv1alpha1.ModelClaimFailed, 0
	case observed != nil && observed.Phase == runtimePhaseSleeping:
		return modelv1alpha1.ModelClaimSleeping, 0
	case serving && limitInForce:
		return modelv1alpha1.ModelClaimActive, observedPort
	case serving && inst.Phase == modelv1alpha1.ModelClaimActive && limitWithinRecord:
		// An engine already serving keeps its route while a larger limit
		// recorded for it has not been written: it is held to less than it was
		// given, not more. Held to more than its record, as when a restart puts
		// its allocator's default back, it loses the route until the record is
		// written again.
		return modelv1alpha1.ModelClaimActive, observedPort
	}
	return modelv1alpha1.ModelClaimActivating, 0
}

// kvLimitInForce reports whether the engine is already held to the limit this
// instance records. An instance records a limit only when a card was divided
// for it, so with no record there is nothing to hold the engine to. That is an
// instance placed before its claim declared a per-GPU cost, or one on a pod
// without a card.
//
// Only the record is asked, not what this reading says of the pod's cards. A
// reading can miss a card, and the engine has its limit to hold all the same.
func kvLimitInForce(inst *modelv1alpha1.ModelClaimInstance, observed *RuntimeSnapshotModel) bool {
	if inst.KVLimitBytes <= 0 {
		return true
	}
	return observed != nil && observed.KVCapacityBytes == inst.KVLimitBytes
}

// kvLimitWithinRecord reports whether the engine is held to no more than the
// limit this instance records, which is what keeps a serving engine routable.
// An engine whose segment cannot be read is not known to be held to anything.
func kvLimitWithinRecord(inst *modelv1alpha1.ModelClaimInstance, observed *RuntimeSnapshotModel) bool {
	if inst.KVLimitBytes <= 0 {
		return true
	}
	return observed != nil && observed.KVCapacityBytes >= 0 && observed.KVCapacityBytes <= inst.KVLimitBytes
}

// writeKVLimit asks the runtime to hold this engine to the limit the instance
// records, and reports whether the runtime took the request. The caller says
// that the limit is set, once it knows.
//
// The runtime runs each operation ID once, and an engine that restarts needs
// the same value written again, so the ID carries the moment the snapshot was
// taken as well as the value itself.
//
// A reported success is not proof. The CLI the runtime drives exits zero when
// the segment does not exist, so the only evidence that a limit is in force is
// reading it back from a later snapshot, which is what the caller does: at once
// for an engine that is not on the route, and on the next pass otherwise.
func (r *ModelClaimReconciler) writeKVLimit(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
	podIP string,
	snapshot *RuntimeSnapshot,
	observed *RuntimeSnapshotModel,
) bool {
	// A write into a segment that does not exist is lost without a word, and
	// the engine overwrites the segment when it builds one anyway.
	if observed == nil || observed.KVCapacityBytes < 0 {
		return false
	}
	served := servedModelName(pm)
	operationID := fmt.Sprintf("kv-limit/%s/%s/%s/%d/%d",
		pm.Namespace, pm.Name, pod.UID, inst.KVLimitBytes, snapshot.ObservedAt.UnixNano())
	if _, err := r.Runtime.SetKVLimit(ctx, podIP, DefaultRuntimePort, &SetKVLimitRequest{
		ModelName:   served,
		LimitBytes:  inst.KVLimitBytes,
		OperationID: operationID,
	}); err != nil {
		klog.ErrorS(err, "could not hold an engine to its KV limit",
			"model", pm.Name, "pod", inst.Pod, "limit", inst.KVLimitBytes)
		r.Recorder.Eventf(pm, corev1.EventTypeWarning, "KVLimitFailed",
			"model %s on pod %s: KV limit %s could not be set: %v",
			served, inst.Pod, gibibytes(inst.KVLimitBytes), err)
		return false
	}
	return true
}

func routingStateForPhase(phase modelv1alpha1.ModelClaimPhase) string {
	switch phase {
	case modelv1alpha1.ModelClaimActive:
		return constants.ModelClaimRoutingStateActive
	case modelv1alpha1.ModelClaimSleeping:
		return constants.ModelClaimRoutingStateSleeping
	case modelv1alpha1.ModelClaimFailed:
		return constants.ModelClaimRoutingStateFailed
	default:
		return constants.ModelClaimRoutingStateActivating
	}
}

// deannotateWarmPod removes this ModelClaim's routing annotation from a warm
// pod (best-effort), used on deactivation/deletion so the gateway stops routing.
func (r *ModelClaimReconciler) deannotateWarmPod(ctx context.Context, namespace, podName, pmName string) {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		return // pod already gone
	}
	key := constants.ModelClaimPodAnnotationPrefix + pmName
	// A wake request for the claim goes with its route.
	wakeKey := constants.ModelClaimWakeAnnotationPrefix + pmName
	_, routed := pod.Annotations[key]
	_, asked := pod.Annotations[wakeKey]
	if !routed && !asked {
		return
	}
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Annotations, key)
	delete(pod.Annotations, wakeKey)
	if err := r.Patch(ctx, pod, patch); err != nil {
		klog.ErrorS(err, "failed to remove model-claim routing annotation", "pod", podName, "model", pmName)
	}
}

// scaleDown deactivates surplus instances until the desired count is met.
func (r *ModelClaimReconciler) scaleDown(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	target int32,
	readings *runtimeReadings,
) {
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
			readings.forget(inst.Pod)
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

// podLoadFrom tallies how many model instances each warm pod currently hosts,
// across all ModelClaims in the namespace, for least-loaded bin-packing. With
// no listing, every pod counts as empty.
func podLoadFrom(list *modelv1alpha1.ModelClaimList) map[string]int {
	load := map[string]int{}
	if list == nil {
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
		// A pod that joins may bring room, so the claims are tried oldest first.
		oldestFirst(list.Items)
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
