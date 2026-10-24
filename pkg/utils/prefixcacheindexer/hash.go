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
	"encoding/binary"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/vllm-project/aibrix/pkg/utils"
	lrustore "github.com/vllm-project/aibrix/pkg/utils/lrustore"
	"k8s.io/klog/v2"
)

const (
	defaultPrefixCacheBlockNumber            = 200000
	defaultPrefixCacheBlockSize              = 4
	defaultPrefixCacheEvictionInternalInSec  = 1  // 1 second
	defaultPrefixCacheEvictionDurationInMins = 20 // 20 minutes
	defaultPrefixCacheHashSeed               = uint64(0)
	envPrefixCacheHashSeed                   = "AIBRIX_PREFIX_CACHE_HASH_SEED"
	envStateSyncEnabled                      = "AIBRIX_STATESYNC_ENABLED"
)

var (
	prefixCacheBlockNumber      = utils.LoadEnvInt("AIBRIX_PREFIX_CACHE_BLOCK_NUMBER", defaultPrefixCacheBlockNumber)
	prefixCacheBlockSize        = utils.LoadEnvInt("AIBRIX_PREFIX_CACHE_BLOCK_SIZE", defaultPrefixCacheBlockSize)
	prefixCacheEvictionInterval = time.Duration(utils.LoadEnvInt("AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_SECONDS", defaultPrefixCacheEvictionInternalInSec)) * time.Second
	prefixCacheEvictionDuration = time.Duration(utils.LoadEnvInt("AIBRIX_PREFIX_CACHE_EVICTION_DURATION_MINS", defaultPrefixCacheEvictionDurationInMins)) * time.Minute
)

var (
	sharedOnce     sync.Once
	sharedInstance *PrefixHashTable
)

func GetSharedPrefixHashTable() *PrefixHashTable {
	sharedOnce.Do(func() {
		sharedInstance = NewPrefixHashTable()
	})
	return sharedInstance
}

type PrefixHashTable struct {
	mu       sync.RWMutex
	seed     uint64
	store    lrustore.Store[uint64, Block]
	dirtyIds map[string]uint64 // block hash as string -> dirtySeq of its last change, for delta sync
	dirtySeq uint64
	// deltaSeq is dirtySeq as of the last GetDeltaForSync. It is written under
	// the read lock, so it must be atomic.
	deltaSeq atomic.Uint64
	// now is the clock for pod access times; tests replace it.
	now func() time.Time
}

type Block struct {
	modelToPods map[string]map[string]time.Time // model_name: map[pod_key]pod_last_access_time, pod_key is namespace/name
}

func randomPrefixCacheHashSeed() uint64 {
	return rand.New(rand.NewSource(time.Now().UnixNano())).Uint64()
}

func prefixCacheHashSeed() uint64 {
	stateSyncEnabled := utils.LoadEnvBool(envStateSyncEnabled, false)

	seedStr, ok := utils.LookupEnv(envPrefixCacheHashSeed)
	if ok && seedStr != "" {
		seed, err := strconv.ParseUint(seedStr, 10, 64)
		if err != nil {
			if stateSyncEnabled {
				klog.Warningf("invalid %s=%q with %s=true, using default %d: %v",
					envPrefixCacheHashSeed, seedStr, envStateSyncEnabled, defaultPrefixCacheHashSeed, err)
				return defaultPrefixCacheHashSeed
			}
			klog.Warningf("invalid %s=%q, using random seed: %v", envPrefixCacheHashSeed, seedStr, err)
			return randomPrefixCacheHashSeed()
		}
		return seed
	}

	if stateSyncEnabled {
		return defaultPrefixCacheHashSeed
	}
	return randomPrefixCacheHashSeed()
}

func NewPrefixHashTable() *PrefixHashTable {
	seed := prefixCacheHashSeed()
	klog.InfoS("prefix_cache_hash_table_configurations",
		"prefix_cache_hash_seed", seed,
		"statesync_enabled", utils.LoadEnvBool(envStateSyncEnabled, false),
		"prefix_cache_block_number", prefixCacheBlockNumber,
		"prefix_cache_block_size", prefixCacheBlockSize,
		"prefix_cache_block_eviction_interval_seconds", prefixCacheEvictionInterval,
		"prefix_cache_block_eviction_duration_minutes", prefixCacheEvictionDuration)
	instance := &PrefixHashTable{
		seed:  seed,
		now:   time.Now,
		store: lrustore.NewLRUStore[uint64, Block](prefixCacheBlockNumber, prefixCacheEvictionDuration, prefixCacheEvictionInterval, func() time.Time { return time.Now() }),
	}
	return instance
}

// EnableDeltaSync initializes dirty-set tracking so GetDeltaForSync/ClearDirtyForSync
// can be used. Call this once before registering with statesync.Manager. It is safe
// to call multiple times; subsequent calls are no-ops.
func (c *PrefixHashTable) EnableDeltaSync() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirtyIds == nil {
		c.dirtyIds = make(map[string]uint64)
	}
}

// MatchPrefix matches the input token prefix's if already cached
// returns map[podkey]%prefixmatch along with all prefix hashes. readyPods and the
// returned map use the same pod keys that AddPrefix stored, namespace/name
// (utils.GeneratePodKey), so same-named pods in different namespaces stay apart.
func (c *PrefixHashTable) MatchPrefix(tokens []byte, model string, readyPods map[string]struct{}) (map[string]int, []uint64) {
	prefixHashes := getPrefixHashes(c.seed, tokens)
	return c.seqSearchPrefix(prefixHashes, model, readyPods)
}

func (c *PrefixHashTable) seqSearchPrefix(prefixHashes []uint64, model string, readyPods map[string]struct{}) (map[string]int, []uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// A pod that has not been routed this prefix for the eviction duration has
	// most likely evicted it, even if the block stays cached for other pods.
	staleBefore := c.now().Add(-prefixCacheEvictionDuration)
	// podkey -> %prefixmatch
	prefixMatchPods := make(map[string]int)
	for i := 0; i < len(prefixHashes); i++ {
		prefixHash := prefixHashes[i]
		block, ok := c.store.Get(prefixHash)
		if !ok {
			break
		}

		blockPods := block.modelToPods[model]
		if len(blockPods) == 0 {
			break
		}

		prefixMatchPercent := (i + 1) * 100 / len(prefixHashes)
		// Integer division drops a real hit on a long prompt to 0, which then
		// ties with pods that matched nothing. Keep the hit visible.
		if prefixMatchPercent == 0 {
			prefixMatchPercent = 1
		}
		if readyPods = matchPods(blockPods, readyPods, prefixMatchPods, prefixMatchPercent, staleBefore); len(readyPods) == 0 {
			break
		}
	}
	return prefixMatchPods, prefixHashes
}

// AddPrefix add prefix hashes for input tokens. pod is the pod key, namespace/name.
func (c *PrefixHashTable) AddPrefix(prefixHashes []uint64, model, pod string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	staleBefore := now.Add(-prefixCacheEvictionDuration)
	for i := 0; i < len(prefixHashes); i++ {
		prefixHash := prefixHashes[i]

		block, ok := c.store.Get(prefixHash)
		if !ok {
			block = Block{
				modelToPods: map[string]map[string]time.Time{
					model: {pod: now},
				},
			}
		} else {
			blockPods, ok := block.modelToPods[model]
			if !ok {
				blockPods = map[string]time.Time{}
			}
			// Each Put refreshes the whole block, so a block that stays hot would
			// otherwise keep every pod it was ever routed to.
			for p, accessed := range blockPods {
				if accessed.Before(staleBefore) {
					delete(blockPods, p)
				}
			}
			blockPods[pod] = now
			block.modelToPods[model] = blockPods
		}

		c.store.Put(prefixHash, block)
		if c.dirtyIds != nil {
			c.dirtySeq++
			c.dirtyIds[strconv.FormatUint(prefixHash, 10)] = c.dirtySeq
		}
	}
}

// matchPods records prefixMatchPercent for the pods in alive that also hold this block, and
// returns them: only they can match the next block. A pod last routed the block before
// staleBefore does not count. When the block is held by fewer pods than are still alive, as
// on the first block of a large fleet, it walks the block's pods into a new set; otherwise it
// narrows alive in place, as before. Either way a block costs O(min(holders, alive)), not
// O(ready pods).
func matchPods(blockPods map[string]time.Time, alive map[string]struct{}, prefixMatchPods map[string]int, prefixMatchPercent int, staleBefore time.Time) map[string]struct{} {
	if len(blockPods) < len(alive) {
		next := make(map[string]struct{}, len(blockPods))
		for pod, accessed := range blockPods {
			if _, ok := alive[pod]; ok && !accessed.Before(staleBefore) {
				prefixMatchPods[pod] = prefixMatchPercent
				next[pod] = struct{}{}
			}
		}
		return next
	}
	for pod := range alive {
		if accessed, ok := blockPods[pod]; ok && !accessed.Before(staleBefore) {
			prefixMatchPods[pod] = prefixMatchPercent
		} else {
			delete(alive, pod)
		}
	}
	return alive
}

func getPrefixHashes(seed uint64, tokens []byte) []uint64 {
	numBlocks := len(tokens) / prefixCacheBlockSize
	prefixHashes := make([]uint64, 0, numBlocks)
	digest := xxhash.NewWithSeed(seed)

	// Use seed as the initial parent hash (equivalent to NONE_HASH in Python)
	parentHash := seed

	var parentHashBytes [8]byte

	for i := 0; i < len(tokens); i += prefixCacheBlockSize {
		end := i + prefixCacheBlockSize
		if end > len(tokens) {
			// Don't hash incomplete blocks
			break
		}

		// Reset digest for new block
		digest.ResetWithSeed(seed)

		// Write parent hash first (as 8 bytes)
		binary.LittleEndian.PutUint64(parentHashBytes[:], parentHash)
		_, _ = digest.Write(parentHashBytes[:])

		// Write current block tokens
		_, _ = digest.Write(tokens[i:end])

		// Compute hash for this block
		currentHash := digest.Sum64()
		prefixHashes = append(prefixHashes, currentHash)

		// Update parent hash for next iteration
		parentHash = currentHash
	}
	return prefixHashes
}

func (c *PrefixHashTable) GetPrefixHashes(tokens []byte) []uint64 {
	return getPrefixHashes(c.seed, tokens)
}

// Len returns the number of cached prefix blocks.
func (c *PrefixHashTable) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.store.Len()
}
