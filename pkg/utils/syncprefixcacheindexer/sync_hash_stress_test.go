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
	crand "crypto/rand"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test configuration
var (
	// Block size configurations to test
	blockSizeConfigs = []int{16, 32, 64, 96}

	// Realistic model/LoRA configuration
	stressTestNumModels   = 5  // 5 base models
	stressTestMaxLoRAs    = 2  // 0-2 LoRA adapters per model
	stressTestMaxContexts = 10 // Total contexts < 10

	// Scale parameters
	stressTestNumBlocks  = 1000000 // 1M blocks default
	stressTestNumReaders = 1000    // 1000 concurrent readers
	stressTestNumPods    = 100     // 100 pods total

	// Operation ratios for realistic workload
	stressTestReadRatio   = 0.65 // 65% reads
	stressTestWriteRatio  = 0.25 // 25% writes
	stressTestRemoveRatio = 0.10 // 10% removes

	stressTestProgressInterval = 10000 // Report progress every N operations
)

// Performance tracking
type performanceStats struct {
	totalOps      atomic.Int64
	totalDuration atomic.Int64 // nanoseconds
	maxLatency    atomic.Int64 // nanoseconds
	errors        atomic.Int64
}

func (p *performanceStats) recordOp(duration time.Duration) {
	p.totalOps.Add(1)
	p.totalDuration.Add(int64(duration))

	// Update max latency
	currentMax := p.maxLatency.Load()
	if int64(duration) > currentMax {
		p.maxLatency.CompareAndSwap(currentMax, int64(duration))
	}
}

func (p *performanceStats) throughput() float64 {
	ops := p.totalOps.Load()
	duration := p.totalDuration.Load()
	if duration == 0 {
		return 0
	}
	return float64(ops) / (float64(duration) / 1e9)
}

func (p *performanceStats) avgLatency() time.Duration {
	ops := p.totalOps.Load()
	if ops == 0 {
		return 0
	}
	return time.Duration(p.totalDuration.Load() / ops)
}

// Helper functions

func generateTokens(size int) []byte {
	tokens := make([]byte, size)
	_, _ = crand.Read(tokens)
	return tokens
}

// generateTokensForBlockSize generates tokens that are multiples of block size
func generateTokensForBlockSize(numBlocks int, blockSize int) []byte {
	totalSize := numBlocks * blockSize
	return generateTokens(totalSize)
}

// getModelLoRAContext generates a realistic model/LoRA combination
func getModelLoRAContext(index int) (string, int64) {
	modelID := index % stressTestNumModels
	loraID := int64((index/stressTestNumModels)%(stressTestMaxLoRAs+1)) - 1 // -1 means no LoRA
	return fmt.Sprintf("model-%d", modelID), loraID
}

func generateBlockStoredEvent(modelID, startBlock, numBlocks int, blockSize int) BlockStored {
	blockHashes := make([]int64, numBlocks)
	tokens := make([][]byte, numBlocks)

	for i := 0; i < numBlocks; i++ {
		blockHashes[i] = int64(startBlock + i)
		tokens[i] = generateTokens(blockSize)
	}

	_, loraID := getModelLoRAContext(modelID)
	var parentHash *int64
	if startBlock > 0 {
		ph := int64(startBlock - 1)
		parentHash = &ph
	}

	return BlockStored{
		BlockHashes:     blockHashes,
		ParentBlockHash: parentHash,
		Tokens:          tokens,
		ModelName:       fmt.Sprintf("model-%d", modelID%stressTestNumModels),
		LoraID:          loraID,
		SourcePod:       "", // Will be set by caller
	}
}

func reportProgress(name string, current, total int) {
	if current%stressTestProgressInterval == 0 || current == total {
		pct := float64(current) * 100 / float64(total)
		fmt.Printf("[%s] Progress: %d/%d (%.1f%%)\n", name, current, total, pct)
	}
}

// BenchmarkMassiveBlockIngestion tests ingesting millions of blocks
func BenchmarkMassiveBlockIngestion(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	configs := []struct {
		name      string
		numBlocks int
		blockSize int
		batchSize int
	}{
		{"1M_Blocks_16B", 1000000, 16, 100},
		{"1M_Blocks_32B", 1000000, 32, 100},
		{"1M_Blocks_64B", 1000000, 64, 100},
		{"1M_Blocks_96B", 1000000, 96, 100},
		{"5M_Blocks_16B", 5000000, 16, 500},
		{"10M_Blocks_16B", 10000000, 16, 1000},
	}

	for _, cfg := range configs {
		b.Run(cfg.name, func(b *testing.B) {
			table := NewSyncPrefixHashTable()
			defer table.Close()

			stats := &performanceStats{}
			startTime := time.Now()

			b.ResetTimer()

			// Generate and process blocks across limited contexts
			contextsUsed := stressTestMaxContexts
			blocksPerContext := cfg.numBlocks / contextsUsed
			currentBlock := 0

			for ctxID := 0; ctxID < contextsUsed; ctxID++ {
				podID := ctxID % stressTestNumPods

				for batch := 0; batch < blocksPerContext/cfg.batchSize; batch++ {
					event := generateBlockStoredEvent(ctxID, currentBlock, cfg.batchSize, cfg.blockSize)
					event.SourcePod = fmt.Sprintf("pod-%d", podID)

					opStart := time.Now()
					err := table.ProcessBlockStored(event)
					opDuration := time.Since(opStart)

					if err != nil {
						stats.errors.Add(1)
						b.Logf("Error processing block: %v", err)
					} else {
						stats.recordOp(opDuration)
					}

					currentBlock += cfg.batchSize
					reportProgress(cfg.name, currentBlock, cfg.numBlocks)
				}
			}

			// Report results
			elapsed := time.Since(startTime)
			b.Logf("Total time: %v", elapsed)
			b.Logf("Blocks processed: %d", stats.totalOps.Load())
			b.Logf("Throughput: %.2f blocks/sec", stats.throughput())
			b.Logf("Avg latency: %v", stats.avgLatency())
			b.Logf("Max latency: %v", time.Duration(stats.maxLatency.Load()))
			b.Logf("Errors: %d", stats.errors.Load())
			b.Logf("Context count: %d", table.contextCount.Load())

			// Memory stats
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			b.Logf("Memory allocated: %.2f MB", float64(m.Alloc)/1024/1024)
			b.Logf("Total memory: %.2f MB", float64(m.TotalAlloc)/1024/1024)
		})
	}
}

// BenchmarkHighConcurrencyPrefixMatching tests 1000+ concurrent readers
func BenchmarkHighConcurrencyPrefixMatching(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	configs := []struct {
		name        string
		numReaders  int
		numPrefixes int
		numBlocks   int // Number of blocks in each prefix
		blockSize   int
	}{
		{"100_Readers_100K_Prefixes_16B", 100, 100000, 100, 16},
		{"500_Readers_500K_Prefixes_16B", 500, 500000, 100, 16},
		{"1000_Readers_1M_Prefixes_16B", 1000, 1000000, 100, 16},
		{"1000_Readers_1M_Prefixes_32B", 1000, 1000000, 100, 32},
		{"1000_Readers_1M_Prefixes_64B", 1000, 1000000, 100, 64},
	}

	for _, cfg := range configs {
		b.Run(cfg.name, func(b *testing.B) {
			table := NewSyncPrefixHashTable()
			defer table.Close()

			// Pre-populate with prefixes
			fmt.Printf("[%s] Pre-populating %d prefixes...\n", cfg.name, cfg.numPrefixes)
			contextsUsed := stressTestMaxContexts
			prefixesPerContext := cfg.numPrefixes / contextsUsed

			for ctxID := 0; ctxID < contextsUsed; ctxID++ {
				modelName, loraID := getModelLoRAContext(ctxID)

				for i := 0; i < prefixesPerContext; i++ {
					tokens := generateTokensForBlockSize(cfg.numBlocks, cfg.blockSize)
					hashes := table.GetPrefixHashes(tokens)
					podName := fmt.Sprintf("pod-%d", (ctxID*10+i)%stressTestNumPods)

					err := table.AddPrefix(modelName, loraID, podName, hashes)
					if err != nil {
						b.Fatalf("Failed to add prefix: %v", err)
					}
				}
				reportProgress(fmt.Sprintf("%s_populate", cfg.name), (ctxID+1)*prefixesPerContext, cfg.numPrefixes)
			}

			fmt.Printf("[%s] Starting concurrent matching with %d readers...\n", cfg.name, cfg.numReaders)

			// Prepare test data
			type testQuery struct {
				modelName string
				loraID    int64
				tokens    []byte
				readyPods map[string]struct{}
			}

			queries := make([]testQuery, cfg.numReaders)
			for i := 0; i < cfg.numReaders; i++ {
				ctxID := i % contextsUsed
				modelName, loraID := getModelLoRAContext(ctxID)
				queries[i] = testQuery{
					modelName: modelName,
					loraID:    loraID,
					tokens:    generateTokensForBlockSize(cfg.numBlocks, cfg.blockSize),
					readyPods: make(map[string]struct{}),
				}
				// Add some ready pods
				for j := 0; j < 20; j++ {
					podID := (ctxID*10 + j) % stressTestNumPods
					queries[i].readyPods[fmt.Sprintf("pod-%d", podID)] = struct{}{}
				}
			}

			b.ResetTimer()

			// Run concurrent matching
			stats := &performanceStats{}
			var wg sync.WaitGroup
			matchesPerReader := 1000

			startTime := time.Now()

			for i := 0; i < cfg.numReaders; i++ {
				wg.Add(1)
				go func(readerID int) {
					defer wg.Done()

					query := queries[readerID]

					for j := 0; j < matchesPerReader; j++ {
						opStart := time.Now()
						matches, _ := table.MatchPrefix(query.modelName, query.loraID, query.tokens, query.readyPods)
						opDuration := time.Since(opStart)

						stats.recordOp(opDuration)

						if len(matches) == 0 && j == 0 {
							// Log first attempt for debugging
							b.Logf("Reader %d: No matches found for model %s", readerID, query.modelName)
						}
					}
				}(i)
			}

			wg.Wait()
			elapsed := time.Since(startTime)

			// Report results
			totalOps := int64(cfg.numReaders * matchesPerReader)
			b.Logf("Total time: %v", elapsed)
			b.Logf("Total operations: %d", totalOps)
			b.Logf("Throughput: %.2f ops/sec", float64(totalOps)/elapsed.Seconds())
			b.Logf("Avg latency: %v", stats.avgLatency())
			b.Logf("Max latency: %v", time.Duration(stats.maxLatency.Load()))
		})
	}
}

// BenchmarkRealisticWorkload simulates real production patterns
func BenchmarkRealisticWorkload(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Configuration
	numWorkers := 500
	duration := 30 * time.Second
	readRatio := stressTestReadRatio   // 65%
	writeRatio := stressTestWriteRatio // 25%
	// removeRatio := stressTestRemoveRatio (10%)

	// Pre-populate with initial blocks
	fmt.Println("Pre-populating with initial data...")
	blockSize := 16 // Use default block size
	for ctxID := 0; ctxID < stressTestMaxContexts; ctxID++ {
		event := generateBlockStoredEvent(ctxID, ctxID*10000, 1000, blockSize)
		event.SourcePod = fmt.Sprintf("pod-%d", ctxID%stressTestNumPods)
		_ = table.ProcessBlockStored(event)
	}

	b.ResetTimer()

	// Statistics
	readStats := &performanceStats{}
	writeStats := &performanceStats{}
	removeStats := &performanceStats{}

	// Run mixed workload
	var wg sync.WaitGroup
	stopCh := make(chan struct{})

	// Start timer
	go func() {
		time.Sleep(duration)
		close(stopCh)
	}()

	fmt.Printf("Running realistic workload for %v with %d workers...\n", duration, numWorkers)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			ctxID := workerID % stressTestMaxContexts
			modelName, loraID := getModelLoRAContext(ctxID)
			blockCounter := workerID * 100000

			for {
				select {
				case <-stopCh:
					return
				default:
					r := rand.Float64()

					if r < readRatio {
						// Read operation - match prefix
						numBlocks := rand.Intn(1000) + 100 // 100-1100 blocks
						tokens := generateTokensForBlockSize(numBlocks, blockSize)
						readyPods := make(map[string]struct{})
						// Add random ready pods
						for j := 0; j < 20; j++ {
							podID := rand.Intn(stressTestNumPods)
							readyPods[fmt.Sprintf("pod-%d", podID)] = struct{}{}
						}

						opStart := time.Now()
						table.MatchPrefix(modelName, loraID, tokens, readyPods)
						readStats.recordOp(time.Since(opStart))

					} else if r < readRatio+writeRatio {
						// Write operation - store new blocks
						numBlocks := rand.Intn(50) + 10 // 10-60 blocks per event
						event := generateBlockStoredEvent(ctxID, blockCounter, numBlocks, blockSize)
						blockCounter += numBlocks
						event.SourcePod = fmt.Sprintf("pod-%d", workerID%stressTestNumPods)

						opStart := time.Now()
						err := table.ProcessBlockStored(event)
						if err != nil {
							writeStats.errors.Add(1)
						} else {
							writeStats.recordOp(time.Since(opStart))
						}

					} else {
						// Remove operation - simulate eviction
						numBlocksToRemove := rand.Intn(20) + 1
						blocksToRemove := make([]int64, numBlocksToRemove)
						for j := 0; j < numBlocksToRemove; j++ {
							blocksToRemove[j] = int64(rand.Intn(blockCounter))
						}

						removeEvent := BlockRemoved{
							BlockHashes: blocksToRemove,
							ModelName:   modelName,
							LoraID:      loraID,
						}

						opStart := time.Now()
						err := table.ProcessBlockRemoved(removeEvent)
						if err != nil {
							removeStats.errors.Add(1)
						} else {
							removeStats.recordOp(time.Since(opStart))
						}
					}
				}
			}
		}(i)
	}

	wg.Wait()

	// Report results
	b.Logf("\n=== Realistic Workload Results ===")
	b.Logf("Duration: %v", duration)
	b.Logf("\nRead Operations:")
	b.Logf("  Total: %d", readStats.totalOps.Load())
	b.Logf("  Throughput: %.2f ops/sec", readStats.throughput())
	b.Logf("  Avg latency: %v", readStats.avgLatency())
	b.Logf("  Max latency: %v", time.Duration(readStats.maxLatency.Load()))

	b.Logf("\nWrite Operations:")
	b.Logf("  Total: %d", writeStats.totalOps.Load())
	b.Logf("  Throughput: %.2f ops/sec", writeStats.throughput())
	b.Logf("  Avg latency: %v", writeStats.avgLatency())
	b.Logf("  Max latency: %v", time.Duration(writeStats.maxLatency.Load()))
	b.Logf("  Errors: %d", writeStats.errors.Load())

	b.Logf("\nRemove Operations:")
	b.Logf("  Total: %d", removeStats.totalOps.Load())
	b.Logf("  Throughput: %.2f ops/sec", removeStats.throughput())
	b.Logf("  Avg latency: %v", removeStats.avgLatency())
	b.Logf("  Max latency: %v", time.Duration(removeStats.maxLatency.Load()))
	b.Logf("  Errors: %d", removeStats.errors.Load())

	b.Logf("\nFinal context count: %d", table.contextCount.Load())
}

// BenchmarkBlockSizeComparison directly compares different block sizes
func BenchmarkBlockSizeComparison(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	for _, blockSize := range blockSizeConfigs {
		b.Run(fmt.Sprintf("BlockSize_%dB", blockSize), func(b *testing.B) {
			// Override the default block size for this test
			originalBlockSize := prefixCacheBlockSize
			prefixCacheBlockSize = blockSize
			defer func() { prefixCacheBlockSize = originalBlockSize }()

			table := NewSyncPrefixHashTable()
			defer table.Close()

			// Test parameters
			numBlocks := 1000 // 1000 blocks per operation

			// Measure hash computation
			b.Run("HashComputation", func(b *testing.B) {
				tokens := generateTokensForBlockSize(numBlocks, blockSize)
				b.SetBytes(int64(len(tokens)))
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					_ = table.GetPrefixHashes(tokens)
				}
			})

			// Measure block ingestion
			b.Run("BlockIngestion", func(b *testing.B) {
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					event := generateBlockStoredEvent(i%stressTestMaxContexts, i*100, 100, blockSize)
					event.SourcePod = fmt.Sprintf("pod-%d", i%stressTestNumPods)
					_ = table.ProcessBlockStored(event)
				}
			})

			// Measure prefix matching
			b.Run("PrefixMatching", func(b *testing.B) {
				// Pre-populate
				for i := 0; i < 1000; i++ {
					tokens := generateTokensForBlockSize(numBlocks, blockSize)
					hashes := table.GetPrefixHashes(tokens)
					modelName, loraID := getModelLoRAContext(i % stressTestMaxContexts)
					_ = table.AddPrefix(modelName, loraID, fmt.Sprintf("pod-%d", i%stressTestNumPods), hashes)
				}

				// Prepare test data
				tokens := generateTokensForBlockSize(numBlocks, blockSize)
				modelName, loraID := getModelLoRAContext(0)
				readyPods := make(map[string]struct{})
				for i := 0; i < 20; i++ {
					readyPods[fmt.Sprintf("pod-%d", i)] = struct{}{}
				}

				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					table.MatchPrefix(modelName, loraID, tokens, readyPods)
				}
			})

			// Report memory usage
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			b.Logf("Block size %dB: Memory allocated: %.2f MB", blockSize, float64(m.Alloc)/1024/1024)
		})
	}
}

// BenchmarkMemoryPressure tests eviction under memory constraints
func BenchmarkMemoryPressure(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	// Create table with aggressive eviction settings
	table := &SyncPrefixHashTable{
		seed:                  12345,
		maxContexts:           stressTestMaxContexts, // Limited contexts
		maxPrefixesPerContext: 1000000,               // Allow many prefixes per context
		blockSize:             16,                    // Default block size
		evictionInterval:      1 * time.Second,
		evictionDuration:      30 * time.Second,
		stopCh:                make(chan struct{}),
		blockIndex:            make(map[int64][]ModelContext),
	}

	// Start eviction worker
	table.wg.Add(1)
	go table.evictionWorker()
	defer table.Close()

	b.ResetTimer()

	// Fill with massive blocks to test memory pressure
	fmt.Println("Filling table with millions of blocks to test memory pressure...")
	targetBlocks := 10000000 // 10M blocks
	blockSize := 16
	batchSize := 1000

	stats := &performanceStats{}
	currentBlock := 0

	// Use limited contexts but massive blocks
	for currentBlock < targetBlocks {
		for ctxID := 0; ctxID < stressTestMaxContexts && currentBlock < targetBlocks; ctxID++ {
			event := generateBlockStoredEvent(ctxID, currentBlock, batchSize, blockSize)
			event.SourcePod = fmt.Sprintf("pod-%d", ctxID%stressTestNumPods)

			opStart := time.Now()
			err := table.ProcessBlockStored(event)
			if err != nil {
				stats.errors.Add(1)
			} else {
				stats.recordOp(time.Since(opStart))
			}

			currentBlock += batchSize
			reportProgress("MemoryPressure", currentBlock, targetBlocks)
		}

		// Periodically report memory stats
		if currentBlock%(100*batchSize) == 0 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			contextCount := table.contextCount.Load()
			b.Logf("Blocks: %d, Contexts: %d, Memory: %.2f MB, Eviction needed: %v",
				currentBlock, contextCount, float64(m.Alloc)/1024/1024, table.evictionNeeded.Load())
		}
	}

	// Wait for eviction to stabilize
	time.Sleep(5 * time.Second)

	// Final stats
	finalCount := table.contextCount.Load()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	b.Logf("\n=== Memory Pressure Results ===")
	b.Logf("Target blocks: %d", targetBlocks)
	b.Logf("Blocks processed: %d", currentBlock)
	b.Logf("Final context count: %d", finalCount)
	b.Logf("Final memory: %.2f MB", float64(m.Alloc)/1024/1024)
	b.Logf("Total operations: %d", stats.totalOps.Load())
	b.Logf("Avg latency: %v", stats.avgLatency())
	b.Logf("Throughput: %.2f ops/sec", stats.throughput())
	b.Logf("Errors: %d", stats.errors.Load())
}

// BenchmarkLargeTokenSequences tests with realistic LLM context sizes
func BenchmarkLargeTokenSequences(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	configs := []struct {
		name      string
		numBlocks int
		blockSize int
		numOps    int
	}{
		{"2K_Blocks_16B", 2048, 16, 1000}, // 32KB total
		{"4K_Blocks_16B", 4096, 16, 500},  // 64KB total
		{"8K_Blocks_16B", 8192, 16, 200},  // 128KB total
		{"2K_Blocks_32B", 2048, 32, 1000}, // 64KB total
		{"4K_Blocks_32B", 4096, 32, 500},  // 128KB total
	}

	for _, cfg := range configs {
		b.Run(cfg.name, func(b *testing.B) {
			table := NewSyncPrefixHashTable()
			defer table.Close()

			// Generate large token sequences
			tokens := generateTokensForBlockSize(cfg.numBlocks, cfg.blockSize)
			modelName, loraID := getModelLoRAContext(0)

			b.ResetTimer()
			b.SetBytes(int64(len(tokens)))

			stats := &performanceStats{}

			// Test hash computation
			b.Run("HashComputation", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					opStart := time.Now()
					hashes := table.GetPrefixHashes(tokens)
					stats.recordOp(time.Since(opStart))

					if i == 0 {
						b.Logf("Generated %d hashes from %d blocks (%d bytes)", len(hashes), cfg.numBlocks, len(tokens))
					}
				}

				b.Logf("Avg hash computation time: %v", stats.avgLatency())
			})

			// Test prefix operations
			b.Run("PrefixOperations", func(b *testing.B) {
				hashes := table.GetPrefixHashes(tokens)

				// Add prefix
				for i := 0; i < 10; i++ {
					podName := fmt.Sprintf("pod-%d", i)
					err := table.AddPrefix(modelName, loraID, podName, hashes)
					if err != nil {
						b.Fatalf("Failed to add prefix: %v", err)
					}
				}

				// Match prefix
				readyPods := map[string]struct{}{
					"pod-0": {},
					"pod-1": {},
					"pod-2": {},
				}

				matchStats := &performanceStats{}

				for i := 0; i < cfg.numOps; i++ {
					opStart := time.Now()
					matches, _ := table.MatchPrefix(modelName, loraID, tokens, readyPods)
					matchStats.recordOp(time.Since(opStart))

					if i == 0 {
						b.Logf("Matched %d pods", len(matches))
					}
				}

				b.Logf("Avg match time for %d blocks: %v", cfg.numBlocks, matchStats.avgLatency())
			})
		})
	}
}

// BenchmarkCascadingBlockRemoval tests reverse index efficiency
func BenchmarkCascadingBlockRemoval(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Setup: Create blocks shared across limited contexts
	numContexts := stressTestMaxContexts // Use limited contexts
	numSharedBlocks := 10000             // Many shared blocks
	numUniqueBlocksPerContext := 5000
	blockSize := 16

	fmt.Printf("Setting up %d contexts with %d shared blocks...\n", numContexts, numSharedBlocks)

	// Create shared blocks first
	sharedBlockHashes := make([]int64, numSharedBlocks)
	for i := 0; i < numSharedBlocks; i++ {
		sharedBlockHashes[i] = int64(i)
	}

	// Add blocks to contexts
	for ctxID := 0; ctxID < numContexts; ctxID++ {
		modelName, loraID := getModelLoRAContext(ctxID)

		// Add shared blocks
		sharedTokens := make([][]byte, numSharedBlocks)
		for i := 0; i < numSharedBlocks; i++ {
			sharedTokens[i] = generateTokens(blockSize)
		}

		sharedEvent := BlockStored{
			BlockHashes: sharedBlockHashes,
			Tokens:      sharedTokens,
			ModelName:   modelName,
			LoraID:      loraID,
			SourcePod:   fmt.Sprintf("pod-%d", ctxID%stressTestNumPods),
		}

		err := table.ProcessBlockStored(sharedEvent)
		if err != nil {
			b.Fatalf("Failed to store shared blocks: %v", err)
		}

		// Add unique blocks
		uniqueHashes := make([]int64, numUniqueBlocksPerContext)
		uniqueTokens := make([][]byte, numUniqueBlocksPerContext)
		for i := 0; i < numUniqueBlocksPerContext; i++ {
			uniqueHashes[i] = int64(numSharedBlocks + ctxID*numUniqueBlocksPerContext + i)
			uniqueTokens[i] = generateTokens(blockSize)
		}

		uniqueEvent := BlockStored{
			BlockHashes: uniqueHashes,
			Tokens:      uniqueTokens,
			ModelName:   modelName,
			LoraID:      loraID,
			SourcePod:   fmt.Sprintf("pod-%d", ctxID%stressTestNumPods),
		}

		err = table.ProcessBlockStored(uniqueEvent)
		if err != nil {
			b.Fatalf("Failed to store unique blocks: %v", err)
		}

		reportProgress("Setup", ctxID+1, numContexts)
	}

	// Verify reverse index
	table.blockIndexMu.RLock()
	sharedBlockContexts := len(table.blockIndex[sharedBlockHashes[0]])
	table.blockIndexMu.RUnlock()

	b.Logf("Shared block 0 is referenced by %d contexts", sharedBlockContexts)

	b.ResetTimer()

	// Benchmark removal of shared blocks
	stats := &performanceStats{}

	fmt.Println("Starting cascading block removal...")

	for i := 0; i < numSharedBlocks; i++ {
		// Remove the shared block from all contexts
		blockStart := time.Now()
		for ctxID := 0; ctxID < numContexts; ctxID++ {
			modelName, loraID := getModelLoRAContext(ctxID)
			removeEvent := BlockRemoved{
				BlockHashes: []int64{sharedBlockHashes[i]},
				ModelName:   modelName,
				LoraID:      loraID,
			}

			opStart := time.Now()
			err := table.ProcessBlockRemoved(removeEvent)
			opDuration := time.Since(opStart)

			if err != nil {
				stats.errors.Add(1)
			} else {
				stats.recordOp(opDuration)
			}
		}
		totalDuration := time.Since(blockStart)

		if i%10 == 0 {
			b.Logf("Removed block %d (used by %d contexts) in %v",
				sharedBlockHashes[i], numContexts, totalDuration)
		}
	}

	// Report results
	b.Logf("\n=== Cascading Removal Results ===")
	b.Logf("Blocks removed: %d", stats.totalOps.Load())
	b.Logf("Contexts affected per block: %d", numContexts)
	b.Logf("Total removal operations: %d", stats.totalOps.Load()*int64(numContexts))
	b.Logf("Avg removal time per block: %v", stats.avgLatency())
	b.Logf("Max removal time: %v", time.Duration(stats.maxLatency.Load()))
	b.Logf("Throughput: %.2f blocks/sec", stats.throughput())
	b.Logf("Errors: %d", stats.errors.Load())

	// Verify blocks were removed
	table.blockIndexMu.RLock()
	remainingShared := 0
	for _, hash := range sharedBlockHashes {
		if _, exists := table.blockIndex[hash]; exists {
			remainingShared++
		}
	}
	table.blockIndexMu.RUnlock()

	b.Logf("Remaining shared blocks in index: %d", remainingShared)
}

// BenchmarkExtremeConcurrency tests extreme concurrent access patterns
func BenchmarkExtremeConcurrency(b *testing.B) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Pre-populate
	numModels := 100
	for i := 0; i < numModels; i++ {
		event := generateBlockStoredEvent(i, i*100, 50, 1024)
		event.SourcePod = fmt.Sprintf("pod-%d", i%10)
		_ = table.ProcessBlockStored(event)
	}

	b.ResetTimer()

	// Test with extreme concurrency
	numGoroutines := 10000
	opsPerGoroutine := 100

	fmt.Printf("Starting extreme concurrency test with %d goroutines...\n", numGoroutines)

	stats := &performanceStats{}
	var wg sync.WaitGroup

	startTime := time.Now()

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			modelID := id % numModels
			modelName := fmt.Sprintf("model-%d", modelID)
			loraID := int64(modelID % 10)

			for j := 0; j < opsPerGoroutine; j++ {
				opType := rand.Intn(3)

				switch opType {
				case 0: // Read
					tokens := generateTokens(1024)
					readyPods := map[string]struct{}{fmt.Sprintf("pod-%d", modelID%10): {}}

					opStart := time.Now()
					table.MatchPrefix(modelName, loraID, tokens, readyPods)
					stats.recordOp(time.Since(opStart))

				case 1: // Write
					tokens := generateTokens(1024)
					hashes := table.GetPrefixHashes(tokens)
					podName := fmt.Sprintf("pod-%d-%d", modelID, j)

					opStart := time.Now()
					_ = table.AddPrefix(modelName, loraID, podName, hashes)
					stats.recordOp(time.Since(opStart))

				case 2: // Block operation
					event := generateBlockStoredEvent(modelID, id*1000+j, 1, 1024)
					event.SourcePod = fmt.Sprintf("pod-%d", modelID%10)

					opStart := time.Now()
					_ = table.ProcessBlockStored(event)
					stats.recordOp(time.Since(opStart))
				}
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	// Report results
	totalOps := int64(numGoroutines * opsPerGoroutine)
	b.Logf("\n=== Extreme Concurrency Results ===")
	b.Logf("Goroutines: %d", numGoroutines)
	b.Logf("Total operations: %d", totalOps)
	b.Logf("Total time: %v", elapsed)
	b.Logf("Throughput: %.2f ops/sec", float64(totalOps)/elapsed.Seconds())
	b.Logf("Avg latency: %v", stats.avgLatency())
	b.Logf("Max latency: %v", time.Duration(stats.maxLatency.Load()))

	// Check for goroutine leaks
	time.Sleep(100 * time.Millisecond)
	finalGoroutines := runtime.NumGoroutine()
	b.Logf("Final goroutine count: %d", finalGoroutines)
}

// BenchmarkLockSeparation tests the benefit of separate locks for prefixStore and hashMapping
// This benchmark demonstrates reduced contention between read and write operations
func BenchmarkLockSeparation(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	configs := []struct {
		name         string
		numReaders   int
		numWriters   int
		numContexts  int
		blockSize    int
		opsPerWorker int
	}{
		{"50R_50W_10Ctx", 50, 50, 10, 16, 1000},
		{"100R_50W_10Ctx", 100, 50, 10, 16, 1000},
		{"200R_100W_10Ctx", 200, 100, 10, 16, 1000},
		{"500R_100W_10Ctx", 500, 100, 10, 16, 500},
		{"1000R_200W_10Ctx", 1000, 200, 10, 16, 200},
	}

	for _, cfg := range configs {
		b.Run(cfg.name, func(b *testing.B) {
			table := NewSyncPrefixHashTable()
			defer table.Close()

			// Pre-populate with some data
			fmt.Printf("[%s] Pre-populating %d contexts...\n", cfg.name, cfg.numContexts)
			for ctxID := 0; ctxID < cfg.numContexts; ctxID++ {
				event := generateBlockStoredEvent(ctxID, ctxID*1000, 100, cfg.blockSize)
				event.SourcePod = fmt.Sprintf("pod-%d", ctxID%stressTestNumPods)
				err := table.ProcessBlockStored(event)
				if err != nil {
					b.Fatalf("Failed to pre-populate: %v", err)
				}
			}

			b.ResetTimer()

			// Performance tracking
			readStats := &performanceStats{}
			writeStats := &performanceStats{}

			var wg sync.WaitGroup
			startTime := time.Now()

			// Launch readers - these only need prefixMu
			for i := 0; i < cfg.numReaders; i++ {
				wg.Add(1)
				go func(readerID int) {
					defer wg.Done()

					ctxID := readerID % cfg.numContexts
					modelName, loraID := getModelLoRAContext(ctxID)

					// Prepare query data
					tokens := generateTokensForBlockSize(100, cfg.blockSize) // 100 blocks
					readyPods := make(map[string]struct{})
					for j := 0; j < 20; j++ {
						readyPods[fmt.Sprintf("pod-%d", j)] = struct{}{}
					}

					for j := 0; j < cfg.opsPerWorker; j++ {
						opStart := time.Now()
						matches, _ := table.MatchPrefix(modelName, loraID, tokens, readyPods)
						readStats.recordOp(time.Since(opStart))

						if j == 0 && len(matches) == 0 {
							// Log for debugging if no matches on first attempt
							b.Logf("Reader %d: No matches for context %d", readerID, ctxID)
						}
					}
				}(i)
			}

			// Launch writers - these need both locks (mappingMu first, then prefixMu)
			for i := 0; i < cfg.numWriters; i++ {
				wg.Add(1)
				go func(writerID int) {
					defer wg.Done()

					ctxID := writerID % cfg.numContexts
					blockStart := 100000 + writerID*10000

					for j := 0; j < cfg.opsPerWorker; j++ {
						event := generateBlockStoredEvent(ctxID, blockStart+j*10, 10, cfg.blockSize)
						event.SourcePod = fmt.Sprintf("pod-%d", writerID%stressTestNumPods)

						opStart := time.Now()
						err := table.ProcessBlockStored(event)
						if err != nil {
							writeStats.errors.Add(1)
						} else {
							writeStats.recordOp(time.Since(opStart))
						}
					}
				}(i)
			}

			wg.Wait()
			elapsed := time.Since(startTime)

			// Report results
			b.Logf("\n=== Lock Separation Results ===")
			b.Logf("Configuration: %d readers, %d writers, %d contexts", cfg.numReaders, cfg.numWriters, cfg.numContexts)
			b.Logf("Total elapsed time: %v", elapsed)

			b.Logf("\nRead Operations (MatchPrefix - only needs prefixMu):")
			b.Logf("  Total: %d", readStats.totalOps.Load())
			b.Logf("  Throughput: %.2f ops/sec", readStats.throughput())
			b.Logf("  Avg latency: %v", readStats.avgLatency())
			b.Logf("  Max latency: %v", time.Duration(readStats.maxLatency.Load()))

			b.Logf("\nWrite Operations (ProcessBlockStored - needs both locks):")
			b.Logf("  Total: %d", writeStats.totalOps.Load())
			b.Logf("  Throughput: %.2f ops/sec", writeStats.throughput())
			b.Logf("  Avg latency: %v", writeStats.avgLatency())
			b.Logf("  Max latency: %v", time.Duration(writeStats.maxLatency.Load()))
			b.Logf("  Errors: %d", writeStats.errors.Load())

			// Calculate read/write ratio achieved
			totalOps := readStats.totalOps.Load() + writeStats.totalOps.Load()
			readPercent := float64(readStats.totalOps.Load()) * 100 / float64(totalOps)
			b.Logf("\nActual read/write ratio: %.1f%% reads", readPercent)
		})
	}
}

// BenchmarkTargetedRemoval tests the efficiency of context-targeted block removal
// This demonstrates O(m) complexity vs the old O(m × n) approach
func BenchmarkTargetedRemoval(b *testing.B) {
	// Report CPU information
	cpuCount := runtime.NumCPU()
	b.Logf("Running on %d CPU cores", cpuCount)

	configs := []struct {
		name               string
		numContexts        int
		blocksPerContext   int
		blocksToRemove     int
		removeFromContexts int // How many contexts to remove each block from
	}{
		{"10Ctx_1000Blocks_Remove100_From1", 10, 1000, 100, 1},
		{"10Ctx_1000Blocks_Remove100_From5", 10, 1000, 100, 5},
		{"10Ctx_1000Blocks_Remove100_From10", 10, 1000, 100, 10},
		{"100Ctx_1000Blocks_Remove100_From10", 100, 1000, 100, 10},
		{"100Ctx_1000Blocks_Remove100_From50", 100, 1000, 100, 50},
	}

	for _, cfg := range configs {
		b.Run(cfg.name, func(b *testing.B) {
			table := NewSyncPrefixHashTable()
			defer table.Close()

			// Setup: Create blocks in multiple contexts
			fmt.Printf("[%s] Setting up %d contexts with %d blocks each...\n",
				cfg.name, cfg.numContexts, cfg.blocksPerContext)

			// Track which blocks are in which contexts
			blockContextMap := make(map[int64][]ModelContext)

			for ctxID := 0; ctxID < cfg.numContexts; ctxID++ {
				modelName, loraID := getModelLoRAContext(ctxID)
				ctx := ModelContext{ModelName: modelName, LoraID: loraID}

				// Add blocks to this context
				blockHashes := make([]int64, cfg.blocksPerContext)
				tokens := make([][]byte, cfg.blocksPerContext)

				for i := 0; i < cfg.blocksPerContext; i++ {
					blockHash := int64(ctxID*cfg.blocksPerContext + i)
					blockHashes[i] = blockHash
					tokens[i] = generateTokens(16)

					// Track which contexts have this block
					blockContextMap[blockHash] = append(blockContextMap[blockHash], ctx)
				}

				event := BlockStored{
					BlockHashes: blockHashes,
					Tokens:      tokens,
					ModelName:   modelName,
					LoraID:      loraID,
					SourcePod:   fmt.Sprintf("pod-%d", ctxID%stressTestNumPods),
				}

				err := table.ProcessBlockStored(event)
				if err != nil {
					b.Fatalf("Failed to setup context %d: %v", ctxID, err)
				}
			}

			// Select blocks to remove
			blocksToRemove := make([]int64, cfg.blocksToRemove)
			for i := 0; i < cfg.blocksToRemove; i++ {
				blocksToRemove[i] = int64(i * 10) // Spread across contexts
			}

			b.ResetTimer()

			// Benchmark targeted removal
			stats := &performanceStats{}
			totalRemovals := 0

			startTime := time.Now()

			for _, blockHash := range blocksToRemove {
				contexts := blockContextMap[blockHash]
				if len(contexts) == 0 {
					continue
				}

				// Remove from specified number of contexts
				numToRemove := cfg.removeFromContexts
				if numToRemove > len(contexts) {
					numToRemove = len(contexts)
				}

				for i := 0; i < numToRemove; i++ {
					ctx := contexts[i]

					removeEvent := BlockRemoved{
						BlockHashes: []int64{blockHash},
						ModelName:   ctx.ModelName,
						LoraID:      ctx.LoraID,
					}

					opStart := time.Now()
					err := table.ProcessBlockRemoved(removeEvent)
					opDuration := time.Since(opStart)

					if err != nil {
						stats.errors.Add(1)
					} else {
						stats.recordOp(opDuration)
						totalRemovals++
					}
				}
			}

			elapsed := time.Since(startTime)

			// Report results
			b.Logf("\n=== Targeted Removal Results ===")
			b.Logf("Configuration: %d contexts, %d blocks per context", cfg.numContexts, cfg.blocksPerContext)
			b.Logf("Removed %d blocks from %d contexts each", cfg.blocksToRemove, cfg.removeFromContexts)
			b.Logf("Total removal operations: %d", totalRemovals)
			b.Logf("Total time: %v", elapsed)
			b.Logf("Throughput: %.2f removals/sec", float64(totalRemovals)/elapsed.Seconds())
			b.Logf("Avg removal latency: %v", stats.avgLatency())
			b.Logf("Max removal latency: %v", time.Duration(stats.maxLatency.Load()))
			b.Logf("Errors: %d", stats.errors.Load())

			// Calculate theoretical old approach complexity
			oldComplexity := cfg.blocksToRemove * cfg.numContexts // O(m × n)
			improvement := float64(oldComplexity) / float64(totalRemovals)
			b.Logf("\nComplexity improvement: %.2fx (old approach would need %d operations)",
				improvement, oldComplexity)
		})
	}
}
