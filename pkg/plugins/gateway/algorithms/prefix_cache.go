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
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"

	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/aibrix/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

const (
	AIBRIX_PREFIX_CACHE_BLOCK_SIZE               = 16 // loadEnv("AIBRIX_PREFIX_CACHE_BLOCK_SIZE", 16)
	AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_MS     = 50
	AIBRIX_PROMPT_PREFIX_MATCH_THRESHOLD_PERCENT = 50
)

type prefixCacheRouter struct {
}

func PrefixCacheRouter() Router {
	return prefixCacheRouter{}
}

// WIP
func (r prefixCacheRouter) Route(ctx context.Context, pods map[string]*v1.Pod, model string) (string, error) {
	cache, _ := cache.GetCache()

	readyPods := utils.FilterReadyPods(pods)
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}
	if len(readyPods) == 1 {
		for _, pod := range pods {
			return getPodAddress(pod.Status.PodIP)
		}
	}

	inputReqText := "hello world, how are you, I am fine"
	tokens, err := utils.TokenizeInputText(inputReqText)
	if err != nil {
		return "", err
	}

	fmt.Println(cache.Pods)
	fmt.Println(tokens)

	// matchedTokens, unMatchedTokens, matchedPods :=  MatchPrefix(tokens, model, readyPods)

	// more than threshold -> get target pod
	// less than threshold -> get target pod

	// add unmatched tokens to prefix block with that target pod

	targetPodIP, err := selectRandomPod(pods, rand.Intn)
	if err != nil {
		return "", err
	}

	return getPodAddress(targetPodIP)
}

func IntArrayToByteArray(intArray []int) []byte {
	buf := new(bytes.Buffer)
	for _, val := range intArray {
		err := binary.Write(buf, binary.LittleEndian, int32(val))
		if err != nil {
			panic(err)
		}
	}
	return buf.Bytes()
}
