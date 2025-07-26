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

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	RouterPD                    types.RoutingAlgorithm = "pd"
	LLMEngineLabel              string                 = "model.aibrix.ai/engine"
	PDRoleIdentifier            string                 = "role-name"
	RoleReplicaIndex            string                 = "stormservice.orchestration.aibrix.ai/role-replica-index"
	PodGroupIndex               string                 = "stormservice.orchestration.aibrix.ai/pod-group-index"
	defaultDecodeOnlyTokenLimit int                    = 256
)

func init() {
	Register(RouterPD, NewPDRouter)
}

type pdRouter struct {
	cache              cache.Cache
	tokenizer          tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable
}

func NewPDRouter() (types.Router, error) {
	var tokenizerObj tokenizer.Tokenizer
	if tokenizerType == "tiktoken" {
		tokenizerObj = tokenizer.NewTiktokenTokenizer()
	} else {
		tokenizerObj = tokenizer.NewCharacterTokenizer()
	}

	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in prefix cache router")
		return nil, err
	}

	return pdRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: prefixcacheindexer.NewPrefixHashTable(),
	}, nil
}

func (r pdRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	prefillPods, decodePods, err := r.filterPrefillDecodePods(readyPodList.All())
	if err != nil {
		return "", err
	}

	if err = r.doPrefillRequest(ctx, prefillPods); err != nil {
		return "", fmt.Errorf("prefill request failed: %w", err)
	}

	decodePod := r.selectDecodePod(decodePods)

	ctx.SetTargetPod(decodePod)
	return ctx.TargetAddress(), nil
}

func (r *pdRouter) filterPrefillDecodePods(readyPods []*v1.Pod) ([]*v1.Pod, []*v1.Pod, error) {
	prefillPods, decodePods := []*v1.Pod{}, []*v1.Pod{}
	var rolename string
	var ok bool
	for _, pod := range readyPods {
		if rolename, ok = pod.Labels[PDRoleIdentifier]; !ok {
			continue
		}

		if rolename == "prefill" {
			prefillPods = append(prefillPods, pod)
		} else if rolename == "decode" {
			decodePods = append(decodePods, pod)
		}
	}

	if len(prefillPods) == 0 || len(decodePods) == 0 {
		return nil, nil, fmt.Errorf("prefill or decodes pods are not ready")
	}

	return prefillPods, decodePods, nil
}

func (r *pdRouter) evaluatePrefixCache(ctx *types.RoutingContext, decodePods []*v1.Pod) (map[string]int, []uint64, error) {
	tokens, err := r.tokenizer.TokenizeInputText(ctx.Message)
	if err != nil {
		return nil, nil, err
	}

	readyPodsMap := map[string]struct{}{}
	for _, pod := range decodePods {
		readyPodsMap[pod.Name] = struct{}{}
	}
	matchedPods, prefixHashes := r.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, readyPodsMap)

	return matchedPods, prefixHashes, nil
}

func (r *pdRouter) selectDecodePod(decodePods []*v1.Pod) *v1.Pod {
	decodePod, _ := utils.SelectRandomPod(decodePods, rand.Intn)
	return decodePod
}

func (r *pdRouter) doPrefillRequest(routingCtx *types.RoutingContext, prefillPods []*v1.Pod) error {
	// TODO: refactor constants post PR: 1293 merge
	if getLLMEngine(prefillPods[0], LLMEngineLabel, "vllm") == "xllm" {
		return nil
	}

	var prefillPod *v1.Pod
	prefixMatchPrefillPods, prefixHashes, err := r.evaluatePrefixCache(routingCtx, prefillPods)
	if err != nil {
		return err
	}

	if len(prefixMatchPrefillPods) > 0 {
		prefillPod = getTargetPodFromMatchedPods(r.cache, prefillPods, prefixMatchPrefillPods)
	}
	if prefillPod == nil {
		prefillPod = selectTargetPodWithLeastRequestCount(r.cache, prefillPods)
	}

	defer func() {
		if len(prefixHashes) > 0 {
			r.prefixCacheIndexer.AddPrefix(prefixHashes, routingCtx.Model, prefillPod.Name)
		}
	}()

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
	defer func() {
		_ = resp.Body.Close()
	}()

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

func getLLMEngine(pod *v1.Pod, labelName string, defaultValue string) string {
	labelTarget, ok := pod.Labels[labelName]
	if !ok {
		return defaultValue
	}
	return labelTarget
}
