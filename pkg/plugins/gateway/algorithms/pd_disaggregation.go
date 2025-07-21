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

package routingalgorithms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	RouterPD         types.RoutingAlgorithm = "pd"
	PDRoleIdentifier string                 = "model.aibrix.ai/role"
)

func init() {
	Register(RouterPD, NewPDRouter)
}

type pdRouter struct {
}

func NewPDRouter() (types.Router, error) {
	return pdRouter{}, nil
}

func (r pdRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	prefillPods, decodePods := []*v1.Pod{}, []*v1.Pod{}
	for _, pod := range readyPodList.All() {
		if value, ok := pod.Labels[PDRoleIdentifier]; ok && value == "prefill" {
			prefillPods = append(prefillPods, pod)
		} else if value, ok := pod.Labels[PDRoleIdentifier]; ok && value == "decode" {
			decodePods = append(decodePods, pod)
		}
	}

	if len(prefillPods) == 0 || len(decodePods) == 0 {
		return "", fmt.Errorf("prefill or decodes pods are not ready")
	}

	prefillPod, err := utils.SelectRandomPod(prefillPods, rand.Intn)
	if err != nil {
		return "", fmt.Errorf("random selection failed for prefill pod: %w", err)
	}
	if prefillPod == nil {
		return "", fmt.Errorf("no prefill pods to forward request")
	}

	// do prefill request
	if err := r.doPrefillRequest(ctx, prefillPod); err != nil {
		return "", fmt.Errorf("prefill request failed: %w", err)
	}

	decodePod, err := utils.SelectRandomPod(decodePods, rand.Intn)
	if err != nil {
		return "", fmt.Errorf("random selection failed for decode pod: %w", err)
	}
	if decodePod == nil {
		return "", fmt.Errorf("no decode pods to forward request")
	}

	ctx.SetTargetPod(decodePod)
	return ctx.TargetAddress(), nil
}

func (r *pdRouter) doPrefillRequest(routingCtx *types.RoutingContext, prefillPod *v1.Pod) error {
	klog.InfoS("doPrefillRequest", "prefill_pod_ip", prefillPod.Status.PodIP)

	// Prepare request payload
	var completionRequest map[string]any
	if err := json.Unmarshal(routingCtx.ReqBody, &completionRequest); err != nil {
		return fmt.Errorf("failed to unmarshal request body for prefill: %w", err)
	}

	// Set prefill-specific parameters
	completionRequest["max_tokens"] = 1
	completionRequest["max_completion_tokens"] = 1
	completionRequest["stream"] = false

	payloadBytes, err := json.Marshal(completionRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal for prefill request: %w", err)
	}

	// Create HTTP request
	apiURL := fmt.Sprintf("http://%s:%d%s", prefillPod.Status.PodIP, utils.GetModelPortForPod(routingCtx.RequestID, prefillPod), routingCtx.ReqPath)
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create prefill request: %w", err)
	}

	// Set headers
	for key, value := range routingCtx.ReqHeaders {
		req.Header.Set(key, value)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("content-length", strconv.Itoa(len(payloadBytes)))

	// Execute request with timeout
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("prefill request failed: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("prefill request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (r *pdRouter) SubscribedMetrics() []string {
	return []string{}
}
