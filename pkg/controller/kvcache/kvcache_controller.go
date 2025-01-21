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

package kvcache

import (
	"context"
	"fmt"
	"reflect"

	orchestrationv1alpha1 "github.com/aibrix/aibrix/api/orchestration/v1alpha1"
	"github.com/aibrix/aibrix/pkg/config"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	KVCacheLabelKeyIdentifier    = "kvcache.orchestration.aibrix.ai/name"
	KVCacheLabelKeyRole          = "kvcache.orchestration.aibrix.ai/role"
	KVCacheLabelKeyMetadataIndex = "kvcache.orchestration.aibrix.ai/etcd-index"

	KVCacheAnnotationNodeAffinityKey        = "kvcache.orchestration.aibrix.ai/node-affinity-key"
	KVCacheAnnotationNodeAffinityDefaultKey = "machine.cluster.vke.volcengine.com/gpu-name"
	KVCacheAnnotationNodeAffinityGPUType    = "kvcache.orchestration.aibrix.ai/node-affinity-gpu-type"
	KVCacheAnnotationPodAffinityKey         = "kvcache.orchestration.aibrix.ai/pod-affinity-workload"

	KVCacheLabelValueRoleCache    = "cache"
	KVCacheLabelValueRoleMetadata = "metadata"
)

var (
	controllerKind = orchestrationv1alpha1.GroupVersion.WithKind("KVCache")
	controllerName = "kv-cache-controller"
)

// Add creates a new ModelAdapter Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager, runtimeConfig config.RuntimeConfig) error {
	r, err := newReconciler(mgr, runtimeConfig)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, runtimeConfig config.RuntimeConfig) (reconcile.Reconciler, error) {
	reconciler := &KVCacheReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorderFor(controllerName),
		RuntimeConfig: runtimeConfig,
	}
	return reconciler, nil
}

func podWithLabelFilter(labelKey string) predicate.Predicate {
	hasLabelAndModelIdentifier := func(labels map[string]string, labelKey string) bool {
		_, exists := labels[labelKey]
		return exists
	}

	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasLabelAndModelIdentifier(e.ObjectNew.GetLabels(), labelKey)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey)
		},
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	err := ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&orchestrationv1alpha1.KVCache{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&corev1.Service{}).
		Owns(&appsv1.Deployment{}).
		Watches(&corev1.Pod{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(podWithLabelFilter(KVCacheLabelKeyIdentifier))).
		Complete(r)

	klog.InfoS("Finished to add kv-cache-controller")
	return err
}

// KVCacheReconciler reconciles a KVCache object
type KVCacheReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	RuntimeConfig config.RuntimeConfig
}

// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch

// Reconcile reconciles a KVCache to desired state.
func (r *KVCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the KVCache instance
	kvCache := &orchestrationv1alpha1.KVCache{}
	err := r.Get(ctx, req.NamespacedName, kvCache)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return. Created objects are automatically garbage collected.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Handle metadata Pods like etcd or redis in future.
	err = r.reconcileMetadataService(ctx, kvCache)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Handle kvCache Deployment
	err = r.reconcileDeployment(ctx, kvCache)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Handle Services
	err = r.reconcileServices(ctx, kvCache)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KVCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchestrationv1alpha1.KVCache{}).
		Complete(r)
}

func (r *KVCacheReconciler) reconcileMetadataService(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	// We only support etcd at this moment, redis will be supported later.
	replicas := int(kvCache.Spec.Metadata.Etcd.Replicas)

	image := kvCache.Spec.Cache.Image
	if kvCache.Spec.Metadata.Etcd.Image != "" {
		image = kvCache.Spec.Metadata.Etcd.Image
	}

	// Reconcile etcd pods and services based on the number of replicas specified
	for i := 0; i < replicas; i++ {
		etcdPodName := fmt.Sprintf("%s-etcd-%d", kvCache.Name, i)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      etcdPodName,
				Namespace: kvCache.Namespace,
				Labels: map[string]string{
					KVCacheLabelKeyIdentifier:    kvCache.Name,
					KVCacheLabelKeyRole:          KVCacheLabelValueRoleMetadata,
					KVCacheLabelKeyMetadataIndex: etcdPodName,
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "etcd",
						Image: image, // share the same kv cache image
						Ports: []corev1.ContainerPort{
							{Name: "client", ContainerPort: 2379, Protocol: corev1.ProtocolTCP},
							{Name: "server", ContainerPort: 2380, Protocol: corev1.ProtocolTCP},
						},
						Command: []string{
							"etcd",
							"--name",
							etcdPodName,
							"--initial-advertise-peer-urls",
							fmt.Sprintf("http://%s:2380", etcdPodName),
							"--advertise-client-urls",
							fmt.Sprintf("http://%s:2379", etcdPodName),
							"--listen-peer-urls",
							"http://0.0.0.0:2380",
							"--listen-client-urls",
							"http://0.0.0.0:2379",
							"--initial-cluster",
							fmt.Sprintf("%s=http://%s:2380", etcdPodName, etcdPodName),
							"--initial-cluster-state",
							"new",
						},
					},
				},
			},
		}

		// Create or update the etcd pod
		if err := r.reconcilePod(ctx, pod); err != nil {
			return err
		}

		// Create or update the etcd service for each pod
		etcdService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      etcdPodName,
				Namespace: kvCache.Namespace,
				Labels: map[string]string{
					KVCacheLabelKeyIdentifier:    kvCache.Name,
					KVCacheLabelKeyRole:          KVCacheLabelValueRoleMetadata,
					KVCacheLabelKeyMetadataIndex: etcdPodName,
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Name: "client", Port: 2379, TargetPort: intstr.FromInt(2379), Protocol: corev1.ProtocolTCP},
					{Name: "server", Port: 2380, TargetPort: intstr.FromInt(2380), Protocol: corev1.ProtocolTCP},
				},
				Selector: map[string]string{
					KVCacheLabelKeyIdentifier:    kvCache.Name,
					KVCacheLabelKeyRole:          KVCacheLabelValueRoleMetadata,
					KVCacheLabelKeyMetadataIndex: etcdPodName,
				},
				Type: corev1.ServiceTypeClusterIP,
			},
		}

		if err := r.reconcileService(ctx, etcdService); err != nil {
			return err
		}
	}

	// Reconcile the RPC service once for all etcd instances
	etcdService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-etcd-service", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleMetadata,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "etcd-for-vineyard-port", Port: 2379, TargetPort: intstr.FromInt(2379), Protocol: corev1.ProtocolTCP},
			},
			Selector: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleMetadata,
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	if err := r.reconcileService(ctx, etcdService); err != nil {
		return err
	}

	return nil
}

func (r *KVCacheReconciler) reconcilePod(ctx context.Context, pod *corev1.Pod) error {
	found := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		klog.InfoS("Creating a new Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		return r.Create(ctx, pod)
	} else if err != nil {
		return err
	}

	// Check if the images need to be updated.
	// most Pod fields are mutable, so we just compare image here. We can extends to tolerations or other fields later.
	updateNeeded := false
	for i, container := range found.Spec.Containers {
		if len(pod.Spec.Containers) > i {
			if pod.Spec.Containers[i].Image != container.Image {
				// update the image
				found.Spec.Containers[i].Image = pod.Spec.Containers[i].Image
				updateNeeded = true
			}
		}
	}

	if updateNeeded {
		klog.InfoS("Updating Pod", "Pod.Namespace", found.Namespace, "Pod.Name", found.Name)
		return r.Update(ctx, found)
	}

	return nil
}

func (r *KVCacheReconciler) reconcileServices(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-rpc", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "vineyard-rpc", Port: 9600, TargetPort: intstr.FromInt(9600), Protocol: corev1.ProtocolTCP},
			},
			Selector: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       KVCacheLabelValueRoleCache,
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	if err := r.reconcileService(ctx, service); err != nil {
		return err
	}

	return nil
}

func (r *KVCacheReconciler) reconcileService(ctx context.Context, service *corev1.Service) error {
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		klog.InfoS("Creating a new Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		return r.Create(ctx, service)
	} else if err != nil {
		return err
	}

	// Update the found object and write the result back if there are any changes
	if needsUpdateService(service, found) {
		found.Spec.Ports = service.Spec.Ports
		found.Spec.Selector = service.Spec.Selector
		found.Spec.Type = service.Spec.Type
		klog.InfoS("Updating Service", "Service.Namespace", found.Namespace, "Service.Name", found.Name)
		return r.Update(ctx, found)
	}

	return nil
}

// needsUpdateService checks if the service spec of the new service differs from the existing one
func needsUpdateService(service, found *corev1.Service) bool {
	// Compare relevant spec fields
	return !reflect.DeepEqual(service.Spec.Ports, found.Spec.Ports) ||
		!reflect.DeepEqual(service.Spec.Selector, found.Spec.Selector) ||
		service.Spec.Type != found.Spec.Type
}

func (r *KVCacheReconciler) reconcileDeployment(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	envs := []corev1.EnvVar{
		{Name: "VINEYARDD_UID", Value: string(kvCache.ObjectMeta.UID)},
		{Name: "VINEYARDD_NAME", Value: kvCache.Name},
		{Name: "VINEYARDD_NAMESPACE", Value: kvCache.Namespace},
		{Name: "MY_NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "MY_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "MY_POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "MY_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "MY_POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "MY_HOST_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "USER", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
	}

	affinity := corev1.Affinity{}

	if value, ok := kvCache.Annotations[KVCacheAnnotationNodeAffinityGPUType]; ok {
		matchKey := KVCacheAnnotationNodeAffinityDefaultKey
		if key, exist := kvCache.Annotations[KVCacheAnnotationNodeAffinityKey]; exist {
			matchKey = key
		}

		nodeAffinity := &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      matchKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{value},
							},
						},
					},
				},
			},
		}
		affinity.NodeAffinity = nodeAffinity
	}

	if value, ok := kvCache.Annotations[KVCacheAnnotationPodAffinityKey]; ok {
		podAffinity := &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "model.aibrix.ai/name",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{value},
							},
						},
					},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		}
		affinity.PodAffinity = podAffinity
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kvCache.Name,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				KVCacheLabelKeyIdentifier: kvCache.Name,
				KVCacheLabelKeyRole:       "cache",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &kvCache.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				// TODO: update metadata
				MatchLabels: map[string]string{
					KVCacheLabelKeyIdentifier: kvCache.Name,
					KVCacheLabelKeyRole:       KVCacheLabelValueRoleCache,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						KVCacheLabelKeyIdentifier: kvCache.Name,
						KVCacheLabelKeyRole:       KVCacheLabelValueRoleCache,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "kvcache",
							Image: kvCache.Spec.Cache.Image,
							Ports: []corev1.ContainerPort{
								{Name: "rpc", ContainerPort: 9600, Protocol: corev1.ProtocolTCP},
							},
							Command: []string{
								"/bin/bash",
								"-c",
								fmt.Sprintf(`/usr/local/bin/vineyardd --sync_crds true --socket /var/run/vineyard.sock --size --stream_threshold 80 --etcd_cmd etcd --etcd_prefix /vineyard --etcd_endpoint http://%s-etcd-service:2379`, kvCache.Name),
							},
							Env: append(envs, kvCache.Spec.Cache.Env...),
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(kvCache.Spec.Cache.CPU),
									corev1.ResourceMemory: resource.MustParse(kvCache.Spec.Cache.Memory),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(kvCache.Spec.Cache.CPU),
									corev1.ResourceMemory: resource.MustParse(kvCache.Spec.Cache.Memory),
								},
							},
							ImagePullPolicy: corev1.PullPolicy(kvCache.Spec.Cache.ImagePullPolicy),
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"ls", "/var/run/vineyard.sock"},
									},
								},
							},
							LivenessProbe: &corev1.Probe{
								FailureThreshold: 3,
								PeriodSeconds:    60,
								SuccessThreshold: 1,
								TimeoutSeconds:   1,
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt(9600),
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "vineyard-socket", MountPath: "/var/run"},
								{Name: "shm", MountPath: "/dev/shm"},
								{Name: "log", MountPath: "/var/log/vineyard"},
							},
						},
					},
					Affinity: &affinity,
					Volumes: []corev1.Volume{
						{
							Name: "vineyard-socket",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: fmt.Sprintf("/var/run/vineyard-kubernetes/%s/%s", kvCache.Namespace, kvCache.Name),
								},
							},
						},
						{
							Name: "shm",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									Medium: "Memory",
								},
							},
						},
						{
							Name: "log",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	found := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		klog.InfoS("Creating a new Deployment", "Deployment.Namespace", deployment.Namespace, "Deployment.Name", deployment.Name)
		err = r.Create(ctx, deployment)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if needsUpdateDeployment(deployment, found) {
		found.Spec = deployment.Spec
		klog.InfoS("Updating Deployment", "Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)
		err = r.Update(ctx, found)
		if err != nil {
			return err
		}
	}

	return nil
}

// needsUpdateDeployment checks if the deployment spec of the new deployment differs from the existing one
// only image and replicas are considered at this moment.
func needsUpdateDeployment(deployment *appsv1.Deployment, found *appsv1.Deployment) bool {
	imageChanged := false
	for i, container := range found.Spec.Template.Spec.Containers {
		if len(deployment.Spec.Template.Spec.Containers) > i {
			if deployment.Spec.Template.Spec.Containers[i].Image != container.Image {
				// update the image
				found.Spec.Template.Spec.Containers[i].Image = deployment.Spec.Template.Spec.Containers[i].Image
				imageChanged = true
			}
		}
	}

	return !reflect.DeepEqual(deployment.Spec.Replicas, found.Spec.Replicas) || imageChanged
}
