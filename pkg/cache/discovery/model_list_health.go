/*
Copyright 2026 The Aibrix Team.

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
	"fmt"
	"sync"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
)

const (
	// modelListPageSize is the LIST page size for the consistency check. Each
	// page costs one token from the client rate limiter, and the whole snapshot
	// must finish within modelListVerificationTimeout. At the default
	// --kube-api-qps 5 and --kube-api-burst 10, the 10 s budget allows about
	// 60 pages, so 500-item pages failed above about 30,000 Pods. A metadata
	// item is under 1 KB, so a 5,000-item page is a few MB, and 100,000 Pods
	// need 20 pages.
	modelListPageSize            = 5000
	modelListVerificationTimeout = 10 * time.Second
	modelListVerificationRetry   = 5 * time.Second
)

// ErrModelListDiscoveryUnavailable means the model cache cannot currently be
// verified against the required Kubernetes discovery sources.
var ErrModelListDiscoveryUnavailable = errors.New("model discovery is unavailable")

// ErrModelListVerificationDeferred is returned, without a Kubernetes request,
// while a recent failed verification's retry delay runs. It wraps
// ErrModelListDiscoveryUnavailable, so callers that only need "unavailable"
// can keep matching that.
var ErrModelListVerificationDeferred = fmt.Errorf("%w: retrying after a recent failed verification",
	ErrModelListDiscoveryUnavailable)

type objectVersion struct {
	uid             string
	resourceVersion string
}

type sourceVersions struct {
	pods     map[string]objectVersion
	adapters map[string]objectVersion
}

// ModelListHealth tracks events after their cache handler completes and checks
// them against a current Kubernetes snapshot when the last proof expires.
// Its lock also makes the health decision and model-list read one snapshot.
type ModelListHealth struct {
	mu           sync.RWMutex
	verifyMu     sync.Mutex
	applied      sourceVersions
	lastVerified time.Time
	// lastMatched is when the latest successful verification started. Unlike
	// lastVerified, a failure or a watch error does not clear it, so it can
	// report how long the cache has gone without matching a LIST.
	lastMatched time.Time
	retryAfter  time.Time
	errorEpoch  uint64
	maxAge      time.Duration
	// verifyTimeout bounds one verification, independently of the caller's
	// context; tests shorten it.
	verifyTimeout time.Duration
	snapshot      func(context.Context) (sourceVersions, error)
}

func newModelListHealth(maxAge time.Duration) *ModelListHealth {
	return &ModelListHealth{
		maxAge:        maxAge,
		verifyTimeout: modelListVerificationTimeout,
		applied: sourceVersions{
			pods:     make(map[string]objectVersion),
			adapters: make(map[string]objectVersion),
		},
	}
}

func objectIdentity(obj metav1.Object) (string, objectVersion) {
	return obj.GetNamespace() + "/" + obj.GetName(), objectVersion{
		uid:             string(obj.GetUID()),
		resourceVersion: obj.GetResourceVersion(),
	}
}

// Apply holds the health read barrier while a required-source event changes
// the derived cache. A failed handler cannot advance the applied record.
func (h *ModelListHealth) Apply(ev WatchEvent, handler func(WatchEvent) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var versions map[string]objectVersion
	var obj metav1.Object
	switch typed := ev.Object.(type) {
	case *v1.Pod:
		if typed == nil {
			h.invalidateLocked()
			return
		}
		versions, obj = h.applied.pods, typed
	case *modelv1alpha1.ModelAdapter:
		if typed == nil {
			h.invalidateLocked()
			return
		}
		versions, obj = h.applied.adapters, typed
	default:
		h.invalidateLocked()
		return
	}
	// The cache handler may dereference the object, so validate it first.
	if !handler(ev) {
		h.invalidateLocked()
		return
	}
	key, version := objectIdentity(obj)
	if ev.Type == EventDelete {
		delete(versions, key)
	} else {
		versions[key] = version
	}
}

// Invalidate causes the next request to verify immediately after a reported
// watch error. A verification already in flight cannot clear this error.
func (h *ModelListHealth) Invalidate() {
	h.mu.Lock()
	h.invalidateLocked()
	h.mu.Unlock()
}

func (h *ModelListHealth) invalidateLocked() {
	h.lastVerified = time.Time{}
	h.retryAfter = time.Time{}
	h.errorEpoch++
}

func equalVersions(a, b map[string]objectVersion) bool {
	if len(a) != len(b) {
		return false
	}
	for key, version := range a {
		other, ok := b[key]
		if !ok || other != version {
			return false
		}
	}
	return true
}

func (h *ModelListHealth) verificationState(now time.Time) (fresh, retryLater bool, epoch uint64) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return !h.lastVerified.IsZero() && now.Sub(h.lastVerified) < h.maxAge,
		now.Before(h.retryAfter), h.errorEpoch
}

// EnsureVerified checks a complete current snapshot at most once per maxAge.
// A failed check returns an error and is retried only after a short delay.
// No Kubernetes request is made while the previous proof is still fresh.
func (h *ModelListHealth) EnsureVerified(ctx context.Context) error {
	if fresh, retryLater, _ := h.verificationState(time.Now()); fresh {
		return nil
	} else if retryLater {
		return ErrModelListVerificationDeferred
	}

	h.verifyMu.Lock()
	defer h.verifyMu.Unlock()
	if fresh, retryLater, _ := h.verificationState(time.Now()); fresh {
		return nil
	} else if retryLater {
		return ErrModelListVerificationDeferred
	}
	_, _, epoch := h.verificationState(time.Now())
	if h.snapshot == nil {
		return ErrModelListDiscoveryUnavailable
	}
	checkCtx, cancel := context.WithTimeout(ctx, h.verifyTimeout)
	defer cancel()
	started := time.Now()
	current, err := h.snapshot(checkCtx)

	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil && epoch == h.errorEpoch &&
		equalVersions(current.pods, h.applied.pods) &&
		equalVersions(current.adapters, h.applied.adapters) {
		// The LIST snapshot may be as old as the request start. Count the
		// freshness bound from that point, not from response completion.
		h.lastVerified = started
		h.lastMatched = started
		h.retryAfter = time.Time{}
		return nil
	}
	h.lastVerified = time.Time{}
	if ctx.Err() != nil {
		// A client cancel must not delay verification for other clients.
		h.retryAfter = time.Time{}
	} else {
		h.retryAfter = time.Now().Add(modelListVerificationRetry)
	}
	if err != nil {
		return fmt.Errorf("verify model discovery: %w", err)
	}
	return ErrModelListDiscoveryUnavailable
}

// LastMatched returns when the latest successful verification started, or the
// zero time if none has succeeded. It counts verifications from every caller,
// including /v1/models.
func (h *ModelListHealth) LastMatched() time.Time {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastMatched
}

// ModelsIfHealthy runs list while required-source event handlers are blocked.
// This keeps the health decision and model-list read in the same snapshot.
func (h *ModelListHealth) ModelsIfHealthy(list func() []string) ([]string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.lastVerified.IsZero() || time.Since(h.lastVerified) >= h.maxAge {
		return nil, false
	}
	return list(), true
}

// The consistency check compares only namespace, name, UID and resourceVersion,
// so it lists object metadata: the API server serializes, and the gateway
// decodes, no spec or status.
var (
	podsResource          = v1.SchemeGroupVersion.WithResource("pods")
	modelAdaptersResource = modelv1alpha1.GroupVersion.WithResource("modeladapters")
)

func listPodVersions(ctx context.Context, client metadata.Interface) (map[string]objectVersion, error) {
	return listObjectVersions(ctx, client, podsResource, "Pod")
}

func listAdapterVersions(ctx context.Context, client metadata.Interface) (map[string]objectVersion, error) {
	return listObjectVersions(ctx, client, modelAdaptersResource, "ModelAdapter")
}

// listObjectVersions pages through every object of resource, in all namespaces,
// with a metadata-only LIST.
func listObjectVersions(ctx context.Context, client metadata.Interface, resource schema.GroupVersionResource,
	kind string) (map[string]objectVersion, error) {
	versions := make(map[string]objectVersion)
	opts := metav1.ListOptions{Limit: modelListPageSize}
	for {
		page, err := client.Resource(resource).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			key, version := objectIdentity(&page.Items[i])
			versions[key] = version
		}
		if page.Continue == "" {
			return versions, nil
		}
		if page.Continue == opts.Continue {
			return nil, fmt.Errorf("repeated %s list continuation token", kind)
		}
		opts.Continue = page.Continue
	}
}
