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

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	defaultModelClaimRuntimePort = 8080
	modelClaimWakePath           = "/v1/runtime/models/wake"
	modelClaimWakeTimeout        = 2 * time.Minute
	modelClaimRetryAfterSeconds  = 10
	// A wake request is a single patch of the pod's annotations.
	modelClaimWakeRequestTimeout = 10 * time.Second
	// A wake request written this recently is not written again while the
	// cache has not seen the pod since.
	modelClaimWakeRequestRecheck = 30 * time.Second
)

type modelWakeRequester interface {
	// RequestWake asks for a sleeping engine to be woken, without holding the
	// current request. The bool is false when the request is invalid, or when
	// the same wake has already been asked for.
	RequestWake(pod *v1.Pod, binding utils.ModelClaimBinding) bool
}

type runtimeModelWakeRequester struct {
	client   *http.Client
	port     int
	inFlight sync.Map
	// pods writes wake requests on pods. It is nil when the gateway runs
	// without Kubernetes, and every wake then goes to the runtime.
	pods kubernetes.Interface
	// asked holds each wake request written, as a wakeRequestWritten, for as
	// long as that keeps the request from being written again.
	asked sync.Map
	now   func() time.Time
}

func newRuntimeModelWakeRequester(client *http.Client, port int, pods kubernetes.Interface) *runtimeModelWakeRequester {
	if client == nil {
		client = &http.Client{Timeout: modelClaimWakeTimeout}
	}
	if port <= 0 {
		port = defaultModelClaimRuntimePort
	}
	return &runtimeModelWakeRequester{client: client, port: port, pods: pods, now: time.Now}
}

// RequestWake wakes a sleeping engine the way its controller expects. A
// controller that wakes engines itself says so on the binding, and the wake is
// then asked of it, by a request written on the pod. It decides whether the
// card has room for the engine first. Otherwise the runtime is asked directly.
func (r *runtimeModelWakeRequester) RequestWake(pod *v1.Pod, binding utils.ModelClaimBinding) bool {
	if r == nil || pod == nil {
		return false
	}
	if binding.WakeByRequest && binding.Claim != "" && r.pods != nil {
		return r.requestWakeOnPod(pod, binding.Claim)
	}
	model := binding.Model
	if pod.Status.PodIP == "" || model == "" {
		return false
	}
	// Pod status churn must not bypass in-flight deduplication. The runtime
	// operation ID stays unique so a later sleep/wake cycle is still applied.
	wakeKey := modelClaimWakeKey(pod, model)
	if _, alreadyRunning := r.inFlight.LoadOrStore(wakeKey, struct{}{}); alreadyRunning {
		return false
	}
	operationID := wakeKey + "/" + uuid.NewString()
	go func() {
		defer r.inFlight.Delete(wakeKey)
		if err := r.wake(pod.Status.PodIP, model, operationID); err != nil {
			klog.ErrorS(err, "ModelClaim request-triggered wake failed", "pod", klog.KObj(pod), "model", model)
			return
		}
		klog.InfoS("ModelClaim request-triggered wake completed", "pod", klog.KObj(pod), "model", model)
	}()
	return true
}

// wakeRequestWritten is a wake request this gateway wrote on a pod.
type wakeRequestWritten struct {
	at time.Time
	// over is the version of the pod that the request was written over. The
	// cache shows the pod at another version once it has seen the request.
	over string
}

// requestWakeOnPod writes a wake request for a claim on its pod. The controller
// wakes the engine and removes the request. A request already on the pod is not
// written again, and neither is one written a moment ago that the cache has not
// seen yet. A pod the cache has seen since, without the request, had it taken
// back, and a client that asks again has a new one written at once. The request
// carries the time it was first asked.
func (r *runtimeModelWakeRequester) requestWakeOnPod(pod *v1.Pod, claim string) bool {
	key := constants.ModelClaimWakeAnnotationPrefix + claim
	askedKey := "wake-request/" + string(pod.UID) + "/" + pod.Namespace + "/" + pod.Name + "/" + claim
	if _, asked := pod.Annotations[key]; asked {
		// The cache has seen the request. From now on, the pod says whether it
		// is still there.
		r.asked.Delete(askedKey)
		return false
	}
	now := r.now()
	r.forgetOldWakeRequests(now)
	if written, recent := r.asked.Load(askedKey); recent && written.(wakeRequestWritten).over == pod.ResourceVersion {
		return false
	}
	over := pod.ResourceVersion
	if _, alreadyRunning := r.inFlight.LoadOrStore(askedKey, struct{}{}); alreadyRunning {
		return false
	}
	go func() {
		defer r.inFlight.Delete(askedKey)
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"annotations": map[string]string{key: now.UTC().Format(time.RFC3339)}},
		})
		if err != nil {
			klog.ErrorS(err, "could not encode a ModelClaim wake request", "pod", klog.KObj(pod), "claim", claim)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), modelClaimWakeRequestTimeout)
		defer cancel()
		if _, err := r.pods.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, k8stypes.MergePatchType, patch,
			metav1.PatchOptions{}); err != nil {
			klog.ErrorS(err, "could not ask the controller to wake a ModelClaim", "pod", klog.KObj(pod), "claim", claim)
			return
		}
		r.asked.Store(askedKey, wakeRequestWritten{at: now, over: over})
		klog.InfoS("asked the controller to wake a ModelClaim", "pod", klog.KObj(pod), "claim", claim)
	}()
	return true
}

// forgetOldWakeRequests forgets the wake requests written longer ago than
// modelClaimWakeRequestRecheck. Only a recent one keeps a request from being
// written again, and the pod of an old one may be gone.
func (r *runtimeModelWakeRequester) forgetOldWakeRequests(now time.Time) {
	r.asked.Range(func(key, written any) bool {
		if now.Sub(written.(wakeRequestWritten).at) >= modelClaimWakeRequestRecheck {
			// A request written again meanwhile is kept.
			r.asked.CompareAndDelete(key, written)
		}
		return true
	})
}

func modelClaimWakeKey(pod *v1.Pod, model string) string {
	podID := string(pod.UID)
	if podID == "" {
		podID = pod.Namespace + "/" + pod.Name
	}
	return fmt.Sprintf("gateway-wake/%s/%s", podID, model)
}

func (r *runtimeModelWakeRequester) wake(podIP, model, operationID string) error {
	payload, err := json.Marshal(struct {
		ModelName   string `json:"model_name"`
		OperationID string `json:"operation_id"`
	}{ModelName: model, OperationID: operationID})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelClaimWakeTimeout)
	defer cancel()
	url := "http://" + net.JoinHostPort(podIP, fmt.Sprintf("%d", r.port)) + modelClaimWakePath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("runtime wake returned %d: %s", response.StatusCode, body)
	}
	return nil
}

var _ modelWakeRequester = (*runtimeModelWakeRequester)(nil)
