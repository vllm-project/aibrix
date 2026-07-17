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

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	defaultModelClaimRuntimePort = 8080
	modelClaimWakePath           = "/v1/runtime/models/wake"
	modelClaimWakeTimeout        = 2 * time.Minute
	modelClaimRetryAfterSeconds  = 10
)

type modelWakeRequester interface {
	// RequestWake queues a wake without holding the current request. The bool is
	// false when the request is invalid or an identical wake is already running.
	RequestWake(pod *v1.Pod, model string) bool
}

type runtimeModelWakeRequester struct {
	client   *http.Client
	port     int
	inFlight sync.Map
}

func newRuntimeModelWakeRequester(client *http.Client, port int) *runtimeModelWakeRequester {
	if client == nil {
		client = &http.Client{Timeout: modelClaimWakeTimeout}
	}
	if port <= 0 {
		port = defaultModelClaimRuntimePort
	}
	return &runtimeModelWakeRequester{client: client, port: port}
}

func (r *runtimeModelWakeRequester) RequestWake(pod *v1.Pod, model string) bool {
	if r == nil || pod == nil || pod.Status.PodIP == "" || model == "" {
		return false
	}
	operationID := modelClaimWakeOperationID(pod, model)
	if _, alreadyRunning := r.inFlight.LoadOrStore(operationID, struct{}{}); alreadyRunning {
		return false
	}
	go func() {
		defer r.inFlight.Delete(operationID)
		if err := r.wake(pod.Status.PodIP, model, operationID); err != nil {
			klog.ErrorS(err, "ModelClaim request-triggered wake failed", "pod", klog.KObj(pod), "model", model)
			return
		}
		klog.InfoS("ModelClaim request-triggered wake completed", "pod", klog.KObj(pod), "model", model)
	}()
	return true
}

func modelClaimWakeOperationID(pod *v1.Pod, model string) string {
	podID := string(pod.UID)
	if podID == "" {
		podID = pod.Namespace + "/" + pod.Name
	}
	revision := pod.ResourceVersion
	if revision == "" {
		revision = "unknown"
	}
	return fmt.Sprintf("gateway-wake/%s/%s/%s", podID, model, revision)
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
