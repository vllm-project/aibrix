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

package prefixcacheindexer

import (
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/cache"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	defaultPrefixCacheBlockNumber            = 200000
	defaultPrefixCacheBlockSize              = 16
	defaultPrefixCacheEvictionInternalInSec  = 1  // 1 second
	defaultPrefixCacheEvictionDurationInMins = 20 // 20 minutes
)

var (
	prefixCacheBlockNumber      = getEnvValueOrDefault("AIBRIX_PREFIX_CACHE_BLOCK_NUMBER", defaultPrefixCacheBlockNumber)
	prefixCacheBlockSize        = getEnvValueOrDefault("AIBRIX_PREFIX_CACHE_BLOCK_SIZE", defaultPrefixCacheBlockSize)
	prefixCacheEvictionInterval = getEnvDurationOrDefault("AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_SECONDS", defaultPrefixCacheEvictionInternalInSec, time.Second)
	prefixCacheEvictionDuration = getEnvDurationOrDefault("AIBRIX_PREFIX_CACHE_EVICTION_DURATION_MINS", defaultPrefixCacheEvictionDurationInMins, time.Minute)
)

func getEnvValueOrDefault(envKey string, defaultValue int) int {
	value := utils.LoadEnv(envKey, "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid %s: %s, falling back to default", envKey, value)
		} else {
			klog.Infof("using %s env value: %d", envKey, intValue)
			return intValue
		}
	}
	klog.Infof("using default %s: %d", envKey, defaultValue)
	return defaultValue
}

func getEnvDurationOrDefault(envKey string, defaultValue int, unit time.Duration) time.Duration {
	value := utils.LoadEnv(envKey, "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid %s: %s, falling back to default", envKey, value)
		} else {
			klog.Infof("using %s env value: %d", envKey, intValue)
			return time.Duration(intValue) * unit
		}
	}
	klog.Infof("using default %s: %d", envKey, defaultValue)
	return time.Duration(defaultValue) * unit
}

type PrefixHashTable struct {
	mu    sync.RWMutex
	hash  *xxhash.Digest
	seed  uint64
	store cache.Store[uint64, Block]
}

type Block struct {
	modelToPods map[string]map[string]time.Time // model_name: map[pod_name]pod_last_access_time
}

func NewPrefixHashTable() PrefixCacheIndexer {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	seed := r.Uint64()
	instance := &PrefixHashTable{
		hash: xxhash.NewWithSeed(seed),
		seed: seed,
		store: cache.NewLRUStore[uint64, Block](prefixCacheBlockNumber,
			prefixCacheEvictionDuration,
			prefixCacheEvictionInterval,
			func() time.Time { return time.Now() }),
	}

	return instance
}

// returns matchedTokens, unMatchedTokens, matchedPods
func (c *PrefixHashTable) MatchPrefix(tokens []byte, model string, pods []*v1.Pod) ([]byte, []byte, []*v1.Pod) {
	var block, lastMatchedBlock Block
	var ok bool
	var lastTokenMatchIndex int

	for i := 0; i < len(tokens); i += prefixCacheBlockSize {
		end := i + prefixCacheBlockSize
		if end > len(tokens) {
			end = len(tokens)
		}

		c.mu.Lock()
		_, _ = c.hash.Write(tokens[i:end])
		prefixHash := c.hash.Sum64()
		c.hash.ResetWithSeed(c.seed)
		c.mu.Unlock()
		block, ok = c.store.Get(prefixHash)
		if !ok || len(block.modelToPods[model]) == 0 || len(matchPods(block.modelToPods[model], pods)) == 0 {
			lastTokenMatchIndex = i
			break
		}

		lastTokenMatchIndex = end
		lastMatchedBlock = block
		c.store.Put(prefixHash, block)
	}

	matchedTokens := tokens[0:lastTokenMatchIndex]
	unMatchedTokens := tokens[lastTokenMatchIndex:]

	var matchedPods []*v1.Pod
	if len(matchedTokens) > 0 {
		matchedPods = matchPods(lastMatchedBlock.modelToPods[model], pods)
	}

	return matchedTokens, unMatchedTokens, matchedPods
}

func (c *PrefixHashTable) AddPrefix(unMatchedTokens []byte, model, pod string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := 0; i < len(unMatchedTokens); i += prefixCacheBlockSize {
		end := i + prefixCacheBlockSize
		if end > len(unMatchedTokens) {
			end = len(unMatchedTokens)
		}

		_, _ = c.hash.Write(unMatchedTokens[i:end])
		prefixHash := c.hash.Sum64()
		c.hash.ResetWithSeed(c.seed)
		block, ok := c.store.Get(prefixHash)
		if !ok {
			block = Block{
				modelToPods: map[string]map[string]time.Time{
					model: {
						pod: time.Now(),
					},
				},
			}
		} else {
			blockPods, ok := block.modelToPods[model]
			if !ok {
				blockPods = map[string]time.Time{}
			}
			blockPods[pod] = time.Now()
			block.modelToPods[model] = blockPods
		}

		c.store.Put(prefixHash, block)
	}
}

// matchPods returns ready pods that intersect with pods on which prefix tokens are catched.
func matchPods(blockPods map[string]time.Time, readyPods []*v1.Pod) []*v1.Pod {
	var matchedPods []*v1.Pod
	for _, pod := range readyPods {
		if _, ok := blockPods[pod.Name]; ok {
			matchedPods = append(matchedPods, pod)
		}
	}
	return matchedPods
}
