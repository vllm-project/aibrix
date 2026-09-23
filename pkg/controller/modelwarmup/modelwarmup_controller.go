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
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	// WarmupLabelKey labels owned Jobs with the name of their ModelWarmup.
	WarmupLabelKey = "model.aibrix.ai/warmup"
	// RevisionLabelKey labels owned Jobs with the immutable warmup workload revision.
	RevisionLabelKey = "model.aibrix.ai/revision"
	// TargetNodeAnnotationKey records the exact node targeted by an owned Job.
	TargetNodeAnnotationKey = "model.aibrix.ai/target-node"

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
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get

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
	if !withinTargetLimit(targets, missing) {
		message := fmt.Sprintf(
			"resolved %d targets; maximum is %d",
			len(targets)+len(missing),
			modelv1alpha1.MaxModelWarmupTargets,
		)
		return r.updateStatus(
			ctx, warmup, revision, targets, missing, "TargetLimitExceeded", message,
		)
	}
	if err := r.cleanupStaleJobs(ctx, warmup, revision, targets); err != nil {
		return ctrl.Result{}, err
	}
	active, err := r.activeJobs(ctx, warmup, revision)
	if err != nil {
		return ctrl.Result{}, err
	}
	policies := effectiveWarmupPolicies(warmup)
	klog.V(4).InfoS("reconciling ModelWarmup Job capacity", "modelWarmup", req.NamespacedName,
		"revision", revision, "activeJobs", active, "parallelism", policies.parallelism)
	nodes := make([]string, 0, len(targets))
	for node := range targets {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if active >= policies.parallelism {
			klog.V(4).InfoS("deferring ModelWarmup target because parallelism is exhausted",
				"modelWarmup", req.NamespacedName, "node", node, "parallelism", policies.parallelism)
			break
		}
		job := r.jobFor(warmup, node, revision)
		var existing batchv1.Job
		if err := r.Get(ctx, client.ObjectKeyFromObject(job), &existing); err == nil {
			klog.V(5).InfoS("ModelWarmup Job already exists", "modelWarmup", req.NamespacedName,
				"node", node, "job", job.Name, "revision", revision)
			continue
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := ctrl.SetControllerReference(warmup, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		klog.V(3).InfoS("created ModelWarmup Job", "modelWarmup", req.NamespacedName,
			"node", node, "job", job.Name, "revision", revision)
		active++
	}
	result, err := r.updateStatus(ctx, warmup, revision, targets, missing, "", "")
	if err != nil {
		return result, err
	}
	return ctrl.Result{}, nil
}

func withinTargetLimit(targets map[string][]string, missing map[string]string) bool {
	return len(targets)+len(missing) <= modelv1alpha1.MaxModelWarmupTargets
}

type warmupPolicies struct {
	parallelism             int32
	jobTimeoutSeconds       int64
	retryLimit              int32
	ttlSecondsAfterFinished int32
}

func effectiveMode(w *modelv1alpha1.ModelWarmup) modelv1alpha1.ModelWarmupMode {
	if w.Spec.Mode == "" {
		return modelv1alpha1.ModelWarmupModeOnce
	}
	return w.Spec.Mode
}

func effectiveWarmupPolicies(w *modelv1alpha1.ModelWarmup) warmupPolicies {
	result := warmupPolicies{
		parallelism:             modelv1alpha1.DefaultModelWarmupParallelism,
		jobTimeoutSeconds:       modelv1alpha1.DefaultModelWarmupJobTimeoutSeconds,
		retryLimit:              modelv1alpha1.DefaultModelWarmupRetryLimit,
		ttlSecondsAfterFinished: modelv1alpha1.DefaultModelWarmupTTLSecondsAfterFinished,
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
	return result
}

func (r *ModelWarmupReconciler) activeJobs(
	ctx context.Context,
	warmup *modelv1alpha1.ModelWarmup,
	revision string,
) (int32, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(warmup.Namespace), client.MatchingLabels{
		WarmupLabelKey:   warmup.Name,
		RevisionLabelKey: revision,
	}); err != nil {
		return 0, err
	}
	var active int32
	for _, job := range jobs.Items {
		if job.DeletionTimestamp == nil && !isJobComplete(&job) && !isJobFailed(&job) {
			active++
		}
	}
	return active, nil
}

func (r *ModelWarmupReconciler) cleanupStaleJobs(
	ctx context.Context,
	warmup *modelv1alpha1.ModelWarmup,
	revision string,
	targets map[string][]string,
) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(warmup.Namespace), client.MatchingLabels{
		WarmupLabelKey: warmup.Name,
	}); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if isJobComplete(job) || isJobFailed(job) {
			continue
		}
		_, targetExists := targets[targetNodeForJob(job)]
		if job.Labels[RevisionLabelKey] == revision && targetExists {
			continue
		}
		if err := r.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		klog.V(3).InfoS("deleted stale ModelWarmup Job", "modelWarmup",
			client.ObjectKeyFromObject(warmup), "job", job.Name,
			"jobRevision", job.Labels[RevisionLabelKey], "currentRevision", revision)
	}
	return nil
}

func (r *ModelWarmupReconciler) resolveTargets(
	ctx context.Context,
	warmup *modelv1alpha1.ModelWarmup,
) (map[string][]string, map[string]string, error) {
	targets, missing := map[string][]string{}, map[string]string{}
	var namespace corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: warmup.Namespace}, &namespace); err != nil {
		return nil, nil, err
	}
	for i, target := range warmup.Spec.Targets {
		source := fmt.Sprintf("target[%d]", i)
		if target.Nodes != nil {
			for _, name := range target.Nodes.Names {
				var node corev1.Node
				if err := r.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
					if apierrors.IsNotFound(err) {
						missing[name] = "NodeNotFound"
						continue
					}
					return nil, nil, err
				}
				if !isNodeAuthorized(&namespace, &node) {
					missing[name] = "NodeNotAuthorized"
					continue
				}
				targets[name] = append(targets[name], source)
			}
		}
		if target.NodeSelector != nil {
			selector, err := metav1.LabelSelectorAsSelector(target.NodeSelector)
			if err != nil {
				return nil, nil, err
			}
			var nodes corev1.NodeList
			if err := r.List(ctx, &nodes, client.MatchingLabelsSelector{Selector: selector}); err != nil {
				return nil, nil, err
			}
			for _, node := range nodes.Items {
				if !isNodeAuthorized(&namespace, &node) {
					missing[node.Name] = "NodeNotAuthorized"
					continue
				}
				targets[node.Name] = append(targets[node.Name], source)
			}
		}
	}
	for name := range targets {
		sort.Strings(targets[name])
	}
	return targets, missing, nil
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
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:12]
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
	name := modelWarmupJobName(w.Name, node, revision)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: w.Namespace,
			Annotations: map[string]string{
				TargetNodeAnnotationKey: node,
			},
			Labels: map[string]string{
				WarmupLabelKey:   w.Name,
				RevisionLabelKey: revision,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(policies.retryLimit),
			TTLSecondsAfterFinished: ptr.To(policies.ttlSecondsAfterFinished),
			ActiveDeadlineSeconds:   ptr.To(policies.jobTimeoutSeconds),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: ptr.To(false),
				ImagePullSecrets:             w.Spec.ImagePreload.PullSecrets,
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
				Containers: containers,
			}},
		},
	}
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func (r *ModelWarmupReconciler) updateStatus(
	ctx context.Context,
	w *modelv1alpha1.ModelWarmup,
	revision string,
	targets map[string][]string,
	missing map[string]string,
	limitReason, limitMessage string,
) (ctrl.Result, error) {
	previousTargets := make(map[string]modelv1alpha1.ModelWarmupTargetStatus, len(w.Status.Targets))
	for _, target := range w.Status.Targets {
		previousTargets[target.NodeName] = target
	}
	now := time.Now()
	if w.Status.StartTime == nil {
		start := metav1.NewTime(now)
		w.Status.StartTime = &start
		w.Status.CompletionTime = nil
	}

	var jobs batchv1.JobList
	if err := r.List(
		ctx, &jobs, client.InNamespace(w.Namespace), client.MatchingLabels{
			WarmupLabelKey:   w.Name,
			RevisionLabelKey: revision,
		},
	); err != nil {
		return ctrl.Result{}, err
	}
	byNode := map[string]batchv1.Job{}
	for _, job := range jobs.Items {
		for _, owner := range job.OwnerReferences {
			if owner.UID == w.UID {
				byNode[targetNodeForJob(&job)] = job
			}
		}
	}
	details := make([]modelv1alpha1.ModelWarmupTargetStatus, 0, len(targets)+len(missing))
	active, pending, succeeded, failed := int32(0), int32(0), int32(0), int32(0)
	for node, sources := range targets {
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
	w.Status.ObservedRevision = revision
	w.Status.DesiredNodes = int32(len(targets) + len(missing))
	w.Status.ActiveNodes = active
	w.Status.SucceededNodes = succeeded
	w.Status.FailedNodes = failed
	w.Status.OmittedTargetDetails = int32(omitted)
	w.Status.Targets = details
	if limitReason != "" {
		w.Status.Phase = modelv1alpha1.ModelWarmupFailed
		setCompletionTime(w)
		setCondition(w, "Degraded", metav1.ConditionTrue, limitReason, limitMessage)
	} else if w.Status.DesiredNodes == 0 {
		w.Status.Phase = modelv1alpha1.ModelWarmupPending
		w.Status.CompletionTime = nil
		setCondition(
			w,
			"Progressing",
			metav1.ConditionTrue,
			"NoTargetsResolved",
			"waiting for target nodes to match the configured selectors",
		)
	} else if active+pending > 0 {
		w.Status.Phase = modelv1alpha1.ModelWarmupRunning
		w.Status.CompletionTime = nil
		setCondition(w, "Progressing", metav1.ConditionTrue, "JobsRunning", "waiting for warmup jobs")
	} else if failed > 0 {
		if succeeded == 0 {
			w.Status.Phase = modelv1alpha1.ModelWarmupFailed
		} else {
			w.Status.Phase = modelv1alpha1.ModelWarmupDegraded
		}
		setCompletionTime(w)
		setCondition(w, "Degraded", metav1.ConditionTrue, "NodeFailed", "one or more warmup jobs failed")
	} else if succeeded == w.Status.DesiredNodes {
		w.Status.Phase = modelv1alpha1.ModelWarmupSucceeded
		setCompletionTime(w)
		setCondition(w, "Complete", metav1.ConditionTrue, "ImagePreloadSucceeded", "all target jobs succeeded")
	} else {
		w.Status.Phase = modelv1alpha1.ModelWarmupRunning
		w.Status.CompletionTime = nil
		setCondition(w, "Progressing", metav1.ConditionTrue, "JobsRunning", "waiting for warmup jobs")
	}
	if err := r.Status().Update(ctx, w); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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

func modelWarmupJobName(warmupName, node, revision string) string {
	suffix := fmt.Sprintf("-%s-%s", shortHash(node), revision)
	prefix := warmupName
	if len(prefix)+len(suffix) > 63 {
		prefix = prefix[:63-len(suffix)]
		prefix = strings.TrimRight(prefix, "-.")
	}
	return prefix + suffix
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

func setCompletionTime(w *modelv1alpha1.ModelWarmup) {
	if w.Status.CompletionTime == nil {
		now := metav1.Now()
		w.Status.CompletionTime = &now
	}
}

func setCondition(
	w *modelv1alpha1.ModelWarmup,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	conditions := make([]metav1.Condition, 0, 3)
	for _, typ := range []string{"Complete", "Progressing", "Degraded"} {
		desiredStatus := metav1.ConditionFalse
		desiredReason, desiredMessage := "NotActive", "condition is not active"
		if typ == conditionType {
			desiredStatus, desiredReason, desiredMessage = status, reason, message
		}
		condition := metav1.Condition{Type: typ, Status: desiredStatus, Reason: desiredReason,
			Message: desiredMessage, ObservedGeneration: w.Generation, LastTransitionTime: now}
		if old := meta.FindStatusCondition(w.Status.Conditions, typ); old != nil && old.Status == condition.Status &&
			old.Reason == condition.Reason && old.Message == condition.Message {
			condition.LastTransitionTime = old.LastTransitionTime
		}
		conditions = append(conditions, condition)
	}
	w.Status.Conditions = conditions
}
