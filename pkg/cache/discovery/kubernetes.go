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

package discovery

import (
	"context"
	"errors"
	"time"

	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"

	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	v1alpha1scheme "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/scheme"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// KubernetesProvider implements Provider using Kubernetes informers.
type KubernetesProvider struct {
	config *rest.Config
	// watchModelClaims adds ModelClaim objects to what is watched.
	watchModelClaims bool
}

// NewKubernetesProvider creates a new Kubernetes discovery provider.
func NewKubernetesProvider(config *rest.Config) *KubernetesProvider {
	return &KubernetesProvider{config: config}
}

// WithModelClaims makes the provider watch ModelClaim objects as well. The
// gateway needs them to answer for a model that is claimed but not placed yet.
func (p *KubernetesProvider) WithModelClaims() *KubernetesProvider {
	p.watchModelClaims = true
	return p
}

// Type returns the provider type identifier.
func (p *KubernetesProvider) Type() string {
	return "kubernetes"
}

// Watch starts K8s informers with the handler wired directly into informer callbacks.
// Watch returns once the initial sync and reconcile are complete. After return,
// informer callbacks continue delivering ongoing changes asynchronously.
func (p *KubernetesProvider) Watch(handler EventHandler, stopCh <-chan struct{}) error {
	if err := v1alpha1scheme.AddToScheme(scheme.Scheme); err != nil {
		return err
	}

	k8sClientSet, err := kubernetes.NewForConfig(p.config)
	if err != nil {
		return err
	}

	crdClientSet, err := v1alpha1.NewForConfig(p.config)
	if err != nil {
		return err
	}

	// Currently watches all pods cluster-wide (same as the old initCacheInformers).
	factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)
	crdFactory := crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)

	podInformer := factory.Core().V1().Pods().Informer()
	modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()
	var claimInformer cache.SharedIndexInformer
	if p.watchModelClaims && canListModelClaims(crdClientSet) {
		claimInformer = crdFactory.Model().V1alpha1().ModelClaims().Informer()
	}

	// Wire handler directly into informer callbacks.
	// Events flow from the start — including during the initial list phase.
	registerHandlers := func(inf cache.SharedIndexInformer) error {
		_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				handler(WatchEvent{Type: EventAdd, Object: obj})
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				handler(WatchEvent{Type: EventUpdate, Object: newObj, OldObject: oldObj})
			},
			DeleteFunc: func(obj interface{}) {
				// Unwrap tombstones — K8s informers may deliver
				// cache.DeletedFinalStateUnknown when a delete event is missed.
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = tombstone.Obj
				}
				handler(WatchEvent{Type: EventDelete, Object: obj})
			},
		})
		return err
	}

	if err := registerHandlers(podInformer); err != nil {
		return err
	}
	if err := registerHandlers(modelInformer); err != nil {
		return err
	}
	if claimInformer != nil {
		if err := registerHandlers(claimInformer); err != nil {
			return err
		}
	}

	// Start informers and wait for initial list+sync.
	// During this phase, AddFunc fires for each existing object.
	factory.Start(stopCh)
	crdFactory.Start(stopCh)

	// ModelClaims are left out of the wait. They only tell a model that is
	// claimed but not placed yet from one that nobody serves. A gateway whose
	// role cannot list them yet should still start, and answer for such a
	// model as it did before.
	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced, modelInformer.HasSynced) {
		return errors.New("timed out waiting for caches to sync")
	}

	// Post-sync reconcile: re-emit all ModelAdapters to fix ordering.
	// During initial sync, Pod and ModelAdapter informers list concurrently.
	// A ModelAdapter's AddFunc may fire before its pods' AddFunc, causing
	// the pod-model mapping to be missed. Re-emitting adapters after sync
	// ensures all mappings are established (addModelAdapter is idempotent).
	adapters := modelInformer.GetStore().List()
	for _, obj := range adapters {
		handler(WatchEvent{Type: EventAdd, Object: obj})
	}

	klog.InfoS("Kubernetes discovery provider initialized",
		"pods", len(podInformer.GetStore().List()), "modelAdapters", len(adapters))

	return nil
}

// canListModelClaims lists ModelClaims once before they are watched. A role
// that may not list them, or a cluster without the ModelClaim CRD, is logged
// here once, where an informer would log it every minute for as long as the
// process runs. Any other error is left to the informer, which retries it.
func canListModelClaims(client v1alpha1.Interface) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := client.ModelV1alpha1().ModelClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{Limit: 1})
	if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
		klog.InfoS("Not watching ModelClaims: a model that is claimed but not placed yet "+
			"is answered as one that does not exist", "err", err)
		return false
	}
	return true
}

var _ Provider = (*KubernetesProvider)(nil)
