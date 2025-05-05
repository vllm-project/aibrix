/*
Copyright 2025 The Aibrix Team.

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

package backends

import (
	"context"
	"errors"
	"fmt"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type DistributedReconciler struct {
	client.Client
	*BaseReconciler
	Backend KVCacheBackend
}

func NewDistributedReconciler(c client.Client, backend string) *DistributedReconciler {
	reconciler := &DistributedReconciler{
		Client:         c,
		BaseReconciler: &BaseReconciler{Client: c},
	}

	if backend == constants.KVCacheBackendInfinistore {
		reconciler.Backend = InfiniStoreBackend{}
	} else if backend == constants.KVCacheBackendHPKV {
		reconciler.Backend = HpKVBackend{}
	} else {
		panic(fmt.Sprintf("unsupported backend: %s", backend))
	}
	return reconciler
}

func (r *DistributedReconciler) Reconcile(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) (ctrl.Result, error) {
	if err := r.Backend.ValidateObject(kvCache); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileRedisService(ctx, kvCache); err != nil {
		return reconcile.Result{}, err
	}

	// Handle infinistore kvCache Deployment
	if err := r.ReconcileStatefulsetObject(ctx, r.Backend.BuildCacheStatefulSet(kvCache)); err != nil {
		return ctrl.Result{}, err
	}

	// Handle Hpkv/infinistore Services
	if err := r.ReconcileServiceObject(ctx, r.Backend.BuildService(kvCache)); err != nil {
		return ctrl.Result{}, err
	}

	// Handle Hpkv/infinistore watcher Pod
	if err := r.ReconcilePodObject(ctx, r.Backend.BuildWatcherPod(kvCache)); err != nil {
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

func (r *DistributedReconciler) reconcileRedisService(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	// We only support etcd at this moment, redis will be supported later.
	replicas := int(kvCache.Spec.Metadata.Redis.Runtime.Replicas)
	if replicas != 1 {
		klog.Warningf("replica %d > 1 is not supported at this moment, we will change to single replica", replicas)
	}

	pod := r.Backend.BuildMetadataPod(kvCache)
	if err := r.ReconcilePodObject(ctx, pod); err != nil {
		return err
	}

	// Create or update the etcd service for each pod
	etcdService := r.Backend.BuildMetadataService(kvCache)
	if err := r.ReconcileServiceObject(ctx, etcdService); err != nil {
		return err
	}

	return nil
}
