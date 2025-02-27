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
	"bytes"
	"encoding/binary"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	defaultPrefixCacheBlockSize              = 16
	defaultPrefixCacheEvictionInternalInMS   = 50
	defaultPrefixCacheEvictionDurationInMins = 60
)

var (
	// TODO: add a helper function for get methods.
	prefixCacheBlockSize        = getPrefixCacheBlockSize()
	prefixCacheEvictionInterval = getPrefixCacheEvictionInterval()
	prefixCacheEvictionDuration = getPrefixCacheEvictionDuration()
)

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
	value := utils.LoadEnv("AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_MS", "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_MS: %s, falling back to default", value)
		} else {
			klog.Infof("using AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_MS env value for prefix cache eviction interval: %d ms", intValue)
			return time.Duration(intValue) * time.Millisecond
		}
	}
	klog.Infof("using default prefix cache eviction interval: %d ms", defaultPrefixCacheEvictionInternalInMS)
	return defaultPrefixCacheEvictionInternalInMS * time.Millisecond
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
	mu     sync.RWMutex
	blocks map[uint64]Block
	hash   *xxhash.Digest
	seed   uint64
}

type Block struct {
	modelToPods    map[string]map[string]time.Time // model_name: map[pod_name]pod_last_access_time
	lastAccessTime time.Time                       //block_last_access_time
}

func NewPrefixHashTable() PrefixCacheIndexer {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	seed := r.Uint64()
	instance := &PrefixHashTable{
		blocks: map[uint64]Block{},
		hash:   xxhash.NewWithSeed(seed),
		seed:   seed,
	}

	ticker := time.NewTicker(prefixCacheEvictionInterval)
	go func() {
		for range ticker.C {
			instance.Evict(time.Now())
		}
	}()

	return instance
}

// returns matchedTokens, unMatchedTokens, matchedPods
// TODO: add an interface with multiple implementations such as hash or radix tree
func (c *PrefixHashTable) MatchPrefix(tokens []int, model string, pods []*v1.Pod) ([]int, []int, []*v1.Pod) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var block, lastMatchedBlock Block
	var ok bool
	var lastTokenMatchIndex int

	for i := 0; i < len(tokens); i += prefixCacheBlockSize {
		end := i + prefixCacheBlockSize
		if end > len(tokens) {
			end = len(tokens)
		}

		chunk := tokens[i:end]
		_, _ = c.hash.Write(IntArrayToByteArray(chunk))
		prefixHash := c.hash.Sum64()
		c.hash.ResetWithSeed(c.seed)
		block, ok = c.blocks[prefixHash]
		if !ok || len(block.modelToPods[model]) == 0 {
			lastTokenMatchIndex = i
			break
		}

		lastTokenMatchIndex = end
		lastMatchedBlock = block
		block.lastAccessTime = time.Now()
		c.blocks[prefixHash] = block
	}

	matchedTokens := tokens[0:lastTokenMatchIndex]
	unMatchedTokens := tokens[lastTokenMatchIndex:]

	var matchedPods []*v1.Pod
	blockPods := lastMatchedBlock.modelToPods[model]
	for _, pod := range pods {
		if _, ok := blockPods[pod.Name]; ok {
			matchedPods = append(matchedPods, pod)
		}
	}

	return matchedTokens, unMatchedTokens, matchedPods
}

func (c *PrefixHashTable) AddPrefix(unMatchedTokens []int, model, pod string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := 0; i < len(unMatchedTokens); i += prefixCacheBlockSize {
		end := i + prefixCacheBlockSize
		if end > len(unMatchedTokens) {
			end = len(unMatchedTokens)
		}

		chunk := unMatchedTokens[i:end]
		_, _ = c.hash.Write(IntArrayToByteArray(chunk))
		prefixHash := c.hash.Sum64()
		c.hash.ResetWithSeed(c.seed)
		block, ok := c.blocks[prefixHash]
		if !ok {
			block = Block{
				modelToPods:    map[string]map[string]time.Time{},
				lastAccessTime: time.Now(),
			}
			c.blocks[prefixHash] = block
		}

		blockPods, ok := block.modelToPods[model]
		if !ok {
			blockPods = map[string]time.Time{}
			block.modelToPods[model] = blockPods
		}

		blockPods[pod] = time.Now()
	}
}

func (c *PrefixHashTable) Evict(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for hash, block := range c.blocks {
		if now.Sub(block.lastAccessTime) > prefixCacheEvictionDuration {
			delete(c.blocks, hash)
			klog.InfoS("prefix cache block evicted", "hash", hash)
		}
	}
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
