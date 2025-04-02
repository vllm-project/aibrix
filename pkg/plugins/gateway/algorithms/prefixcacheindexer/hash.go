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
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcacheindexer/cache"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	defaultPrefixCacheBlockNumber            = 200000
	defaultPrefixCacheBlockSize              = 4
	defaultPrefixCacheEvictionInternalInSec  = 1  // 1 second
	defaultPrefixCacheEvictionDurationInMins = 20 // 20 minutes
)

var (
	// TODO: add a helper function for get methods.
	prefixCacheBlockNumber      = getPrefixCacheBlockNumber()
	prefixCacheBlockSize        = getPrefixCacheBlockSize()
	prefixCacheEvictionInterval = getPrefixCacheEvictionInterval()
	prefixCacheEvictionDuration = getPrefixCacheEvictionDuration()
)

func getPrefixCacheBlockNumber() int {
	value := utils.LoadEnv("AIBRIX_PREFIX_CACHE_BLOCK_NUMBER", "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid AIBRIX_PREFIX_CACHE_BLOCK_NUMBER: %s, falling back to default", value)
		} else {
			klog.Infof("using AIBRIX_PREFIX_CACHE_BLOCK_NUMBER env value for prefix cache block number: %d", intValue)
			return intValue
		}
	}
	klog.Infof("using default prefix cache block number: %d", defaultPrefixCacheBlockNumber)
	return defaultPrefixCacheBlockNumber
}

func getPrefixCacheBlockSize() int {
	value := utils.LoadEnv("AIBRIX_PREFIX_CACHE_BLOCK_SIZE", "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid AIBRIX_PREFIX_CACHE_BLOCK_SIZE: %s, falling back to default", value)
		} else {
			klog.Infof("using AIBRIX_PREFIX_CACHE_BLOCK_SIZE env value for prefix cache block size: %d", intValue)
			return intValue
		}
	}
	klog.Infof("using default prefix cache block size: %d", defaultPrefixCacheBlockSize)
	return defaultPrefixCacheBlockSize
}

func getPrefixCacheEvictionInterval() time.Duration {
	value := utils.LoadEnv("AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_SECONDS", "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_SECONDS: %s, falling back to default", value)
		} else {
			klog.Infof("using AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_SECONDS env value for prefix cache eviction interval: %d ms", intValue)
			return time.Duration(intValue) * time.Second
		}
	}
	klog.Infof("using default prefix cache eviction interval: %d ms", defaultPrefixCacheEvictionInternalInSec)
	return defaultPrefixCacheEvictionInternalInSec * time.Second
}

func getPrefixCacheEvictionDuration() time.Duration {
	value := utils.LoadEnv("AIBRIX_PREFIX_CACHE_EVICTION_DURATION_MINS", "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid AIBRIX_PREFIX_CACHE_EVICTION_DURATION_MINS: %s, falling back to default", value)
		} else {
			klog.Infof("using AIBRIX_PREFIX_CACHE_EVICTION_DURATION_MINS env value for prefix cache eviction duration: %d ms", intValue)
			return time.Duration(intValue) * time.Minute
		}
	}
	klog.Infof("using default prefix cache eviction duration: %d mins", defaultPrefixCacheEvictionDurationInMins)
	return defaultPrefixCacheEvictionDurationInMins * time.Minute
}

type PrefixHashTable struct {
	mu    sync.RWMutex
	seed  uint64
	store cache.Store[uint64, Block]
}

type Block struct {
	modelToPods map[string]map[string]time.Time // model_name: map[pod_name]pod_last_access_time
}

func NewPrefixHashTable() *PrefixHashTable {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	seed := r.Uint64()
	instance := &PrefixHashTable{
		seed: seed,
		store: cache.NewLRUStore[uint64, Block](prefixCacheBlockNumber,
			prefixCacheEvictionDuration,
			prefixCacheEvictionInterval,
			func() time.Time { return time.Now() }),
	}
	return instance
}

// MatchPrefix matches the input token prefix's if already cached
// returns map[podname]%prefixmatch along with all prefix hashes
func (c *PrefixHashTable) MatchPrefix(tokens []byte, model string, readyPods map[string]struct{}) (map[string]int, []uint64) {
	prefixHashes := getPrefixHashes(c.seed, tokens)
	return c.seqSearchPrefix(prefixHashes, model, readyPods)
}

func (c *PrefixHashTable) seqSearchPrefix(prefixHashes []uint64, model string, readyPods map[string]struct{}) (map[string]int, []uint64) {
	// podname -> %prefixmatch
	prefixMatchPods := map[string]int{}
	for i := 0; i < len(prefixHashes); i++ {
		prefixHash := prefixHashes[i]
		prefixMatchPercent := (i + 1) * 100 / len(prefixHashes)

		block, ok := c.store.Get(prefixHash)
		if !ok || len(block.modelToPods[model]) == 0 ||
			!matchPods(block.modelToPods[model], readyPods, prefixMatchPods, prefixMatchPercent) {
			break
		}
	}
	return prefixMatchPods, prefixHashes
}

// AddPrefix add prefix hashes for input tokens
func (c *PrefixHashTable) AddPrefix(prefixHashes []uint64, model, pod string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := 0; i < len(prefixHashes); i++ {
		prefixHash := prefixHashes[i]

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
func matchPods(blockPods map[string]time.Time, readyPods map[string]struct{}, prefixMatchPods map[string]int, prefixMatchPercent int) bool {
	var isMatch bool
	for pod := range readyPods {
		if _, ok := blockPods[pod]; ok {
			prefixMatchPods[pod] = prefixMatchPercent
			isMatch = true
		} else {
			delete(readyPods, pod)
		}
	}
	return isMatch
}

func getPrefixHashes(seed uint64, tokens []byte) []uint64 {
	prefixHashes := []uint64{}
	digest := xxhash.NewWithSeed(seed)
	for i := 0; i < len(tokens); i += prefixCacheBlockSize {
		end := i + prefixCacheBlockSize
		if end > len(tokens) {
			break
		}
		_, _ = digest.Write(tokens[i:end])
		prefixHashes = append(prefixHashes, digest.Sum64())
		digest.ResetWithSeed(seed)
	}
	return prefixHashes
}

func (c *PrefixHashTable) GetPrefixHashes(tokens []byte) []uint64 {
	return getPrefixHashes(c.seed, tokens)
}
