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
	"sync/atomic"
	"time"

	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"

	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	v1alpha1scheme "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/scheme"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/metadata"
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
	// podInformer holds the Pod informer once Watch has created it, for
	// PodWatchResourceVersion.
	podInformer atomic.Pointer[podInformerRef]
}

type podInformerRef struct{ informer cache.SharedIndexInformer }

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

// PodWatchResourceVersion returns the resource version the Pod informer has
// reached: the reflector updates it after every list and every watch event,
// bookmarks included, so it stops moving when the watch stops delivering. It
// is "" before Watch starts the informer. It makes no API request.
func (p *KubernetesProvider) PodWatchResourceVersion() string {
	ref := p.podInformer.Load()
	if ref == nil {
		return ""
	}
	return ref.informer.LastSyncResourceVersion()
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

func (p *KubernetesProvider) configureModelListHealth(metadataClient metadata.Interface) {
	health := p.modelListHealth
	if health == nil {
		return
	}
	health.snapshot = func(ctx context.Context) (sourceVersions, error) {
		pods, err := listPodVersions(ctx, metadataClient)
		if err != nil {
			return sourceVersions{}, err
		}
		adapters := make(map[string]objectVersion)
		if p.watchModelAdapters {
			adapters, err = listAdapterVersions(ctx, metadataClient)
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
	p.podInformer.Store(&podInformerRef{informer: podInformer})
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
	if p.modelListHealth != nil {
		// The metadata client has its own rate limiter, built from the same
		// QPS and Burst, so verification LISTs don't compete with informer
		// relists for tokens.
		metadataClient, err := metadata.NewForConfig(p.config)
		if err != nil {
			return err
		}
		p.configureModelListHealth(metadataClient)
	}

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
		adapterCount = reemitModelAdapters(modelInformer.GetStore(), func(ev WatchEvent) {
			if p.modelListHealth != nil {
				p.modelListHealth.Apply(ev, handler)
			} else {
				handler(ev)
			}
		}, stopCh)
	}

	klog.InfoS("Kubernetes discovery provider initialized",
		"pods", len(podInformer.GetStore().List()), "modelAdapters", adapterCount)

	return nil
}

// reemitModelAdapters replays each ModelAdapter in store as EventAdd and
// returns how many it emitted. After each emit it re-reads the key and emits
// a compensating Delete, Add or Update until a re-read matches the object last
// applied, or stop closes. Each emit is applied synchronously by the handler
// before the next re-read. client-go updates the store before handlers run on
// the informer's listener goroutine, so the loop ends only when a re-read
// matches what was applied: any later store change is ordered after that apply
// and its notification is delivered after it. Buffered earlier notifications
// re-assert states in store order, so the last delivered state equals the
// store's. Stop ends the loop on shutdown.
func reemitModelAdapters(store cache.Store, emit func(WatchEvent), stop <-chan struct{}) int {
	emitted := 0
	for _, key := range store.ListKeys() {
		current, exists, err := store.GetByKey(key)
		if err != nil {
			klog.V(4).InfoS("Skipping ModelAdapter re-emit: cannot read from store", "key", key, "err", err)
			continue
		}
		if !exists {
			klog.V(4).InfoS("Skipping ModelAdapter re-emit: deleted since snapshot", "key", key)
			continue
		}
		emit(WatchEvent{Type: EventAdd, Object: current})
		emitted++

		applied := current
		settled := false
		for !settled {
			select {
			case <-stop:
				return emitted
			default:
			}
			latest, exists, err := store.GetByKey(key)
			switch {
			case err != nil:
				klog.V(4).InfoS("Stopping ModelAdapter re-emit: cannot read from store", "key", key, "err", err)
				settled = true
			case !exists && applied == nil:
				settled = true
			case !exists:
				// A recreate may land before the next read, so keep checking.
				emit(WatchEvent{Type: EventDelete, Object: applied})
				applied = nil
			case applied == nil:
				emit(WatchEvent{Type: EventAdd, Object: latest})
				applied = latest
			default:
				same, ok := sameAdapterVersion(applied, latest)
				switch {
				case !ok:
					// Without metadata there is no version to compare, so the
					// key is treated as settled rather than re-emitted forever.
					settled = true
				case same:
					settled = true
				default:
					emit(WatchEvent{Type: EventUpdate, Object: latest, OldObject: applied})
					applied = latest
				}
			}
		}
	}
	return emitted
}

// sameAdapterVersion reports whether a and b are the same ModelAdapter at the
// same resource version. ok is false if either object has no metadata.
func sameAdapterVersion(a, b any) (same, ok bool) {
	am, err := meta.Accessor(a)
	if err != nil {
		return false, false
	}
	bm, err := meta.Accessor(b)
	if err != nil {
		return false, false
	}
	return am.GetUID() == bm.GetUID() && am.GetResourceVersion() == bm.GetResourceVersion(), true
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
