package backends

import (
	"context"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type InfiniStoreReconciler struct {
	*BaseReconciler
}

func NewInfiniStoreReconciler(c client.Client) *InfiniStoreReconciler {
	return &InfiniStoreReconciler{&BaseReconciler{Client: c}}
}

func (v InfiniStoreReconciler) Reconcile(ctx context.Context, kv *orchestrationv1alpha1.KVCache) (reconcile.Result, error) {
	//TODO implement me
	panic("implement me")
}
