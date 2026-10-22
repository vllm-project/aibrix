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
	"fmt"
	"strings"
	"sync"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// KubernetesProvider implements Provider using Kubernetes informers.
type KubernetesProvider struct {
	config             *rest.Config
	namespace          string
	startupTimeout     time.Duration
	watchModelAdapters bool
	watchModelClaims   bool
	modelListHealth    *ModelListHealth
}

// NewKubernetesProvider creates discovery restricted to namespace. Watch rejects
// an empty or invalid namespace, nil config, inaccessible namespace, or missing
// list/watch permissions. Startup is bounded to one minute; stopCh owns lifetime.
func NewKubernetesProvider(config *rest.Config, namespace string) *KubernetesProvider {
	return &KubernetesProvider{
		config: config, namespace: namespace, startupTimeout: time.Minute, watchModelAdapters: true,
	}
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

// ValidateNamespace rejects missing and invalid installation namespaces without
// trimming or substituting a default. It performs no Kubernetes API requests.
func ValidateNamespace(namespace string) error {
	if problems := validation.ValidateNamespaceName(namespace, false); len(problems) > 0 {
		return fmt.Errorf("installation namespace %q is required and must be a valid Kubernetes namespace: %s",
			namespace, strings.Join(problems, "; "))
	}
	return nil
}

// Type returns the provider type identifier.
func (p *KubernetesProvider) Type() string {
	return "kubernetes"
}

// Watch starts K8s informers with the handler wired directly into informer callbacks.
// Watch returns once the initial sync and reconcile are complete. After return,
// informer callbacks continue delivering ongoing changes asynchronously.
func (p *KubernetesProvider) Watch(handler EventHandler, stopCh <-chan struct{}) error {
	if handler == nil {
		return fmt.Errorf("discovery event handler is required")
	}
	return p.watch(func(event WatchEvent) bool {
		handler(event)
		return true
	}, stopCh)
}

// WatchApplied is Watch with a handler that reports whether a cache event was applied.
func (p *KubernetesProvider) WatchApplied(handler func(WatchEvent) bool, stopCh <-chan struct{}) error {
	return p.watch(handler, stopCh)
}

func (p *KubernetesProvider) configureModelListHealth(k8sClientSet kubernetes.Interface, crdClientSet v1alpha1.Interface) {
	health := p.modelListHealth
	if health == nil {
		return
	}
	health.snapshot = func(ctx context.Context) (sourceVersions, error) {
		pods, err := listPodVersions(ctx, k8sClientSet, p.namespace)
		if err != nil {
			return sourceVersions{}, err
		}
		adapters := make(map[string]objectVersion)
		if p.watchModelAdapters {
			adapters, err = listAdapterVersions(ctx, crdClientSet, p.namespace)
			if err != nil {
				return sourceVersions{}, err
			}
		}
		return sourceVersions{pods: pods, adapters: adapters}, nil
	}
}

func (p *KubernetesProvider) watch(handler func(WatchEvent) bool, stopCh <-chan struct{}) error {
	if err := ValidateNamespace(p.namespace); err != nil {
		return err
	}
	if p.config == nil {
		return fmt.Errorf("kubernetes configuration is required")
	}
	if handler == nil {
		return fmt.Errorf("discovery event handler is required")
	}
	ctx, cancel := context.WithCancel(wait.ContextForChannel(stopCh))
	ready := false
	defer func() {
		if !ready {
			cancel()
		}
	}()
	startup, cancelStartup := context.WithTimeout(ctx, p.startupTimeout)
	defer cancelStartup()

	k8sClientSet, err := kubernetes.NewForConfig(p.config)
	if err != nil {
		return err
	}

	var crdClientSet v1alpha1.Interface
	if p.watchModelAdapters || p.watchModelClaims {
		crdClientSet, err = v1alpha1.NewForConfig(p.config)
		if err != nil {
			return err
		}
	}
	p.configureModelListHealth(k8sClientSet, crdClientSet)

	if err := p.checkAccess(startup, k8sClientSet, crdClientSet); err != nil {
		return err
	}

	pods := k8sClientSet.CoreV1().Pods(p.namespace)
	// ListWatch binds requests to the provider lifetime as well as the explicit
	// namespace, so failed startup can cancel even an in-flight initial LIST.
	podInformer := cache.NewSharedIndexInformer(&cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return pods.List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return pods.Watch(ctx, options)
		},
	}, &corev1.Pod{}, 0, cache.Indexers{})
	requiredInformers := []cache.SharedIndexInformer{podInformer}
	var modelInformer cache.SharedIndexInformer
	if p.watchModelAdapters {
		adapters := crdClientSet.ModelV1alpha1().ModelAdapters(p.namespace)
		modelInformer = cache.NewSharedIndexInformer(&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return adapters.List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return adapters.Watch(ctx, options)
			},
		}, &modelv1alpha1.ModelAdapter{}, 0, cache.Indexers{})
		requiredInformers = append(requiredInformers, modelInformer)
	}
	synced, err := p.registerHandlers(handler, requiredInformers...)
	if err != nil {
		return err
	}
	allInformers := append([]cache.SharedIndexInformer(nil), requiredInformers...)
	// Claims remain optional, as upstream: missing CRDs or permissions do not
	// prevent Pod-based serving. Their discovery still uses the same namespace.
	if p.watchModelClaims && canListModelClaims(startup, crdClientSet, p.namespace) {
		claims := crdClientSet.ModelV1alpha1().ModelClaims(p.namespace)
		claimInformer := cache.NewSharedIndexInformer(&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return claims.List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return claims.Watch(ctx, options)
			},
		}, &modelv1alpha1.ModelClaim{}, 0, cache.Indexers{})
		if _, err := p.registerHandlers(handler, claimInformer); err != nil {
			return err
		}
		allInformers = append(allInformers, claimInformer)
	}

	// Start informers and wait for initial list+sync.
	// During this phase, AddFunc fires for each existing object.
	// Surface initial LIST/WATCH errors instead of retrying forever at startup.
	watchErrors := make(chan error, 1)
	for _, informer := range requiredInformers {
		if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
			if p.modelListHealth != nil {
				p.modelListHealth.Invalidate()
			}
			select {
			case watchErrors <- err:
			default:
			}
			klog.ErrorS(err, "Installation discovery watch failed", "namespace", p.namespace)
			cancelStartup()
		}); err != nil {
			return err
		}
	}
	var running sync.WaitGroup
	for _, informer := range allInformers {
		running.Add(1)
		go func(informer cache.SharedIndexInformer) {
			defer running.Done()
			informer.Run(ctx.Done())
		}(informer)
	}
	defer func() {
		if !ready {
			cancel()
			running.Wait()
		}
	}()

	if !cache.WaitForCacheSync(startup.Done(), synced...) {
		select {
		case err := <-watchErrors:
			return fmt.Errorf("sync installation namespace %q: %w", p.namespace, err)
		default:
			return fmt.Errorf("sync installation namespace %q: %w", p.namespace, startup.Err())
		}
	}

	// Post-sync reconcile: re-emit all ModelAdapters to fix ordering.
	// During initial sync, Pod and ModelAdapter informers list concurrently.
	// A ModelAdapter's AddFunc may fire before its pods' AddFunc, causing
	// the pod-model mapping to be missed. Re-emitting adapters after sync
	// ensures all mappings are established (addModelAdapter is idempotent).
	adapterCount := 0
	if modelInformer != nil {
		adapterObjects := modelInformer.GetStore().List()
		adapterCount = len(adapterObjects)
		for _, obj := range adapterObjects {
			p.deliver(handler, WatchEvent{Type: EventAdd, Object: obj})
		}
	}
	klog.InfoS("Kubernetes discovery provider initialized",
		"namespace", p.namespace, "pods", len(podInformer.GetStore().List()), "modelAdapters", adapterCount)

	if err := startup.Err(); err != nil {
		return fmt.Errorf("initialize installation namespace %q: %w", p.namespace, err)
	}
	ready = true
	go func() {
		<-ctx.Done()
		running.Wait()
		cancel()
	}()
	return nil
}

// checkAccess verifies namespace existence and WATCH authorization. LIST is
// checked by the initial informer synchronization, avoiding a redundant list.
func (p *KubernetesProvider) checkAccess(ctx context.Context, pods kubernetes.Interface, adapters v1alpha1.Interface) error {
	// A list of a nonexistent namespace can succeed with no objects. Verify the
	// namespace itself, using GET permission restricted to this exact name.
	if _, err := pods.CoreV1().Namespaces().Get(ctx, p.namespace, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("get installation namespace %q: %w", p.namespace, err)
	}
	// Cache sync only proves LIST succeeded. Check WATCH too before accepting
	// traffic, since list-only RBAC would otherwise appear ready indefinitely.
	podWatch, err := pods.CoreV1().Pods(p.namespace).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("watch pods in installation namespace %q: %w", p.namespace, err)
	}
	podWatch.Stop()
	if p.watchModelAdapters {
		adapterWatch, err := adapters.ModelV1alpha1().ModelAdapters(p.namespace).Watch(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("watch modeladapters in installation namespace %q: %w", p.namespace, err)
		}
		adapterWatch.Stop()
	}

	return nil
}

func (p *KubernetesProvider) registerHandlers(handler func(WatchEvent) bool, informers ...cache.SharedIndexInformer) ([]cache.InformerSynced, error) {
	var synced []cache.InformerSynced
	for _, informer := range informers {
		registration, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				p.deliver(handler, WatchEvent{Type: EventAdd, Object: obj})
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				p.deliver(handler, WatchEvent{Type: EventUpdate, Object: newObj, OldObject: oldObj})
			},
			DeleteFunc: func(obj interface{}) {
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = tombstone.Obj
				}
				p.deliver(handler, WatchEvent{Type: EventDelete, Object: obj})
			},
		})
		if err != nil {
			return nil, err
		}
		synced = append(synced, registration.HasSynced)
	}
	return synced, nil
}

// Check events as well as request scope so unexpected objects cannot enter the
// routing cache, including deletes and adapter reconciliation after initial sync.
func (p *KubernetesProvider) deliver(handler func(WatchEvent) bool, event WatchEvent) {
	object := discoveryMetadata(event.Object)
	if object == nil {
		if p.modelListHealth != nil {
			p.modelListHealth.Invalidate()
		}
		return
	}
	if object.GetNamespace() != p.namespace {
		return
	}
	if event.Type == EventUpdate {
		old := discoveryMetadata(event.OldObject)
		if old == nil || old.GetNamespace() != p.namespace {
			return
		}
	}
	if _, claim := event.Object.(*modelv1alpha1.ModelClaim); !claim && p.modelListHealth != nil {
		p.modelListHealth.Apply(event, handler)
		return
	}
	handler(event)
}

// Only known non-nil discovery objects may enter the cache or health record.
func discoveryMetadata(object interface{}) metav1.Object {
	switch typed := object.(type) {
	case *corev1.Pod:
		if typed != nil {
			return typed
		}
	case *modelv1alpha1.ModelAdapter:
		if typed != nil {
			return typed
		}
	case *modelv1alpha1.ModelClaim:
		if typed != nil {
			return typed
		}
	}
	return nil
}

// canListModelClaims lists ModelClaims once before they are watched. A role
// that may not list them, or a cluster without the ModelClaim CRD, is logged
// here once, where an informer would log it every minute for as long as the
// process runs. Any other error is left to the informer, which retries it.
func canListModelClaims(parent context.Context, client v1alpha1.Interface, namespace string) bool {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	_, err := client.ModelV1alpha1().ModelClaims(namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
		klog.InfoS("Not watching ModelClaims: a model that is claimed but not placed yet "+
			"is answered as one that does not exist", "err", err)
		return false
	}
	return true
}

var _ Provider = (*KubernetesProvider)(nil)
