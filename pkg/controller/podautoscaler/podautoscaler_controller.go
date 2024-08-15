/*
Copyright 2024 The Aibrix Team.

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

package podautoscaler

import (
	"context"
	"fmt"
	"io"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Add creates a new PodAutoscaler Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	// Instantiate a new PodAutoscalerReconciler with the given manager's client and scheme
	reconciler := &PodAutoscalerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	return reconciler, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller managed by AIBrix manager, watching for changes to PodAutoscaler objects
	// and HorizontalPodAutoscaler objects.
	err := ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.PodAutoscaler{}).
		Watches(&autoscalingv2.HorizontalPodAutoscaler{}, &handler.EnqueueRequestForObject{}).
		Complete(r)

	klog.V(4).InfoS("Added AIBricks pod-autoscaler-controller successfully")
	return err
}

var _ reconcile.Reconciler = &PodAutoscalerReconciler{}

// PodAutoscalerReconciler reconciles a PodAutoscaler object
type PodAutoscalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers/finalizers,verbs=update
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main Kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state as specified by
// the PodAutoscaler resource. It handles the creation, update, and deletion logic for
// HorizontalPodAutoscalers based on the PodAutoscaler specifications.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *PodAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Implement a timeout for the reconciliation process.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	klog.V(3).InfoS("Reconciling PodAutoscaler", "requestName", req.NamespacedName)

	var pa autoscalingv1alpha1.PodAutoscaler
	if err := r.Get(ctx, req.NamespacedName, &pa); err != nil {
		if errors.IsNotFound(err) {
			// Object might have been deleted after reconcile request, ignore and return.
			klog.InfoS("PodAutoscaler resource not found. Ignoring since object must have been deleted")
			return ctrl.Result{}, nil
		}
		klog.ErrorS(err, "Failed to get PodAutoscaler")
		return ctrl.Result{}, err
	}

	switch pa.Spec.ScalingStrategy {
	case autoscalingv1alpha1.HPA:
		return r.reconcileHPA(ctx, pa)
	case autoscalingv1alpha1.KPA:
		return r.reconcileKPA(ctx, pa)
	case autoscalingv1alpha1.APA:
		return r.reconcileAPA(ctx, pa)
	default:
		return ctrl.Result{}, fmt.Errorf("unknown autoscaling strategy: %s", pa.Spec.ScalingStrategy)
	}
}

func (r *PodAutoscalerReconciler) reconcileHPA(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler) (ctrl.Result, error) {
	// Generate a corresponding HorizontalPodAutoscaler
	hpa := makeHPA(&pa)
	hpaName := types.NamespacedName{
		Name:      hpa.Name,
		Namespace: hpa.Namespace,
	}

	existingHPA := autoscalingv2.HorizontalPodAutoscaler{}
	err := r.Get(ctx, hpaName, &existingHPA)
	if err != nil && errors.IsNotFound(err) {
		// HPA does not exist, create a new one.
		klog.InfoS("Creating a new HPA", "HPA.Namespace", hpa.Namespace, "HPA.Name", hpa.Name)
		if err = r.Create(ctx, hpa); err != nil {
			klog.ErrorS(err, "Failed to create new HPA")
			return ctrl.Result{}, err
		}
	} else if err != nil {
		// Error occurred while fetching the existing HPA, report the error and requeue.
		klog.ErrorS(err, "Failed to get HPA")
		return ctrl.Result{}, err
	} else {
		// Update the existing HPA if it already exists.
		klog.InfoS("Updating existing HPA", "HPA.Namespace", existingHPA.Namespace, "HPA.Name", existingHPA.Name)

		err = r.Update(ctx, hpa)
		if err != nil {
			klog.ErrorS(err, "Failed to update HPA")
			return ctrl.Result{}, err
		}
	}

	// TODO: add status update. Currently, actualScale and desireScale are not synced from HPA object yet.
	// Return with no error and no requeue needed.
	return ctrl.Result{}, nil
}

func (r *PodAutoscalerReconciler) reconcileKPA(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler) (ctrl.Result, error) {
	// Let's fetch associated deployment. To simplify the process, let's assume we just use deployment now.
	// TODO: based on the Kind to Get the object.
	var deployment appsv1.Deployment
	deploymentNamespacedName := types.NamespacedName{
		Namespace: pa.Spec.ScaleTargetRef.Namespace,
		Name:      pa.Spec.ScaleTargetRef.Name,
	}
	if err := r.Get(ctx, deploymentNamespacedName, &deployment); err != nil {
		return ctrl.Result{}, err
	}



	scaleReference := fmt.Sprintf("%s/%s/%s", pa.Spec.ScaleTargetRef.Kind, pa.Namespace, pa.Spec.ScaleTargetRef.Name)

	targetGV, err := schema.ParseGroupVersion(pa.Spec.ScaleTargetRef.APIVersion)
	if err != nil {
		r.eventRecorder.Event(pa, corev1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(pa, autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale", "the PodAutoscaler controller was unable to get the target's current scale: %v", err)
		if err := r.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa); err != nil {
			utilruntime.HandleError(err)
		}
		return fmt.Errorf("invalid API version in scale target reference: %v%w", err, errSpec)
	}


	// current scale's replica count
	currentReplicas := *deployment.Spec.Replicas
	// desired replica count
	desiredReplicas := int32(0)
	var minReplicas int32
	// minReplica is optional
	if pa.Spec.MinReplicas != nil {
		minReplicas = *pa.Spec.MinReplicas
	} else {
		minReplicas = 1
	}

	// https://xiaorui.cc/archives/7365
	rescale := true
	if *deployment.Spec.Replicas == int32(0) && minReplicas != 0 {
		// if the replica is 0, then we should not enable autoscaling
		desiredReplicas = 0
		rescale = false
	} else if currentReplicas > pa.Spec.MaxReplicas {
		desiredReplicas = pa.Spec.MaxReplicas
	} else if currentReplicas < minReplicas {
		desiredReplicas = minReplicas
	} else {
		metricDesiredReplicas, metricName, metricStatuses, metricTimestamp, err := r.computeReplicasForMetrics(ctx, pa, scale, pa.Spec.MetricsSources)
		if err != nil {
			r.updaStatusIfNeeded(ctx, paStatusOld, pa)
			return ctrl.Result{}, fmt.Errorf("failed to compute desired number of replicas based on listed metrics for %s: %v\", reference, err")
		}

		rescale = desiredReplicas != currentReplicas
	}

	if rescale {
		deployment.Spec.Replicas = desiredReplicas

		_, err = r.scaleNamespacer.Scales(hpa.Namespace).Update(ctx, targetGR, scale, metav1.UpdateOptions{})
	} else {
		desiredReplicas = currentReplicas
	}

	r.SetStatus(hpa, currentReplicas, desiredReplicas, metricsStatus, rescale)

	retrn r.updaStatusIfNeeded(ctx, paStatusOld, pa)

	// list all the pods
	var podList corev1.PodList
	labelSelector := client.MatchingLabels(deployment.Spec.Selector.MatchLabels)
	if err := r.List(ctx, &podList, client.InNamespace(deployment.Namespace), labelSelector); err != nil {
		return ctrl.Result{}, err
	}

	metrics, err := r.getMetricsFromPods(podList.Items)
	if err != nil {
		return ctrl.Result{}, err
	}

	newReplicaCount := r.calculateReplicaCount(metrics)

	if *deployment.Spec.Replicas != newReplicaCount {
		deployment.Spec.Replicas = &newReplicaCount
		if err := r.Update(ctx, &deployment); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.updateAutoscalerStatus(ctx, &pa, newReplicaCount); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Second * 5}, nil
}

func updateScale(ctx context.Context, c client.Client, namespace string, targetGR schema.GroupResource, scale *autoscalingv1.Scale) error {
	// Get GVK
	gvk, err := apiutil.GVKForObject(scale, c.Scheme())
	if err != nil {
		return err
	}

	// Get unstructured object
	scaleObj := &unstructured.Unstructured{}
	scaleObj.SetGroupVersionKind(gvk)
	scaleObj.SetNamespace(namespace)
	scaleObj.SetName(scale.Name)

	// Update scale object
	// TODO: change to kind name later.
	err = c.Patch(ctx, scale, client.Apply, client.FieldOwner("operator-name"))
	if err != nil {
		return err
	}

	return nil
}

func (r *PodAutoscalerReconciler) reconcileAPA(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler) (ctrl.Result, error) {
	// scraper
	// decider

	return ctrl.Result{}, fmt.Errorf("not implemneted yet")
}

func (r *PodAutoscalerReconciler) getMetricsFromPods(pods []corev1.Pod) ([]float64, error) {
	var metrics []float64

	for _, pod := range pods {
		// We should use the primary container port. In future, we can decide whether to use sidecar container's port
		url := fmt.Sprintf("http://%s:8000/metrics", pod.Status.PodIP)

		// scrape metrics
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch metrics from pod %s: %v", pod.Name, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response from pod %s: %v", pod.Name, err)
		}

		metricValue, err := r.parseMetricFromBody(body)
		if err != nil {
			return nil, fmt.Errorf("failed to parse metrics from pod %s: %v", pod.Name, err)
		}

		metrics = append(metrics, metricValue)
	}

	return metrics, nil
}

func (r *PodAutoscalerReconciler) calculateReplicaCount(metrics []float64) int32 {
	panic("not implemented")
}

func (r *PodAutoscalerReconciler) updateAutoscalerStatus(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, count int32) error {
	panic("not implemented")
}

func (r *PodAutoscalerReconciler) parseMetricFromBody(body []byte) (float64, error) {
	// TODO: let's use a different metrics name later
	metricName := "http_requests_total"
	lines := strings.Split(string(body), "\n")

	for _, line := range lines {
		if strings.Contains(line, metricName) {
			// format is `http_requests_total 1234.56`
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return 0, fmt.Errorf("unexpected format for metric %s", metricName)
			}

			// parse to float64
			value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse metric value for %s: %v", metricName, err)
			}

			return value, nil
		}
	}
	return 0, fmt.Errorf("metrics %s not found", metricName)
}

