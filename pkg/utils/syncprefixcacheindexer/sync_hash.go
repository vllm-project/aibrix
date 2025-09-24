/*
Copyright 2025 The Aibrix Team.

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

package syncprefixcacheindexer

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

const (
	defaultMaxContexts             = 1000
	defaultMaxPrefixesPerContext   = 10000
	defaultEvictionIntervalSeconds = 60
	defaultEvictionDurationMinutes = 20
	defaultPrefixCacheBlockSize    = 16
	evictionBatchSize              = 100 // Process contexts in batches
)

var (
	maxContexts           = utils.LoadEnvInt("AIBRIX_SYNC_MAX_CONTEXTS", defaultMaxContexts)
	maxPrefixesPerContext = utils.LoadEnvInt("AIBRIX_SYNC_MAX_PREFIXES_PER_CONTEXT", defaultMaxPrefixesPerContext)
	evictionInterval      = time.Duration(utils.LoadEnvInt("AIBRIX_SYNC_EVICTION_INTERVAL_SECONDS", defaultEvictionIntervalSeconds)) * time.Second
	evictionDuration      = time.Duration(utils.LoadEnvInt("AIBRIX_SYNC_EVICTION_DURATION_MINUTES", defaultEvictionDurationMinutes)) * time.Minute
	prefixCacheBlockSize  = utils.LoadEnvInt("AIBRIX_PREFIX_CACHE_BLOCK_SIZE", defaultPrefixCacheBlockSize)
)

// ModelContext represents the first-level hash key
type ModelContext struct {
	ModelName string
	LoraID    int64 // -1 represents no LoRA adapter
}

// PodInfo stores pod access information
type PodInfo struct {
	LastAccessTime atomic.Int64 // Unix timestamp (lock-free update)
	SourcePod      string
}

// PrefixStore manages prefix hashes for a specific (model, lora_id) context
type PrefixStore struct {
	// Second-level hash: prefixHash → pods
	prefixMap map[uint64]map[string]*PodInfo

	// Metadata
	createTime    time.Time
	lastAccess    atomic.Int64 // Unix timestamp (lock-free update)
	totalPrefixes int64
}

// EngineHashMapping maintains unidirectional mapping
type EngineHashMapping struct {
	engineToAibrix map[int64]uint64 // engine block hash → aibrix prefix hash
}

// ContextData holds all data for a specific context with separate locks
type ContextData struct {
	// Separate locks for different data structures
	prefixMu  sync.RWMutex // Protects prefixStore
	mappingMu sync.RWMutex // Protects hashMapping

	// Data structures
	prefixStore *PrefixStore
	hashMapping *EngineHashMapping

	// Eviction tracking (lock-free)
	markedForEviction atomic.Bool
}

// SyncPrefixHashTable is the main structure
type SyncPrefixHashTable struct {
	// Lock-free context map
	contextMap sync.Map // ModelContext → *ContextData

	// Global configuration (read-only after init)
	seed                  uint64
	maxContexts           int
	maxPrefixesPerContext int
	blockSize             int
	evictionInterval      time.Duration
	evictionDuration      time.Duration

	// Global state
	contextCount    atomic.Int32
	evictionRunning atomic.Bool
	evictionNeeded  atomic.Bool // Flag to trigger eviction
	stopCh          chan struct{}
	wg              sync.WaitGroup

	// Reverse index for efficient block removal
	blockIndexMu sync.RWMutex
	blockIndex   map[int64][]ModelContext // engine block hash → contexts using it
}

// NewSyncPrefixHashTable creates a new sync prefix hash table
func NewSyncPrefixHashTable() *SyncPrefixHashTable {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	seed := r.Uint64()

	klog.InfoS("sync_prefix_hash_table_configurations",
		"max_contexts", maxContexts,
		"max_prefixes_per_context", maxPrefixesPerContext,
		"eviction_interval_seconds", evictionInterval,
		"eviction_duration_minutes", evictionDuration,
		"prefix_cache_block_size", prefixCacheBlockSize,
		"seed", seed)

	s := &SyncPrefixHashTable{
		seed:                  seed,
		maxContexts:           maxContexts,
		maxPrefixesPerContext: maxPrefixesPerContext,
		blockSize:             prefixCacheBlockSize,
		evictionInterval:      evictionInterval,
		evictionDuration:      evictionDuration,
		stopCh:                make(chan struct{}),
		blockIndex:            make(map[int64][]ModelContext),
	}

	// Start eviction worker
	s.wg.Add(1)
	go s.evictionWorker()

	return s
}

// Close stops the eviction worker and cleans up resources
func (s *SyncPrefixHashTable) Close() {
	close(s.stopCh)

	// Wait for eviction worker with timeout
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Clean shutdown
	case <-time.After(5 * time.Second):
		klog.Warning("eviction worker shutdown timeout")
	}
}

// MatchPrefix matches the input token prefix if already cached
// returns map[podname]%prefixmatch along with all prefix hashes
func (s *SyncPrefixHashTable) MatchPrefix(modelName string, loraID int64, tokens []byte, readyPods map[string]struct{}) (map[string]int, []uint64) {
	ctx := ModelContext{
		ModelName: modelName,
		LoraID:    loraID,
	}

	// Compute hashes outside any lock
	prefixHashes := s.GetPrefixHashes(tokens)

	// Lock-free context lookup
	value, exists := s.contextMap.Load(ctx)
	if !exists {
		return map[string]int{}, prefixHashes
	}

	contextData := value.(*ContextData)

	// Check if marked for eviction (lock-free)
	if contextData.markedForEviction.Load() {
		return map[string]int{}, prefixHashes
	}

	// Acquire read lock only for prefixStore
	contextData.prefixMu.RLock()
	defer contextData.prefixMu.RUnlock()

	// Sequential prefix matching
	prefixMatchPods := map[string]int{}
	prefixStore := contextData.prefixStore

	for i, prefixHash := range prefixHashes {
		pods, exists := prefixStore.prefixMap[prefixHash]
		if !exists || len(pods) == 0 {
			break
		}

		prefixMatchPercent := (i + 1) * 100 / len(prefixHashes)

		// Find ready pods with this prefix
		hasMatch := false
		for podName := range pods {
			if _, isReady := readyPods[podName]; isReady {
				prefixMatchPods[podName] = prefixMatchPercent
				hasMatch = true
			}
		}

		if !hasMatch {
			break
		}
	}

	// Update access time (lock-free)
	prefixStore.lastAccess.Store(time.Now().Unix())

	return prefixMatchPods, prefixHashes
}

// ProcessBlockStored handles BlockStored events
func (s *SyncPrefixHashTable) ProcessBlockStored(event BlockStored) error {
	// Validate input
	if len(event.BlockHashes) == 0 {
		return nil
	}
	if len(event.BlockHashes) != len(event.Tokens) {
		return fmt.Errorf("block hashes and tokens length mismatch: %d vs %d", len(event.BlockHashes), len(event.Tokens))
	}

	ctx := ModelContext{
		ModelName: event.ModelName,
		LoraID:    event.LoraID,
	}

	// Get or create context data
	contextData := s.getOrCreateContextData(ctx)

	// First, acquire mapping lock to check what needs to be done
	contextData.mappingMu.Lock()
	defer contextData.mappingMu.Unlock()

	hashMapping := contextData.hashMapping
	prefixUpdates := make([]struct {
		hash uint64
		pod  string
	}, 0)

	// Process blocks with consistent parent hash lookup
	for i, engineBlockHash := range event.BlockHashes {
		// Check if already exists (idempotent)
		if existingHash, exists := hashMapping.engineToAibrix[engineBlockHash]; exists {
			// Just need to update pod info
			prefixUpdates = append(prefixUpdates, struct {
				hash uint64
				pod  string
			}{existingHash, event.SourcePod})
			continue
		}

		// Compute hash with proper parent lookup
		var parentAibrixHash uint64 = s.seed
		if event.ParentBlockHash != nil {
			if ph, exists := hashMapping.engineToAibrix[*event.ParentBlockHash]; exists {
				parentAibrixHash = ph
			}
		}

		// Compute AIBrix hash
		blockTokens := s.getBlockTokens(event, i)
		aibrixHash := s.computeHash(parentAibrixHash, blockTokens)

		// Store mapping
		hashMapping.engineToAibrix[engineBlockHash] = aibrixHash

		// Need to update prefix cache
		prefixUpdates = append(prefixUpdates, struct {
			hash uint64
			pod  string
		}{aibrixHash, event.SourcePod})

		// Update reverse index
		s.updateBlockIndex(engineBlockHash, ctx, true)
	}

	// Now update prefix store if needed
	if len(prefixUpdates) > 0 {
		contextData.prefixMu.Lock()
		defer contextData.prefixMu.Unlock()

		prefixStore := contextData.prefixStore
		for _, update := range prefixUpdates {
			s.addPrefixToPodLocked(prefixStore, update.hash, update.pod)
		}
	}

	return nil
}

// ProcessBlockRemoved handles BlockRemoved events
func (s *SyncPrefixHashTable) ProcessBlockRemoved(event BlockRemoved) error {
	if len(event.BlockHashes) == 0 {
		return nil
	}

	ctx := ModelContext{
		ModelName: event.ModelName,
		LoraID:    event.LoraID,
	}

	// Can now target specific context instead of searching all contexts
	value, exists := s.contextMap.Load(ctx)
	if !exists {
		return nil
	}

	contextData := value.(*ContextData)
	if contextData.markedForEviction.Load() {
		return nil
	}

	// First, check what needs to be removed using mapping lock
	contextData.mappingMu.Lock()
	toRemove := make(map[uint64]bool)
	for _, engineBlockHash := range event.BlockHashes {
		if aibrixHash, exists := contextData.hashMapping.engineToAibrix[engineBlockHash]; exists {
			toRemove[aibrixHash] = true
			delete(contextData.hashMapping.engineToAibrix, engineBlockHash)
		}
		// Update reverse index
		s.updateBlockIndex(engineBlockHash, ctx, false)
	}
	contextData.mappingMu.Unlock()

	// Then update prefix store if needed
	if len(toRemove) > 0 {
		contextData.prefixMu.Lock()
		for aibrixHash := range toRemove {
			delete(contextData.prefixStore.prefixMap, aibrixHash)
			contextData.prefixStore.totalPrefixes--
		}
		contextData.prefixMu.Unlock()
	}

	return nil
}

// ProcessAllBlocksCleared handles AllBlocksCleared events (placeholder implementation)
func (s *SyncPrefixHashTable) ProcessAllBlocksCleared(event AllBlocksCleared) error {
	// Empty implementation as per requirements
	return nil
}

// AddPrefix adds prefix hashes for a specific model/lora context and pod
func (s *SyncPrefixHashTable) AddPrefix(modelName string, loraID int64, podName string, prefixHashes []uint64) error {
	ctx := ModelContext{
		ModelName: modelName,
		LoraID:    loraID,
	}

	// Get or create context (mostly lock-free)
	contextData := s.getOrCreateContextData(ctx)

	// Lock only prefixStore
	contextData.prefixMu.Lock()
	defer contextData.prefixMu.Unlock()

	prefixStore := contextData.prefixStore
	now := time.Now().Unix()

	// Update access time
	prefixStore.lastAccess.Store(now)

	// Batch update prefixes
	for _, prefixHash := range prefixHashes {
		// Check limit
		if prefixStore.totalPrefixes >= int64(s.maxPrefixesPerContext) {
			break
		}

		pods, exists := prefixStore.prefixMap[prefixHash]
		if !exists {
			pods = make(map[string]*PodInfo)
			prefixStore.prefixMap[prefixHash] = pods
			prefixStore.totalPrefixes++
		}

		// Update pod info
		if podInfo, exists := pods[podName]; exists {
			podInfo.LastAccessTime.Store(now)
		} else {
			pods[podName] = &PodInfo{
				SourcePod: podName,
			}
			pods[podName].LastAccessTime.Store(now)
		}
	}

	return nil
}

// RemovePrefix removes a specific pod from all prefix entries
func (s *SyncPrefixHashTable) RemovePrefix(modelName string, loraID int64, podName string) error {
	ctx := ModelContext{
		ModelName: modelName,
		LoraID:    loraID,
	}

	value, exists := s.contextMap.Load(ctx)
	if !exists {
		return nil
	}

	contextData := value.(*ContextData)
	contextData.prefixMu.Lock()
	defer contextData.prefixMu.Unlock()

	prefixStore := contextData.prefixStore

	// Remove pod from all prefix entries
	for prefixHash, pods := range prefixStore.prefixMap {
		delete(pods, podName)
		if len(pods) == 0 {
			delete(prefixStore.prefixMap, prefixHash)
			prefixStore.totalPrefixes--
		}
	}

	return nil
}

// GetPrefixHashes computes prefix hashes for given tokens
func (s *SyncPrefixHashTable) GetPrefixHashes(tokens []byte) []uint64 {
	numBlocks := len(tokens) / s.blockSize
	prefixHashes := make([]uint64, 0, numBlocks)
	digest := xxhash.NewWithSeed(s.seed)

	// Use seed as the initial parent hash (equivalent to NONE_HASH in Python)
	parentHash := s.seed

	var parentHashBytes [8]byte

	for i := 0; i < len(tokens); i += s.blockSize {
		end := i + s.blockSize
		if end > len(tokens) {
			// Don't hash incomplete blocks
			break
		}

		// Reset digest for new block
		digest.ResetWithSeed(s.seed)

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

// Helper methods

// getOrCreateContextData returns existing or creates new context data
func (s *SyncPrefixHashTable) getOrCreateContextData(ctx ModelContext) *ContextData {
	// Fast path: load existing
	if value, exists := s.contextMap.Load(ctx); exists {
		return value.(*ContextData)
	}

	// Slow path: create new
	newContextData := &ContextData{
		prefixStore: &PrefixStore{
			prefixMap:     make(map[uint64]map[string]*PodInfo),
			createTime:    time.Now(),
			totalPrefixes: 0,
		},
		hashMapping: &EngineHashMapping{
			engineToAibrix: make(map[int64]uint64),
		},
	}
	newContextData.prefixStore.lastAccess.Store(time.Now().Unix())

	// LoadOrStore ensures only one creation succeeds
	actual, loaded := s.contextMap.LoadOrStore(ctx, newContextData)
	if !loaded {
		// We created it
		currentCount := s.contextCount.Add(1)

		// Check limit
		if int(currentCount) > s.maxContexts {
			// Mark for eviction in background
			s.scheduleEviction()
		}
	}

	return actual.(*ContextData)
}

// computeHash computes hash given parent hash and tokens
func (s *SyncPrefixHashTable) computeHash(parentHash uint64, blockTokens []byte) uint64 {
	digest := xxhash.NewWithSeed(s.seed)
	var parentHashBytes [8]byte
	binary.LittleEndian.PutUint64(parentHashBytes[:], parentHash)
	_, _ = digest.Write(parentHashBytes[:])
	_, _ = digest.Write(blockTokens)
	return digest.Sum64()
}

// addPrefixToPodLocked adds or updates pod info for a prefix (caller must hold lock)
func (s *SyncPrefixHashTable) addPrefixToPodLocked(prefixStore *PrefixStore, prefixHash uint64, podName string) {
	now := time.Now().Unix()

	pods, exists := prefixStore.prefixMap[prefixHash]
	if !exists {
		pods = make(map[string]*PodInfo)
		prefixStore.prefixMap[prefixHash] = pods
		prefixStore.totalPrefixes++
	}

	if podInfo, exists := pods[podName]; exists {
		podInfo.LastAccessTime.Store(now)
	} else {
		pods[podName] = &PodInfo{
			SourcePod: podName,
		}
		pods[podName].LastAccessTime.Store(now)
	}
}

// getBlockTokens extracts tokens for a specific block
func (s *SyncPrefixHashTable) getBlockTokens(event BlockStored, index int) []byte {
	if index < len(event.Tokens) {
		return event.Tokens[index]
	}
	return []byte{}
}

// scheduleEviction triggers an eviction check
func (s *SyncPrefixHashTable) scheduleEviction() {
	// Set flag to trigger immediate eviction
	s.evictionNeeded.Store(true)
}

// Eviction methods

// evictionWorker runs the background eviction process
func (s *SyncPrefixHashTable) evictionWorker() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.evictionInterval)
	defer ticker.Stop()

	checkTicker := time.NewTicker(1 * time.Second) // Check for immediate eviction
	defer checkTicker.Stop()

	for {
		select {
		case <-ticker.C:
			s.performEviction()
		case <-checkTicker.C:
			// Check if immediate eviction needed
			if s.evictionNeeded.CompareAndSwap(true, false) {
				s.performEviction()
			}
		case <-s.stopCh:
			return
		}
	}
}

// performEviction executes the eviction logic
func (s *SyncPrefixHashTable) performEviction() {
	// Skip if already running
	if !s.evictionRunning.CompareAndSwap(false, true) {
		return
	}
	defer s.evictionRunning.Store(false)

	now := time.Now().Unix()
	expiredBefore := now - int64(s.evictionDuration.Seconds())

	// Phase 1: Mark contexts for eviction (lock-free)
	evictionCandidates := make([]interface{}, 0)

	s.contextMap.Range(func(key, value interface{}) bool {
		contextData := value.(*ContextData)
		lastAccess := contextData.prefixStore.lastAccess.Load()

		if lastAccess < expiredBefore {
			contextData.markedForEviction.Store(true)
			evictionCandidates = append(evictionCandidates, key)
		}

		return true
	})

	// Phase 2: Remove marked contexts
	for _, key := range evictionCandidates {
		s.contextMap.Delete(key)
		s.contextCount.Add(-1)
	}

	// Phase 3: Clean up expired pods within active contexts
	s.contextMap.Range(func(key, value interface{}) bool {
		contextData := value.(*ContextData)
		if contextData.markedForEviction.Load() {
			return true // Skip marked contexts
		}

		return true
	})
}

// evictExpiredPodsInBatch processes multiple contexts to remove expired pods
func (s *SyncPrefixHashTable) evictExpiredPodsInBatch(contexts []*ContextData, expiredBefore int64) {
	for _, contextData := range contexts {
		contextData.prefixMu.Lock()
		prefixStore := contextData.prefixStore

		for prefixHash, pods := range prefixStore.prefixMap {
			// Remove expired pods
			for podName, podInfo := range pods {
				if podInfo.LastAccessTime.Load() < expiredBefore {
					delete(pods, podName)
				}
			}

			// Remove empty prefix entries
			if len(pods) == 0 {
				delete(prefixStore.prefixMap, prefixHash)
				prefixStore.totalPrefixes--
			}
		}
		contextData.prefixMu.Unlock()
	}
}

// enforceContextLimit removes oldest contexts to stay within limit
func (s *SyncPrefixHashTable) enforceContextLimit(excess int) {
	type contextAge struct {
		key        interface{}
		lastAccess int64
	}

	ages := make([]contextAge, 0, excess*2)

	// Collect context ages
	s.contextMap.Range(func(key, value interface{}) bool {
		contextData := value.(*ContextData)
		if !contextData.markedForEviction.Load() {
			ages = append(ages, contextAge{
				key:        key,
				lastAccess: contextData.prefixStore.lastAccess.Load(),
			})
		}
		return true
	})

	// Sort by age (oldest first)
	sort.Slice(ages, func(i, j int) bool {
		return ages[i].lastAccess < ages[j].lastAccess
	})

	// Remove oldest contexts
	removed := 0
	for _, age := range ages {
		if removed >= excess {
			break
		}
		s.contextMap.Delete(age.key)
		s.contextCount.Add(-1)
		removed++
	}
}

// updateBlockIndex updates the reverse index for block to context mapping
func (s *SyncPrefixHashTable) updateBlockIndex(blockHash int64, ctx ModelContext, add bool) {
	s.blockIndexMu.Lock()
	defer s.blockIndexMu.Unlock()

	if add {
		contexts := s.blockIndex[blockHash]
		// Check if already exists
		for _, existingCtx := range contexts {
			if existingCtx == ctx {
				return
			}
		}
		s.blockIndex[blockHash] = append(contexts, ctx)
	} else {
		contexts := s.blockIndex[blockHash]
		for i, existingCtx := range contexts {
			if existingCtx == ctx {
				// Remove this context
				s.blockIndex[blockHash] = append(contexts[:i], contexts[i+1:]...)
				if len(s.blockIndex[blockHash]) == 0 {
					delete(s.blockIndex, blockHash)
				}
				break
			}
		}
	}
}
