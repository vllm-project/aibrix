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
	"errors"
	"sync/atomic"

	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"

	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	v1alpha1scheme "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/scheme"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// KubernetesProvider implements Provider using Kubernetes informers.
// It watches Pods and ModelAdapters via the K8s API server.
//
// All data — both initial state and ongoing changes — is delivered
// via the Watch handler callback. Load() returns nil.
// The handler is wired directly into K8s informer callbacks with
// no intermediate channel or buffer.
type KubernetesProvider struct {
	config *rest.Config
}

// NewKubernetesProvider creates a new Kubernetes discovery provider.
func NewKubernetesProvider(config *rest.Config) *KubernetesProvider {
	return &KubernetesProvider{config: config}
}

// Type returns the provider type identifier.
func (p *KubernetesProvider) Type() string {
	return "kubernetes"
}

// Load returns nil — all data is delivered via Watch().
func (p *KubernetesProvider) Load() ([]any, error) {
	return nil, nil
}

// Watch starts K8s informers, waits for the initial list+sync to complete,
// delivers all existing objects via the handler, then returns.
// After Watch returns, the informers continue running and invoke the handler
// for ongoing changes until stopCh is closed.
//
// Delivery order: all Pods first (as EventAdd), then all ModelAdapters (as EventAdd).
// This ordering ensures pods exist in the cache before ModelAdapter handlers look them up.
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

	factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)
	crdFactory := crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)

	// synced gates live event delivery. During the initial list+sync phase,
	// informer callbacks are suppressed — initial objects are delivered
	// explicitly from the informer stores after sync completes.
	var synced atomic.Bool

	podInformer := factory.Core().V1().Pods().Informer()
	modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()

	registerHandlers := func(inf cache.SharedIndexInformer) error {
		_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if synced.Load() {
					handler(WatchEvent{Type: EventAdd, Object: obj})
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if synced.Load() {
					handler(WatchEvent{Type: EventUpdate, Object: newObj, OldObject: oldObj})
				}
			},
			DeleteFunc: func(obj interface{}) {
				if synced.Load() {
					handler(WatchEvent{Type: EventDelete, Object: obj})
				}
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

	// Start informers and wait for initial list+sync.
	factory.Start(stopCh)
	crdFactory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced, modelInformer.HasSynced) {
		return errors.New("timed out waiting for caches to sync")
	}

	// Deliver all existing objects — pods first, then adapters.
	pods := podInformer.GetStore().List()
	adapters := modelInformer.GetStore().List()
	for _, obj := range pods {
		handler(WatchEvent{Type: EventAdd, Object: obj})
	}
	for _, obj := range adapters {
		handler(WatchEvent{Type: EventAdd, Object: obj})
	}

	// Enable live event delivery from informer handlers.
	synced.Store(true)

	klog.InfoS("Kubernetes discovery provider initialized",
		"pods", len(pods), "modelAdapters", len(adapters))

	return nil
}

var _ Provider = (*KubernetesProvider)(nil)
