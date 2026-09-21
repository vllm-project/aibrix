/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
)

const (
	controllerName = "model-warmup-controller"

	WarmupLabelKey   = "model.aibrix.ai/warmup"
	RevisionLabelKey = "model.aibrix.ai/revision"
)

type ModelWarmupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func Add(mgr manager.Manager, _ config.RuntimeConfig) error {
	return ctrl.NewControllerManagedBy(mgr).Named(controllerName).
		For(&modelv1alpha1.ModelWarmup{}, builder.WithPredicates()).
		Owns(&batchv1.Job{}).
		Complete(&ModelWarmupReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()})
}

//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modelwarmups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modelwarmups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

func (r *ModelWarmupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	warmup := &modelv1alpha1.ModelWarmup{}
	if err := r.Get(ctx, req.NamespacedName, warmup); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
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
	parallelism := warmupParallelism(warmup)
	remainingTimeout, workflowTimedOut := remainingGlobalTimeout(warmup, time.Now())
	klog.V(4).InfoS("reconciling ModelWarmup Job capacity", "modelWarmup", req.NamespacedName,
		"revision", revision, "activeJobs", active, "parallelism", parallelism,
		"remainingTimeoutSeconds", remainingTimeout, "workflowTimedOut", workflowTimedOut)
	nodes := make([]string, 0, len(targets))
	for node := range targets {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if workflowTimedOut {
			klog.V(4).InfoS("not creating ModelWarmup Job after the global timeout",
				"modelWarmup", req.NamespacedName, "node", node, "revision", revision)
			break
		}
		if active >= parallelism {
			klog.V(4).InfoS("deferring ModelWarmup target because parallelism is exhausted",
				"modelWarmup", req.NamespacedName, "node", node, "parallelism", parallelism)
			break
		}
		job := r.jobFor(warmup, node, revision)
		job.Spec.ActiveDeadlineSeconds = ptr.To(remainingTimeout)
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
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func withinTargetLimit(targets map[string][]string, missing map[string]string) bool {
	return len(targets)+len(missing) <= modelv1alpha1.MaxModelWarmupTargets
}

func warmupParallelism(w *modelv1alpha1.ModelWarmup) int32 {
	if w.Spec.Policies != nil && w.Spec.Policies.Parallelism != nil {
		return *w.Spec.Policies.Parallelism
	}
	return modelv1alpha1.DefaultModelWarmupParallelism
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
		if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
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
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}
		_, targetExists := targets[job.Spec.Template.Spec.NodeName]
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
				targets[node.Name] = append(targets[node.Name], source)
			}
		}
	}
	for name := range targets {
		sort.Strings(targets[name])
	}
	return targets, missing, nil
}

func revisionFor(w *modelv1alpha1.ModelWarmup) string {
	parts := make([]string, 0, len(w.Spec.ImagePreload.Images)+5)
	for _, image := range w.Spec.ImagePreload.Images {
		imageParts := []string{
			image.Image,
			strings.Join(image.Command, "\x00"),
			strings.Join(image.Args, "\x00"),
			string(image.ImagePullPolicy),
		}
		parts = append(parts, strings.Join(imageParts, "\x01"))
	}
	for _, secret := range w.Spec.ImagePreload.PullSecrets {
		parts = append(parts, "secret="+secret.Name)
	}
	if p := w.Spec.Policies; p != nil {
		if p.Parallelism != nil {
			parts = append(parts, fmt.Sprintf("p=%d", *p.Parallelism))
		}
		if p.GlobalTimeoutSeconds != nil {
			parts = append(parts, fmt.Sprintf("t=%d", *p.GlobalTimeoutSeconds))
		}
		if p.RetryLimit != nil {
			parts = append(parts, fmt.Sprintf("r=%d", *p.RetryLimit))
		}
		if p.TTLSecondsAfterFinished != nil {
			parts = append(parts, fmt.Sprintf("ttl=%d", *p.TTLSecondsAfterFinished))
		}
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

func (r *ModelWarmupReconciler) jobFor(w *modelv1alpha1.ModelWarmup, node, revision string) *batchv1.Job {
	p := w.Spec.Policies
	retry := modelv1alpha1.DefaultModelWarmupRetryLimit
	timeout := modelv1alpha1.DefaultModelWarmupGlobalTimeoutSeconds
	ttl := modelv1alpha1.DefaultModelWarmupTTLSecondsAfterFinished
	if p != nil {
		if p.RetryLimit != nil {
			retry = *p.RetryLimit
		}
		if p.GlobalTimeoutSeconds != nil {
			timeout = *p.GlobalTimeoutSeconds
		}
		if p.TTLSecondsAfterFinished != nil {
			ttl = *p.TTLSecondsAfterFinished
		}
	}
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
	name := fmt.Sprintf("%s-%s-%s", w.Name, shortHash(node), revision)
	if len(name) > 63 {
		name = name[:63]
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: w.Namespace,
			Labels: map[string]string{
				WarmupLabelKey:   w.Name,
				RevisionLabelKey: revision,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(retry),
			TTLSecondsAfterFinished: ptr.To(ttl),
			ActiveDeadlineSeconds:   ptr.To(timeout),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeName:                     node,
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: ptr.To(false),
				ImagePullSecrets:             w.Spec.ImagePreload.PullSecrets,
				Tolerations: []corev1.Toleration{{
					Operator: corev1.TolerationOpExists,
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
	previousRevision := w.Status.ObservedRevision
	previousTargets := make(map[string]modelv1alpha1.ModelWarmupTargetStatus, len(w.Status.Targets))
	for _, target := range w.Status.Targets {
		previousTargets[target.NodeName] = target
	}
	resetWorkflow := previousRevision != "" && previousRevision != revision
	if !resetWorkflow && isTerminalPhase(w.Status.Phase) {
		for node := range targets {
			if _, ok := previousTargets[node]; !ok {
				resetWorkflow = true
				break
			}
		}
	}
	now := time.Now()
	if w.Status.StartTime == nil || resetWorkflow {
		start := metav1.NewTime(now)
		w.Status.StartTime = &start
		w.Status.CompletionTime = nil
	}
	timeout := warmupGlobalTimeout(w)
	timedOut := timeout > 0 && !now.Before(w.Status.StartTime.Add(time.Duration(timeout)*time.Second))

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
				byNode[job.Spec.Template.Spec.NodeName] = job
			}
		}
	}
	status := make([]modelv1alpha1.ModelWarmupTargetStatus, 0, len(targets)+len(missing))
	active, succeeded, failed := int32(0), int32(0), int32(0)
	for node, sources := range targets {
		item := modelv1alpha1.ModelWarmupTargetStatus{
			NodeName: node, Sources: sources, Revision: revision,
			Phase: modelv1alpha1.ModelWarmupTargetPending, Message: "waiting for a warmup job",
		}
		if job, ok := byNode[node]; ok {
			item.JobName = job.Name
			switch {
			case job.Status.Succeeded > 0:
				item.Phase = modelv1alpha1.ModelWarmupTargetSucceeded
				item.Message = "warmup job completed successfully"
				succeeded++
			case job.Status.Failed > 0:
				item.Phase = modelv1alpha1.ModelWarmupTargetFailed
				item.Reason, item.Message = jobFailureDetails(&job)
				failed++
			case timedOut:
				item.Phase = modelv1alpha1.ModelWarmupTargetFailed
				item.Reason = "Timeout"
				item.Message = fmt.Sprintf("warmup exceeded global timeout of %d seconds", timeout)
				failed++
			default:
				item.Phase = modelv1alpha1.ModelWarmupTargetRunning
				item.Message = "warmup job is running"
				active++
			}
		} else if timedOut {
			item.Phase = modelv1alpha1.ModelWarmupTargetFailed
			item.Reason = "Timeout"
			item.Message = fmt.Sprintf("warmup exceeded global timeout of %d seconds", timeout)
			failed++
		}
		preserveTargetTransition(&item, previousTargets[node].LastTransitionTime, previousTargets[node].Phase,
			previousTargets[node].Reason, previousTargets[node].Message, now)
		status = append(status, item)
	}
	for node, reason := range missing {
		item := modelv1alpha1.ModelWarmupTargetStatus{
			NodeName: node, Revision: revision,
			Phase: modelv1alpha1.ModelWarmupTargetFailed, Reason: reason, Message: "node was not found",
		}
		preserveTargetTransition(&item, previousTargets[node].LastTransitionTime, previousTargets[node].Phase,
			previousTargets[node].Reason, previousTargets[node].Message, now)
		status = append(status, item)
		failed++
	}
	sort.Slice(status, func(i, j int) bool { return status[i].NodeName < status[j].NodeName })
	w.Status.ObservedRevision = revision
	w.Status.DesiredNodes = int32(len(status))
	w.Status.ActiveNodes = active
	w.Status.SucceededNodes = succeeded
	w.Status.FailedNodes = failed
	w.Status.Targets = status
	if limitReason != "" {
		w.Status.Phase = modelv1alpha1.ModelWarmupDegraded
		setCompletionTime(w)
		setCondition(w, "Degraded", metav1.ConditionTrue, limitReason, limitMessage)
	} else if failed > 0 {
		w.Status.Phase = modelv1alpha1.ModelWarmupDegraded
		setCompletionTime(w)
		setCondition(w, "Degraded", metav1.ConditionTrue, "NodeFailed", "one or more warmup jobs failed")
	} else if w.Status.DesiredNodes > 0 && succeeded == w.Status.DesiredNodes {
		w.Status.Phase = modelv1alpha1.ModelWarmupSucceeded
		setCompletionTime(w)
		setCondition(w, "Ready", metav1.ConditionTrue, "ImagePreloadSucceeded", "all target jobs succeeded")
	} else {
		w.Status.Phase = modelv1alpha1.ModelWarmupRunning
		setCondition(w, "Progressing", metav1.ConditionTrue, "JobsRunning", "waiting for warmup jobs")
	}
	if err := r.Status().Update(ctx, w); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func warmupGlobalTimeout(w *modelv1alpha1.ModelWarmup) int64 {
	if w.Spec.Policies != nil && w.Spec.Policies.GlobalTimeoutSeconds != nil {
		return *w.Spec.Policies.GlobalTimeoutSeconds
	}
	return modelv1alpha1.DefaultModelWarmupGlobalTimeoutSeconds
}

func remainingGlobalTimeout(w *modelv1alpha1.ModelWarmup, now time.Time) (int64, bool) {
	timeout := warmupGlobalTimeout(w)
	if w.Status.StartTime == nil {
		return timeout, false
	}
	remaining := w.Status.StartTime.Add(time.Duration(timeout) * time.Second).Sub(now)
	if remaining <= 0 {
		return 0, true
	}
	seconds := int64((remaining + time.Second - 1) / time.Second)
	return seconds, false
}

func isTerminalPhase(phase modelv1alpha1.ModelWarmupPhase) bool {
	return phase == modelv1alpha1.ModelWarmupSucceeded ||
		phase == modelv1alpha1.ModelWarmupDegraded || phase == modelv1alpha1.ModelWarmupFailed
}

func jobFailureDetails(job *batchv1.Job) (string, string) {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed {
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
	for _, typ := range []string{"Ready", "Progressing", "Degraded"} {
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
