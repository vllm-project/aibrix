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
	// watchModelAdapters keeps the existing adapter discovery behavior by default.
	watchModelAdapters bool
	// watchModelClaims adds ModelClaim objects to what is watched.
	watchModelClaims bool
	modelListHealth  *ModelListHealth
}

// NewKubernetesProvider creates a new Kubernetes discovery provider.
func NewKubernetesProvider(config *rest.Config) *KubernetesProvider {
	return &KubernetesProvider{config: config, watchModelAdapters: true}
}

// WithModelAdapters controls whether the provider lists and watches ModelAdapters.
func (p *KubernetesProvider) WithModelAdapters(enabled bool) *KubernetesProvider {
	p.watchModelAdapters = enabled
	return p
}

// WithModelClaims makes the provider watch ModelClaim objects as well. The
// gateway needs them to answer for a model that is claimed but not placed yet.
func (p *KubernetesProvider) WithModelClaims() *KubernetesProvider {
	p.watchModelClaims = true
	return p
}

// WithModelListHealth enables on-demand verification of the model-list cache.
// A positive maxAge is required; callers select the bound they need.
func (p *KubernetesProvider) WithModelListHealth(maxAge time.Duration) *KubernetesProvider {
	p.modelListHealth = newModelListHealth(maxAge)
	return p
}

// ModelListHealth returns the verification state after WatchApplied starts.
func (p *KubernetesProvider) ModelListHealth() *ModelListHealth {
	return p.modelListHealth
}

// Type returns the provider type identifier.
func (p *KubernetesProvider) Type() string {
	return "kubernetes"
}

// Watch starts K8s informers with the handler wired directly into informer callbacks.
// Watch returns once the initial sync and reconcile are complete. After return,
// informer callbacks continue delivering ongoing changes asynchronously.
func (p *KubernetesProvider) Watch(handler EventHandler, stopCh <-chan struct{}) error {
	return p.watch(func(ev WatchEvent) bool {
		handler(ev)
		return true
	}, stopCh)
}

// WatchApplied is Watch with a handler that reports whether a cache event was
// applied. Use it when model-list verification is enabled.
func (p *KubernetesProvider) WatchApplied(handler func(WatchEvent) bool, stopCh <-chan struct{}) error {
	return p.watch(handler, stopCh)
}

func (p *KubernetesProvider) configureModelListHealth(k8sClientSet kubernetes.Interface, crdClientSet v1alpha1.Interface) {
	health := p.modelListHealth
	if health == nil {
		return
	}
	health.snapshot = func(ctx context.Context) (sourceVersions, error) {
		pods, err := listPodVersions(ctx, k8sClientSet)
		if err != nil {
			return sourceVersions{}, err
		}
		adapters := make(map[string]objectVersion)
		if p.watchModelAdapters {
			adapters, err = listAdapterVersions(ctx, crdClientSet)
			if err != nil {
				return sourceVersions{}, err
			}
		}
		return sourceVersions{pods: pods, adapters: adapters}, nil
	}
}

func (p *KubernetesProvider) watch(handler func(WatchEvent) bool, stopCh <-chan struct{}) error {
	if err := v1alpha1scheme.AddToScheme(scheme.Scheme); err != nil {
		return err
	}

	k8sClientSet, err := kubernetes.NewForConfig(p.config)
	if err != nil {
		return err
	}

	// Currently watches all pods cluster-wide (same as the old initCacheInformers).
	factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)

	podInformer := factory.Core().V1().Pods().Informer()
	var modelInformer cache.SharedIndexInformer
	var claimInformer cache.SharedIndexInformer
	var crdFactory crdinformers.SharedInformerFactory
	var crdClientSet v1alpha1.Interface
	if p.watchModelAdapters || p.watchModelClaims {
		crdClientSet, err = v1alpha1.NewForConfig(p.config)
		if err != nil {
			return err
		}
		crdFactory = crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)
		if p.watchModelAdapters {
			modelInformer = crdFactory.Model().V1alpha1().ModelAdapters().Informer()
		}
		if p.watchModelClaims && canListModelClaims(crdClientSet) {
			claimInformer = crdFactory.Model().V1alpha1().ModelClaims().Informer()
		}
	}
	p.configureModelListHealth(k8sClientSet, crdClientSet)

	// Wire handler directly into informer callbacks.
	// Events flow from the start — including during the initial list phase.
	var requiredHandlers []cache.ResourceEventHandlerRegistration
	registerHandlers := func(inf cache.SharedIndexInformer, required bool) error {
		deliver := func(ev WatchEvent) {
			if required && p.modelListHealth != nil {
				p.modelListHealth.Apply(ev, handler)
			} else {
				handler(ev)
			}
		}
		registration, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				deliver(WatchEvent{Type: EventAdd, Object: obj})
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				deliver(WatchEvent{Type: EventUpdate, Object: newObj, OldObject: oldObj})
			},
			DeleteFunc: func(obj interface{}) {
				// Unwrap tombstones — K8s informers may deliver
				// cache.DeletedFinalStateUnknown when a delete event is missed.
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = tombstone.Obj
				}
				deliver(WatchEvent{Type: EventDelete, Object: obj})
			},
		})
		if err != nil {
			return err
		}
		if required && p.modelListHealth != nil {
			requiredHandlers = append(requiredHandlers, registration)
			if err := inf.SetWatchErrorHandler(func(_ *cache.Reflector, watchErr error) {
				p.modelListHealth.Invalidate()
				klog.ErrorS(watchErr, "Model-list discovery watch failed")
			}); err != nil {
				return err
			}
		}
		return nil
	}

	if err := registerHandlers(podInformer, true); err != nil {
		return err
	}
	if modelInformer != nil {
		if err := registerHandlers(modelInformer, true); err != nil {
			return err
		}
	}
	if claimInformer != nil {
		if err := registerHandlers(claimInformer, false); err != nil {
			return err
		}
	}

	// Start informers and wait for initial list+sync.
	// During this phase, AddFunc fires for each existing object.
	factory.Start(stopCh)
	if crdFactory != nil {
		crdFactory.Start(stopCh)
	}

	// ModelClaims are left out of the wait. They only tell a model that is
	// claimed but not placed yet from one that nobody serves. A gateway whose
	// role cannot list them yet should still start, and answer for such a
	// model as it did before.
	requiredSync := []cache.InformerSynced{podInformer.HasSynced}
	if modelInformer != nil {
		requiredSync = append(requiredSync, modelInformer.HasSynced)
	}
	for _, registration := range requiredHandlers {
		requiredSync = append(requiredSync, registration.HasSynced)
	}
	if !cache.WaitForCacheSync(stopCh, requiredSync...) {
		return errors.New("timed out waiting for caches to sync")
	}

	// Post-sync reconcile: re-emit all ModelAdapters to fix ordering.
	// During initial sync, Pod and ModelAdapter informers list concurrently.
	// A ModelAdapter's AddFunc may fire before its pods' AddFunc, causing
	// the pod-model mapping to be missed. Re-emitting adapters after sync
	// ensures all mappings are established (addModelAdapter is idempotent).
	adapterCount := 0
	if modelInformer != nil {
		adapters := modelInformer.GetStore().List()
		adapterCount = len(adapters)
		for _, obj := range adapters {
			ev := WatchEvent{Type: EventAdd, Object: obj}
			if p.modelListHealth != nil {
				p.modelListHealth.Apply(ev, handler)
			} else {
				handler(ev)
			}
		}
	}

	klog.InfoS("Kubernetes discovery provider initialized",
		"pods", len(podInformer.GetStore().List()), "modelAdapters", adapterCount)

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
