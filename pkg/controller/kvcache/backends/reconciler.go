package backends

import (
	"context"
	"fmt"
	"github.com/vllm-project/aibrix/pkg/constants"
	"reflect"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	KVCacheBackendVineyard    = "vineyard"
	KVCacheBackendHPKV        = "hpkv"
	KVCacheBackendInfinistore = "infinistore"
	KVCacheBackendDefault     = KVCacheBackendVineyard
)

type BackendReconciler interface {
	Reconcile(ctx context.Context, kv *orchestrationv1alpha1.KVCache) (reconcile.Result, error)
}

type BaseReconciler struct {
	client.Client
}

func (r *BaseReconciler) ReconcilePodObject(ctx context.Context, desired *corev1.Pod) error {
	found := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating a new Pod", "Pod.Namespace", desired.Namespace, "Pod.Name", desired.Name)
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}

	// Check if the images need to be updated.
	// most Pod fields are mutable, so we just compare image here. We can extends to tolerations or other fields later.
	updateNeeded := false
	for i, container := range found.Spec.Containers {
		if len(desired.Spec.Containers) > i {
			if desired.Spec.Containers[i].Image != container.Image {
				// update the image
				found.Spec.Containers[i].Image = desired.Spec.Containers[i].Image
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

func (r *BaseReconciler) ReconcileDeploymentObject(ctx context.Context, desired *appsv1.Deployment) error {
	found := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating Deployment", "Name", desired.Name)
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	} else if needsUpdateDeployment(desired, found) {
		found.Spec = desired.Spec
		klog.InfoS("Updating Deployment", "Name", desired.Name)
		return r.Update(ctx, found)
	}
	return nil
}

func (r *BaseReconciler) ReconcileServiceObject(ctx context.Context, service *corev1.Service) error {
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
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

func (r *BaseReconciler) ReconcileStatefulsetObject(ctx context.Context, sts *appsv1.StatefulSet) error {
	found := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating a new StatefulSet", "Sts.Namespace", sts.Namespace, "Sts.Name", sts.Name)
		return r.Create(ctx, sts)
	} else if err != nil {
		return err
	}

	// Update the found object and write the result back if there are any changes
	if needsUpdateStatefulset(sts, found) {
		found.Spec = sts.Spec
		klog.InfoS("Updating Statefulset", "Sts.Namespace", found.Namespace, "Sts.Name", found.Name)
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

// needsUpdateStatefulset checks if the StatefulSet spec of the new Statefulset differs from the existing one
// only image and replicas are considered at this moment.
func needsUpdateStatefulset(sts *appsv1.StatefulSet, found *appsv1.StatefulSet) bool {
	imageChanged := false
	for i, container := range found.Spec.Template.Spec.Containers {
		if len(sts.Spec.Template.Spec.Containers) > i {
			if sts.Spec.Template.Spec.Containers[i].Image != container.Image {
				// update the image
				found.Spec.Template.Spec.Containers[i].Image = sts.Spec.Template.Spec.Containers[i].Image
				imageChanged = true
			}
		}
	}

	return !reflect.DeepEqual(sts.Spec.Replicas, found.Spec.Replicas) || imageChanged
}

func (r *BaseReconciler) reconcileRedisService(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	// We only support etcd at this moment, redis will be supported later.
	replicas := int(kvCache.Spec.Metadata.Redis.Replicas)
	if replicas != 1 {
		klog.Warningf("replica %d > 1 is not supported at this moment, we will change to single replica", replicas)
	}

	pod := buildRedisPod(kvCache)
	if err := r.ReconcilePodObject(ctx, pod); err != nil {
		return err
	}

	// Create or update the etcd service for each pod
	etcdService := buildRedisService(kvCache)
	if err := r.ReconcileServiceObject(ctx, etcdService); err != nil {
		return err
	}

	return nil
}
func buildRedisService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	redisServiceName := fmt.Sprintf("%s-redis", kvCache.Name)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisServiceName,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "service",
					Port:       6379,
					TargetPort: intstr.FromInt32(6379),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	return svc
}

func buildRedisPod(kvCache *orchestrationv1alpha1.KVCache) *corev1.Pod {
	image := kvCache.Spec.Cache.Image
	if kvCache.Spec.Metadata.Redis.Image != "" {
		image = kvCache.Spec.Metadata.Redis.Image
	}

	redisPodName := fmt.Sprintf("%s-redis", kvCache.Name)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisPodName,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "redis",
					Image: image,
					Ports: []corev1.ContainerPort{
						{
							Name:          "redis",
							ContainerPort: 6379,
							Protocol:      corev1.ProtocolTCP,
						},
					},
					Command: []string{
						"redis-server",
					},
					// You can also add volumeMounts, env vars, etc. if needed.
				},
			},
		},
	}

	return pod
}
