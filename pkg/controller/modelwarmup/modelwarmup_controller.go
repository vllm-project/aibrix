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

package modelwarmup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
)

const (
	controllerName = "model-warmup-controller"

	// WarmupLabelKey labels owned Jobs with the UID of their ModelWarmup.
	WarmupLabelKey = "model.aibrix.ai/warmup"
	// WarmupNameAnnotationKey records the human-readable ModelWarmup name.
	WarmupNameAnnotationKey = "model.aibrix.ai/warmup-name"
	// RevisionLabelKey labels owned Jobs with the immutable warmup workload revision.
	RevisionLabelKey = "model.aibrix.ai/revision"
	// TargetNodeAnnotationKey records the exact node targeted by an owned Job.
	TargetNodeAnnotationKey = "model.aibrix.ai/target-node"
	// TargetNodeUIDAnnotationKey records the Kubernetes identity of the target Node.
	TargetNodeUIDAnnotationKey = "model.aibrix.ai/target-node-uid"
	// AttemptAnnotationKey records the one-based Continuous Job attempt.
	AttemptAnnotationKey = "model.aibrix.ai/attempt"

	// WarmupEnabledLabelKey opts a Node into ModelWarmup scheduling when its
	// value is WarmupEnabledLabelValue.
	WarmupEnabledLabelKey   = "model.aibrix.ai/warmup-enabled"
	WarmupEnabledLabelValue = "true"
	// ResourcePoolLabelKey authorizes a Node for ModelWarmups in Namespaces
	// carrying the same non-empty label value.
	ResourcePoolLabelKey = "resource-pool.aibrix.ai/name"
)

var controlPlaneLabelKeys = []string{
	"node-role.kubernetes.io/control-plane",
	"node-role.kubernetes.io/master",
}

type ModelWarmupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func Add(mgr manager.Manager, _ config.RuntimeConfig) error {
	return ctrl.NewControllerManagedBy(mgr).Named(controllerName).
		For(&modelv1alpha1.ModelWarmup{}, builder.WithPredicates()).
		Owns(&batchv1.Job{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(enqueueActiveModelWarmups(mgr.GetClient())),
			builder.WithPredicates(nodeMembershipChanged()),
		).
		Complete(&ModelWarmupReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()})
}

//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modelwarmups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modelwarmups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

func nodeMembershipChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !labels.Equals(labels.Set(e.ObjectOld.GetLabels()), labels.Set(e.ObjectNew.GetLabels()))
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func enqueueActiveModelWarmups(c client.Client) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		var warmups modelv1alpha1.ModelWarmupList
		if err := c.List(ctx, &warmups); err != nil {
			klog.ErrorS(err, "unable to list ModelWarmups for Node event")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(warmups.Items))
		for i := range warmups.Items {
			warmup := &warmups.Items[i]
			if effectiveMode(warmup) == modelv1alpha1.ModelWarmupModeOnce && isTerminalPhase(warmup.Status.Phase) {
				continue
			}
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})
		}
		return requests
	}
}

func (r *ModelWarmupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	warmup := &modelv1alpha1.ModelWarmup{}
	if err := r.Get(ctx, req.NamespacedName, warmup); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if effectiveMode(warmup) == modelv1alpha1.ModelWarmupModeOnce && isTerminalPhase(warmup.Status.Phase) {
		return ctrl.Result{}, nil
	}
	revision := revisionFor(warmup)
	targets, missing, err := r.resolveTargets(ctx, warmup)
	if err != nil {
		return ctrl.Result{}, err
	}
	klog.V(4).InfoS("resolved ModelWarmup targets", "modelWarmup", req.NamespacedName,
		"revision", revision, "targets", len(targets), "missing", len(missing))
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(warmup.Namespace), client.MatchingLabels{
		WarmupLabelKey: string(warmup.UID),
	}); err != nil {
		return ctrl.Result{}, err
	}
	if effectiveMode(warmup) == modelv1alpha1.ModelWarmupModeContinuous {
		return r.reconcileContinuous(ctx, warmup, revision, targets, jobs.Items)
	}
	snapshot := buildReconcileSnapshot(warmup, revision, targets, missing, jobs.Items)
	if !withinTargetLimit(targets, missing) {
		message := fmt.Sprintf(
			"resolved %d targets; maximum is %d",
			len(targets)+len(missing),
			modelv1alpha1.MaxModelWarmupTargets,
		)
		return r.updateStatus(
			ctx, warmup, revision, targets, missing, snapshot.jobsForStatus(nil), jobs.Items,
			"TargetLimitExceeded", message,
		)
	}
	for i := range snapshot.StaleActiveJobs {
		job := &snapshot.StaleActiveJobs[i]
		if err := r.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		klog.V(3).InfoS("deleted stale ModelWarmup Job", "modelWarmup",
			client.ObjectKeyFromObject(warmup), "job", job.Name,
			"jobRevision", job.Labels[RevisionLabelKey], "currentRevision", revision)
	}
	policies := effectiveWarmupPolicies(warmup)
	klog.V(4).InfoS("reconciling ModelWarmup Job capacity", "modelWarmup", req.NamespacedName,
		"revision", revision, "activeJobs", snapshot.ActiveJobs, "parallelism", policies.parallelism)
	active := snapshot.ActiveJobs
	created := make([]*batchv1.Job, 0, len(snapshot.MissingNodes))
	for _, node := range snapshot.MissingNodes {
		if active >= policies.parallelism {
			klog.V(4).InfoS("deferring ModelWarmup target because parallelism is exhausted",
				"modelWarmup", req.NamespacedName, "node", node, "parallelism", policies.parallelism)
			break
		}
		job := r.jobForTarget(warmup, snapshot.Targets[node], revision, 0)
		if err := ctrl.SetControllerReference(warmup, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil {
			if apierrors.IsAlreadyExists(err) {
				var existing batchv1.Job
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(job), &existing); getErr != nil {
					return ctrl.Result{}, fmt.Errorf("diagnose existing ModelWarmup Job %s/%s: %w",
						job.Namespace, job.Name, getErr)
				}
				if !metav1.IsControlledBy(&existing, warmup) {
					return ctrl.Result{}, fmt.Errorf("job %s/%s already exists and is not controlled by ModelWarmup %s",
						existing.Namespace, existing.Name, warmup.Name)
				}
				if existing.Labels[WarmupLabelKey] != string(warmup.UID) ||
					existing.Labels[RevisionLabelKey] != revision {
					continue
				}
				created = append(created, existing.DeepCopy())
				if existing.DeletionTimestamp == nil && !isJobComplete(&existing) && !isJobFailed(&existing) {
					active++
				}
				continue
			}
			return ctrl.Result{}, fmt.Errorf("create ModelWarmup Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		klog.V(3).InfoS("created ModelWarmup Job", "modelWarmup", req.NamespacedName,
			"node", node, "job", job.Name, "revision", revision)
		created = append(created, job)
		active++
	}
	jobsForTTL := make([]batchv1.Job, 0, len(jobs.Items)+len(created))
	jobsForTTL = append(jobsForTTL, jobs.Items...)
	for _, job := range created {
		if job != nil {
			jobsForTTL = append(jobsForTTL, *job)
		}
	}
	result, err := r.updateStatus(ctx, warmup, revision, targets, missing,
		snapshot.jobsForStatus(created), jobsForTTL, "", "")
	if err != nil {
		return result, err
	}
	return ctrl.Result{}, nil
}

func withinTargetLimit(targets map[string]resolvedTarget, missing map[string]string) bool {
	return len(targets)+len(missing) <= modelv1alpha1.MaxModelWarmupTargets
}

type warmupPolicies struct {
	parallelism                    int32
	jobTimeoutSeconds              int64
	retryLimit                     int32
	ttlSecondsAfterFinished        int32
	continuousRetryLimit           int32
	continuousRetryIntervalSeconds int64
}

func effectiveMode(w *modelv1alpha1.ModelWarmup) modelv1alpha1.ModelWarmupMode {
	if w.Spec.Mode == "" {
		return modelv1alpha1.ModelWarmupModeOnce
	}
	return w.Spec.Mode
}

func effectiveWarmupPolicies(w *modelv1alpha1.ModelWarmup) warmupPolicies {
	result := warmupPolicies{
		parallelism:                    modelv1alpha1.DefaultModelWarmupParallelism,
		jobTimeoutSeconds:              modelv1alpha1.DefaultModelWarmupJobTimeoutSeconds,
		retryLimit:                     modelv1alpha1.DefaultModelWarmupRetryLimit,
		ttlSecondsAfterFinished:        modelv1alpha1.DefaultModelWarmupTTLSecondsAfterFinished,
		continuousRetryLimit:           modelv1alpha1.DefaultModelWarmupContinuousRetryLimit,
		continuousRetryIntervalSeconds: modelv1alpha1.DefaultModelWarmupContinuousRetryInterval,
	}
	if w.Spec.Policies == nil {
		return result
	}
	if w.Spec.Policies.Parallelism != nil {
		result.parallelism = *w.Spec.Policies.Parallelism
	}
	if w.Spec.Policies.JobTimeoutSeconds != nil {
		result.jobTimeoutSeconds = *w.Spec.Policies.JobTimeoutSeconds
	}
	if w.Spec.Policies.RetryLimit != nil {
		result.retryLimit = *w.Spec.Policies.RetryLimit
	}
	if w.Spec.Policies.TTLSecondsAfterFinished != nil {
		result.ttlSecondsAfterFinished = *w.Spec.Policies.TTLSecondsAfterFinished
	}
	if w.Spec.Policies.ContinuousRetryLimit != nil {
		result.continuousRetryLimit = *w.Spec.Policies.ContinuousRetryLimit
	}
	if w.Spec.Policies.ContinuousRetryIntervalSeconds != nil {
		result.continuousRetryIntervalSeconds = *w.Spec.Policies.ContinuousRetryIntervalSeconds
	}
	return result
}

func (r *ModelWarmupReconciler) resolveTargets(
	ctx context.Context,
	warmup *modelv1alpha1.ModelWarmup,
) (map[string]resolvedTarget, map[string]string, error) {
	var namespace corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: warmup.Namespace}, &namespace); err != nil {
		return nil, nil, err
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, nil, err
	}
	return resolveTargetsFromNodes(warmup, &namespace, nodes.Items)
}

func isNodeAuthorized(namespace *corev1.Namespace, node *corev1.Node) bool {
	if node.Labels[WarmupEnabledLabelKey] != WarmupEnabledLabelValue {
		return false
	}
	pool := namespace.Labels[ResourcePoolLabelKey]
	if pool == "" || node.Labels[ResourcePoolLabelKey] != pool {
		return false
	}
	for _, key := range controlPlaneLabelKeys {
		if _, exists := node.Labels[key]; exists {
			return false
		}
	}
	return true
}

func revisionFor(w *modelv1alpha1.ModelWarmup) string {
	parts := make([]string, 0, len(w.Spec.ImagePreload.Images)+5)
	for _, image := range w.Spec.ImagePreload.Images {
		pullPolicy := image.ImagePullPolicy
		if pullPolicy == "" {
			pullPolicy = corev1.PullIfNotPresent
		}
		imageParts := []string{
			image.Image,
			strings.Join(image.Command, "\x00"),
			strings.Join(image.Args, "\x00"),
			string(pullPolicy),
		}
		parts = append(parts, strings.Join(imageParts, "\x01"))
	}
	for _, secret := range w.Spec.ImagePreload.PullSecrets {
		parts = append(parts, "secret="+secret.Name)
	}
	if w.Spec.Custom != nil {
		parts = append(parts, customActionRevisionInput(w.Spec.Custom))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

func customActionRevisionInput(custom *modelv1alpha1.ModelWarmupCustomAction) string {
	canonical := custom.DeepCopy()
	canonicalizeResourceQuantities(reflect.ValueOf(canonical))
	data, err := json.Marshal(canonical)
	if err != nil {
		// ModelWarmupCustomAction contains only Kubernetes API types, all of which are JSON-safe.
		panic(fmt.Sprintf("marshal ModelWarmup custom action for revision: %v", err))
	}
	return "custom=" + string(data)
}

var resourceQuantityType = reflect.TypeOf(resource.Quantity{})

// canonicalizeResourceQuantities rewrites every Quantity in a copied custom
// action to DecimalSI so semantically equal Kubernetes resource values produce
// the same revision regardless of their input format.
func canonicalizeResourceQuantities(value reflect.Value) {
	if !value.IsValid() {
		return
	}
	if value.Type() == resourceQuantityType && value.CanSet() {
		quantity := value.Interface().(resource.Quantity)
		canonical := resource.NewDecimalQuantity(*quantity.AsDec(), resource.DecimalSI)
		value.Set(reflect.ValueOf(*canonical))
		return
	}

	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			canonicalizeResourceQuantities(value.Elem())
		}
	case reflect.Interface:
		if !value.IsNil() && value.CanSet() {
			element := reflect.New(value.Elem().Type()).Elem()
			element.Set(value.Elem())
			canonicalizeResourceQuantities(element)
			value.Set(element)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Field(i).CanSet() {
				canonicalizeResourceQuantities(value.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			canonicalizeResourceQuantities(value.Index(i))
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			item := reflect.New(value.Type().Elem()).Elem()
			item.Set(value.MapIndex(key))
			canonicalizeResourceQuantities(item)
			value.SetMapIndex(key, item)
		}
	}
}

func (r *ModelWarmupReconciler) jobFor(w *modelv1alpha1.ModelWarmup, node, revision string) *batchv1.Job {
	policies := effectiveWarmupPolicies(w)
	containers := make([]corev1.Container, 0, len(w.Spec.ImagePreload.Images))
	for i, image := range w.Spec.ImagePreload.Images {
		if image.ImagePullPolicy == "" {
			image.ImagePullPolicy = corev1.PullIfNotPresent
		}
		containers = append(containers, corev1.Container{
			Name:            fmt.Sprintf("image-%d", i),
			Image:           image.Image,
			Command:         image.Command,
			Args:            image.Args,
			ImagePullPolicy: image.ImagePullPolicy,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
			},
		})
	}
	var initContainers []corev1.Container
	var volumes []corev1.Volume
	var customPullSecrets []corev1.LocalObjectReference
	if w.Spec.Custom != nil {
		initContainers = copyContainers(w.Spec.Custom.InitContainers)
		containers = append(containers, copyContainers(w.Spec.Custom.Containers)...)
		volumes = copyVolumes(w.Spec.Custom.Volumes)
		customPullSecrets = w.Spec.Custom.ImagePullSecrets
	}
	name := modelWarmupJobName(w.Name, string(w.UID), node, revision)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: w.Namespace,
			Annotations: map[string]string{
				TargetNodeAnnotationKey: node,
				WarmupNameAnnotationKey: w.Name,
			},
			Labels: map[string]string{
				WarmupLabelKey:   string(w.UID),
				RevisionLabelKey: revision,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To(policies.retryLimit),
			ActiveDeadlineSeconds: ptr.To(policies.jobTimeoutSeconds),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: ptr.To(false),
				ImagePullSecrets:             mergePullSecrets(w.Spec.ImagePreload.PullSecrets, customPullSecrets),
				Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchFields: []corev1.NodeSelectorRequirement{{
								Key:      "metadata.name",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{node},
							}},
						}},
					},
				}},
				InitContainers: initContainers,
				Containers:     containers,
				Volumes:        volumes,
			}},
		},
	}
}

func (r *ModelWarmupReconciler) jobForTarget(
	w *modelv1alpha1.ModelWarmup,
	target resolvedTarget,
	revision string,
	attempt int32,
) *batchv1.Job {
	job := r.jobFor(w, target.NodeName, revision)
	job.Annotations[TargetNodeUIDAnnotationKey] = string(target.NodeUID)
	if attempt > 0 {
		job.Annotations[AttemptAnnotationKey] = strconv.FormatInt(int64(attempt), 10)
		job.Name = modelWarmupJobNameForAttempt(w.Name, string(w.UID), target.NodeName, revision, attempt)
	}
	return job
}

func copyContainers(containers []corev1.Container) []corev1.Container {
	if len(containers) == 0 {
		return nil
	}
	result := make([]corev1.Container, len(containers))
	for i := range containers {
		containers[i].DeepCopyInto(&result[i])
	}
	return result
}

func copyVolumes(volumes []corev1.Volume) []corev1.Volume {
	if len(volumes) == 0 {
		return nil
	}
	result := make([]corev1.Volume, len(volumes))
	for i := range volumes {
		volumes[i].DeepCopyInto(&result[i])
	}
	return result
}

func mergePullSecrets(groups ...[]corev1.LocalObjectReference) []corev1.LocalObjectReference {
	seen := make(map[string]struct{})
	var result []corev1.LocalObjectReference
	for _, group := range groups {
		for _, secret := range group {
			if _, exists := seen[secret.Name]; exists {
				continue
			}
			seen[secret.Name] = struct{}{}
			result = append(result, secret)
		}
	}
	return result
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func (r *ModelWarmupReconciler) updateStatus(
	ctx context.Context,
	w *modelv1alpha1.ModelWarmup,
	revision string,
	targets map[string]resolvedTarget,
	missing map[string]string,
	statusJobs []batchv1.Job,
	ttlJobs []batchv1.Job,
	limitReason, limitMessage string,
) (ctrl.Result, error) {
	desired := w.DeepCopy()
	previousTargets := make(map[string]modelv1alpha1.ModelWarmupTargetStatus, len(desired.Status.Targets))
	for _, target := range desired.Status.Targets {
		previousTargets[target.NodeName] = target
	}
	now := time.Now()
	if desired.Status.StartTime == nil {
		start := metav1.NewTime(now)
		desired.Status.StartTime = &start
		desired.Status.CompletionTime = nil
	}

	byNode := map[string]batchv1.Job{}
	for _, job := range statusJobs {
		if job.Labels[RevisionLabelKey] == revision && metav1.IsControlledBy(&job, desired) {
			byNode[targetNodeForJob(&job)] = job
		}
	}
	details := make([]modelv1alpha1.ModelWarmupTargetStatus, 0, len(targets)+len(missing))
	active, pending, succeeded, failed := int32(0), int32(0), int32(0), int32(0)
	for node, target := range targets {
		sources := target.Sources
		item := modelv1alpha1.ModelWarmupTargetStatus{
			NodeName: node, Source: primarySource(sources), SourceCount: int32(len(sources)), Revision: revision,
			Phase: modelv1alpha1.ModelWarmupTargetPending, Message: boundedDiagnostic("waiting for a warmup job"),
		}
		if job, ok := byNode[node]; ok {
			item.JobName = job.Name
			switch {
			case isJobComplete(&job):
				succeeded++
				continue
			case isJobFailed(&job):
				item.Phase = modelv1alpha1.ModelWarmupTargetFailed
				item.Reason, item.Message = jobFailureDetails(&job)
				item.Reason = boundedDiagnostic(item.Reason)
				item.Message = boundedDiagnostic(item.Message)
				failed++
			default:
				item.Phase = modelv1alpha1.ModelWarmupTargetRunning
				item.Message = boundedDiagnostic("warmup job is running")
				active++
			}
		} else {
			pending++
		}
		preserveTargetTransition(&item, previousTargets[node].LastTransitionTime, previousTargets[node].Phase,
			previousTargets[node].Reason, previousTargets[node].Message, now)
		details = append(details, item)
	}
	for node, reason := range missing {
		message := "node was not found"
		if reason == "NodeNotAuthorized" {
			message = "node is not authorized for this namespace"
		}
		item := modelv1alpha1.ModelWarmupTargetStatus{
			NodeName: node, Revision: revision,
			Phase: modelv1alpha1.ModelWarmupTargetFailed, Reason: reason, Message: boundedDiagnostic(message),
		}
		preserveTargetTransition(&item, previousTargets[node].LastTransitionTime, previousTargets[node].Phase,
			previousTargets[node].Reason, previousTargets[node].Message, now)
		details = append(details, item)
		failed++
	}
	sort.Slice(details, func(i, j int) bool {
		leftPriority := targetDetailPriority(details[i].Phase)
		rightPriority := targetDetailPriority(details[j].Phase)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return details[i].NodeName < details[j].NodeName
	})
	omitted := 0
	if len(details) > modelv1alpha1.MaxModelWarmupTargetDetails {
		omitted = len(details) - modelv1alpha1.MaxModelWarmupTargetDetails
		details = details[:modelv1alpha1.MaxModelWarmupTargetDetails]
	}
	desired.Status.ObservedRevision = revision
	desired.Status.DesiredNodes = int32(len(targets) + len(missing))
	desired.Status.ActiveNodes = active
	desired.Status.SucceededNodes = succeeded
	desired.Status.FailedNodes = failed
	desired.Status.OmittedTargetDetails = int32(omitted)
	desired.Status.Targets = details
	if limitReason != "" {
		desired.Status.Phase = modelv1alpha1.ModelWarmupFailed
		setCompletionTime(desired, now)
		setCondition(desired, "Degraded", metav1.ConditionTrue, limitReason, limitMessage, now)
	} else if desired.Status.DesiredNodes == 0 {
		desired.Status.Phase = modelv1alpha1.ModelWarmupPending
		desired.Status.CompletionTime = nil
		setCondition(
			desired,
			"Progressing",
			metav1.ConditionTrue,
			"NoTargetsResolved",
			"waiting for target nodes to match the configured selectors",
			now,
		)
	} else if active+pending > 0 {
		desired.Status.Phase = modelv1alpha1.ModelWarmupRunning
		desired.Status.CompletionTime = nil
		setCondition(desired, "Progressing", metav1.ConditionTrue, "JobsRunning", "waiting for warmup jobs", now)
	} else if failed > 0 {
		if succeeded == 0 {
			desired.Status.Phase = modelv1alpha1.ModelWarmupFailed
		} else {
			desired.Status.Phase = modelv1alpha1.ModelWarmupDegraded
		}
		setCompletionTime(desired, now)
		setCondition(desired, "Degraded", metav1.ConditionTrue, "NodeFailed", "one or more warmup jobs failed", now)
	} else if succeeded == desired.Status.DesiredNodes {
		desired.Status.Phase = modelv1alpha1.ModelWarmupSucceeded
		setCompletionTime(desired, now)
		setCondition(desired, "Complete", metav1.ConditionTrue, successReason(desired), "all target jobs succeeded", now)
	} else {
		desired.Status.Phase = modelv1alpha1.ModelWarmupRunning
		desired.Status.CompletionTime = nil
		setCondition(desired, "Progressing", metav1.ConditionTrue, "JobsRunning", "waiting for warmup jobs", now)
	}
	if isTerminalPhase(desired.Status.Phase) {
		if err := r.applyFinishedJobTTL(ctx, desired, revision, ttlJobs); err != nil {
			return ctrl.Result{}, err
		}
	}
	if modelWarmupStatusEqual(w.Status, desired.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Update(ctx, desired); err != nil {
		return ctrl.Result{}, err
	}
	desired.DeepCopyInto(w)
	return ctrl.Result{}, nil
}

func successReason(w *modelv1alpha1.ModelWarmup) string {
	if w.Spec.Custom != nil {
		return "WarmupSucceeded"
	}
	return "ImagePreloadSucceeded"
}

func targetNodeForJob(job *batchv1.Job) string {
	if node := job.Annotations[TargetNodeAnnotationKey]; node != "" {
		return node
	}
	return job.Spec.Template.Spec.NodeName
}

func primarySource(sources []string) string {
	if len(sources) == 0 {
		return ""
	}
	return sources[0]
}

func boundedDiagnostic(message string) string {
	if len(message) <= modelv1alpha1.MaxModelWarmupDiagnosticLength {
		return message
	}
	return message[:modelv1alpha1.MaxModelWarmupDiagnosticLength]
}

func targetDetailPriority(phase modelv1alpha1.ModelWarmupTargetPhase) int {
	switch phase {
	case modelv1alpha1.ModelWarmupTargetFailed:
		return 0
	case modelv1alpha1.ModelWarmupTargetRunning:
		return 1
	default:
		return 2
	}
}

func isTerminalPhase(phase modelv1alpha1.ModelWarmupPhase) bool {
	return phase == modelv1alpha1.ModelWarmupSucceeded ||
		phase == modelv1alpha1.ModelWarmupDegraded || phase == modelv1alpha1.ModelWarmupFailed
}

func isJobComplete(job *batchv1.Job) bool {
	return hasJobCondition(job, batchv1.JobComplete)
}

func isJobFailed(job *batchv1.Job) bool {
	return hasJobCondition(job, batchv1.JobFailed)
}

func hasJobCondition(job *batchv1.Job, conditionType batchv1.JobConditionType) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailureDetails(job *batchv1.Job) (string, string) {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			reason := condition.Reason
			if reason == "" {
				reason = "JobFailed"
			}
			message := condition.Message
			if message == "" {
				message = "warmup job failed"
			}
			return reason, message
		}
	}
	return "JobFailed", "warmup job failed"
}

func modelWarmupJobName(warmupName, warmupUID, node, revision string) string {
	return modelWarmupJobNameForAttempt(warmupName, warmupUID, node, revision, 1)
}

func modelWarmupJobNameForAttempt(warmupName, warmupUID, node, revision string, attempt int32) string {
	attemptSuffix := ""
	if attempt > 1 {
		attemptSuffix = fmt.Sprintf("-a%d", attempt)
	}
	suffix := fmt.Sprintf("-%s-%s-%s%s", shortHash(warmupUID), shortHash(node), revision, attemptSuffix)
	prefix := warmupName
	if len(prefix)+len(suffix) > 63 {
		prefix = prefix[:63-len(suffix)]
		prefix = strings.TrimRight(prefix, "-.")
	}
	return prefix + suffix
}

func (r *ModelWarmupReconciler) applyFinishedJobTTL(
	ctx context.Context,
	warmup *modelv1alpha1.ModelWarmup,
	revision string,
	jobs []batchv1.Job,
) error {
	ttl := effectiveWarmupPolicies(warmup).ttlSecondsAfterFinished
	for i := range jobs {
		job := &jobs[i]
		if job.Labels[RevisionLabelKey] != revision || !metav1.IsControlledBy(job, warmup) ||
			(!isJobComplete(job) && !isJobFailed(job)) {
			continue
		}
		if job.Spec.TTLSecondsAfterFinished != nil && *job.Spec.TTLSecondsAfterFinished == ttl {
			continue
		}
		job.Spec.TTLSecondsAfterFinished = ptr.To(ttl)
		if err := r.Update(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

func preserveTargetTransition(item *modelv1alpha1.ModelWarmupTargetStatus, previous *metav1.Time,
	previousPhase modelv1alpha1.ModelWarmupTargetPhase, previousReason, previousMessage string, now time.Time) {
	if previous != nil && item.Phase == previousPhase && item.Reason == previousReason && item.Message == previousMessage {
		item.LastTransitionTime = previous.DeepCopy()
		return
	}
	transition := metav1.NewTime(now)
	item.LastTransitionTime = &transition
}

func setCompletionTime(w *modelv1alpha1.ModelWarmup, now time.Time) {
	if w.Status.CompletionTime == nil {
		completion := metav1.NewTime(now)
		w.Status.CompletionTime = &completion
	}
}

func setCondition(
	w *modelv1alpha1.ModelWarmup,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
	now time.Time,
) {
	transition := metav1.NewTime(now)
	conditions := make([]metav1.Condition, 0, 3)
	for _, typ := range []string{"Complete", "Progressing", "Degraded"} {
		desiredStatus := metav1.ConditionFalse
		desiredReason, desiredMessage := "NotActive", "condition is not active"
		if typ == conditionType {
			desiredStatus, desiredReason, desiredMessage = status, reason, message
		}
		condition := metav1.Condition{Type: typ, Status: desiredStatus, Reason: desiredReason,
			Message: desiredMessage, ObservedGeneration: w.Generation, LastTransitionTime: transition}
		if old := meta.FindStatusCondition(w.Status.Conditions, typ); old != nil && old.Status == condition.Status &&
			old.Reason == condition.Reason && old.Message == condition.Message {
			condition.LastTransitionTime = old.LastTransitionTime
		}
		conditions = append(conditions, condition)
	}
	w.Status.Conditions = conditions
}
