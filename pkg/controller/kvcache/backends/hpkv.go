package backends

import (
	"context"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type HpKVReconciler struct {
	*BaseReconciler
}

func NewHpKVReconciler(c client.Client) *HpKVReconciler {
	return &HpKVReconciler{&BaseReconciler{Client: c}}
}

func (v HpKVReconciler) Reconcile(ctx context.Context, kv *orchestrationv1alpha1.KVCache) (reconcile.Result, error) {
	//TODO implement me
	panic("implement me")
}
