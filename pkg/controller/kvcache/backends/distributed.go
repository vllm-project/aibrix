package backends

import (
	"context"
	"errors"
	"fmt"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type DistributedReconciler struct {
	client.Client
	*BaseReconciler
	Scheme  *runtime.Scheme
	Backend KVCacheBackend
}

type KVCacheBackend interface {
	Name() string
	ValidateObject(*orchestrationv1alpha1.KVCache) error
	BuildMetadataWorkload(*orchestrationv1alpha1.KVCache) *appsv1.Deployment
	BuildMetadataService(*orchestrationv1alpha1.KVCache) *corev1.Service
	BuildWatcherPod(*orchestrationv1alpha1.KVCache) *corev1.Pod
	BuildCacheStatefulSet(*orchestrationv1alpha1.KVCache) *appsv1.StatefulSet
	BuildService(*orchestrationv1alpha1.KVCache) *corev1.Service
}

func NewDistributedReconciler(c client.Client, scheme *runtime.Scheme, backend string) *DistributedReconciler {
	reconciler := &DistributedReconciler{
		Client:         c,
		Scheme:         scheme,
		BaseReconciler: &BaseReconciler{Client: c},
	}

	if backend == KVCacheBackendInfinistore {
		reconciler.Backend = InfiniStoreBackend{}
	} else if backend == KVCacheBackendHPKV {
		reconciler.Backend = HpKVBackend{}
	} else {
		panic(fmt.Sprintf("unsupported backend: %s", backend))
	}
	return reconciler
}

func (r *DistributedReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var kvCache orchestrationv1alpha1.KVCache
	if err := r.Get(ctx, req.NamespacedName, &kvCache); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.Backend.ValidateObject(&kvCache); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileRedisService(ctx, &kvCache); err != nil {
		return reconcile.Result{}, err
	}

	// Handle infinistore kvCache Deployment
	if err := r.ReconcileStatefulsetObject(ctx, r.Backend.BuildCacheStatefulSet(&kvCache)); err != nil {
		return ctrl.Result{}, err
	}
	// Handle Hpkv/infinistore Services
	if err := r.ReconcileServiceObject(ctx, r.Backend.BuildService(&kvCache)); err != nil {
		return ctrl.Result{}, err
	}
	// Handle Hpkv/infinistore watcher Pod
	if err := r.ReconcilePodObject(ctx, r.Backend.BuildWatcherPod(&kvCache)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *DistributedReconciler) reconcileMetadataService(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	if kvCache.Spec.Metadata != nil && kvCache.Spec.Metadata.Etcd == nil && kvCache.Spec.Metadata.Redis == nil {
		return errors.New("either etcd or redis configuration is required")
	}

	if kvCache.Spec.Metadata.Redis != nil {
		return r.reconcileRedisService(ctx, kvCache)
	}

	return nil
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
