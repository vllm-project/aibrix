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
	"sync"
	"testing"
	"time"
)

// BenchmarkMatchPrefix measures read performance
func BenchmarkMatchPrefix(b *testing.B) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Setup test data
	numContexts := 100
	numPrefixesPerContext := 100
	tokenSize := 1024 // 1KB tokens

	// Pre-populate table
	for i := 0; i < numContexts; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		loraID := int64(i)
		podName := fmt.Sprintf("pod-%d", i)

		tokens := make([]byte, tokenSize)
		_, _ = crand.Read(tokens)
		hashes := table.GetPrefixHashes(tokens)

		// Use all hashes if we have fewer than numPrefixesPerContext
		if len(hashes) < numPrefixesPerContext {
			_ = table.AddPrefix(modelName, loraID, podName, hashes)
		} else {
			_ = table.AddPrefix(modelName, loraID, podName, hashes[:numPrefixesPerContext])
		}
	}

	// Benchmark concurrent reads
	benchmarks := []struct {
		name       string
		numReaders int
	}{
		{"1Reader", 1},
		{"10Readers", 10},
		{"100Readers", 100},
		{"1000Readers", 1000},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			b.ResetTimer()

			var wg sync.WaitGroup
			numOpsPerReader := b.N / bm.numReaders
			if numOpsPerReader == 0 {
				numOpsPerReader = 1
			}

			for i := 0; i < bm.numReaders; i++ {
				wg.Add(1)
				go func(readerID int) {
					defer wg.Done()

					modelName := fmt.Sprintf("model-%d", readerID%numContexts)
					loraID := int64(readerID % numContexts)
					podName := fmt.Sprintf("pod-%d", readerID%numContexts)
					readyPods := map[string]struct{}{podName: {}}

					tokens := make([]byte, tokenSize)
					_, _ = crand.Read(tokens)

					for j := 0; j < numOpsPerReader; j++ {
						table.MatchPrefix(modelName, loraID, tokens, readyPods)
					}
				}(i)
			}

			wg.Wait()
		})
	}
}

// BenchmarkAddPrefix measures write performance
func BenchmarkAddPrefix(b *testing.B) {
	benchmarks := []struct {
		name       string
		numWriters int
	}{
		{"1Writer", 1},
		{"10Writers", 10},
		{"100Writers", 100},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			table := NewSyncPrefixHashTable()
			defer table.Close()

			b.ResetTimer()

			var wg sync.WaitGroup
			numOpsPerWriter := b.N / bm.numWriters
			if numOpsPerWriter == 0 {
				numOpsPerWriter = 1
			}

			for i := 0; i < bm.numWriters; i++ {
				wg.Add(1)
				go func(writerID int) {
					defer wg.Done()

					for j := 0; j < numOpsPerWriter; j++ {
						modelName := fmt.Sprintf("model-%d", writerID)
						loraID := int64(writerID)
						podName := fmt.Sprintf("pod-%d-%d", writerID, j)

						tokens := make([]byte, 1024)
						_, _ = crand.Read(tokens)
						hashes := table.GetPrefixHashes(tokens)

						_ = table.AddPrefix(modelName, loraID, podName, hashes)
					}
				}(i)
			}

			wg.Wait()
		})
	}
}

// BenchmarkMixedOperations measures mixed read/write performance
func BenchmarkMixedOperations(b *testing.B) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	// Pre-populate with some data
	for i := 0; i < 50; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		loraID := int64(i)
		podName := fmt.Sprintf("pod-%d", i)

		tokens := make([]byte, 1024)
		_, _ = crand.Read(tokens)
		hashes := table.GetPrefixHashes(tokens)

		_ = table.AddPrefix(modelName, loraID, podName, hashes)
	}

	b.ResetTimer()

	var wg sync.WaitGroup
	numWorkers := 100
	numOpsPerWorker := b.N / numWorkers
	if numOpsPerWorker == 0 {
		numOpsPerWorker = 1
	}

	// 80% reads, 20% writes
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for j := 0; j < numOpsPerWorker; j++ {
				if rand.Float32() < 0.8 {
					// Read operation
					modelName := fmt.Sprintf("model-%d", rand.Intn(50))
					loraID := int64(rand.Intn(50))
					podName := fmt.Sprintf("pod-%d", rand.Intn(50))
					readyPods := map[string]struct{}{podName: {}}

					tokens := make([]byte, 1024)
					_, _ = crand.Read(tokens)

					table.MatchPrefix(modelName, loraID, tokens, readyPods)
				} else {
					// Write operation
					modelName := fmt.Sprintf("model-%d", workerID)
					loraID := int64(workerID)
					podName := fmt.Sprintf("pod-%d-%d", workerID, j)

					tokens := make([]byte, 1024)
					_, _ = crand.Read(tokens)
					hashes := table.GetPrefixHashes(tokens)

					_ = table.AddPrefix(modelName, loraID, podName, hashes)
				}
			}
		}(i)
	}

	wg.Wait()
}

// BenchmarkProcessBlockStored measures event processing performance
func BenchmarkProcessBlockStored(b *testing.B) {
	benchmarks := []struct {
		name           string
		numProcessors  int
		blocksPerEvent int
	}{
		{"1Proc_10Blocks", 1, 10},
		{"10Proc_10Blocks", 10, 10},
		{"100Proc_10Blocks", 100, 10},
		{"10Proc_100Blocks", 10, 100},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			table := NewSyncPrefixHashTable()
			defer table.Close()

			b.ResetTimer()

			var wg sync.WaitGroup
			numOpsPerProcessor := b.N / bm.numProcessors
			if numOpsPerProcessor == 0 {
				numOpsPerProcessor = 1
			}

			for i := 0; i < bm.numProcessors; i++ {
				wg.Add(1)
				go func(procID int) {
					defer wg.Done()

					for j := 0; j < numOpsPerProcessor; j++ {
						modelName := fmt.Sprintf("model-%d", procID)
						loraID := int64(procID)
						sourcePod := fmt.Sprintf("pod-%d", procID)

						// Create event with multiple blocks
						blockHashes := make([]int64, bm.blocksPerEvent)
						tokens := make([][]byte, bm.blocksPerEvent)

						for k := 0; k < bm.blocksPerEvent; k++ {
							blockHashes[k] = int64(procID*1000000 + j*1000 + k)
							blockTokens := make([]byte, prefixCacheBlockSize)
							_, _ = crand.Read(blockTokens)
							tokens[k] = blockTokens
						}

						event := BlockStored{
							BlockHashes: blockHashes,
							Tokens:      tokens,
							ModelName:   modelName,
							LoraID:      loraID,
							SourcePod:   sourcePod,
						}

						_ = table.ProcessBlockStored(event)
					}
				}(i)
			}

			wg.Wait()
		})
	}
}

// BenchmarkGetPrefixHashes measures hash computation performance
func BenchmarkGetPrefixHashes(b *testing.B) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	sizes := []int{
		100,    // 100 bytes
		1024,   // 1 KB
		10240,  // 10 KB
		102400, // 100 KB
	}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%d", size), func(b *testing.B) {
			tokens := make([]byte, size)
			_, _ = crand.Read(tokens)

			b.ResetTimer()
			b.SetBytes(int64(size))

			for i := 0; i < b.N; i++ {
				_ = table.GetPrefixHashes(tokens)
			}
		})
	}
}

// BenchmarkContextCreation measures context creation overhead
func BenchmarkContextCreation(b *testing.B) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		loraID := int64(i)
		podName := fmt.Sprintf("pod-%d", i)

		tokens := []byte{1, 2, 3, 4}
		hashes := table.GetPrefixHashes(tokens)

		_ = table.AddPrefix(modelName, loraID, podName, hashes)
	}
}

// BenchmarkEviction measures eviction performance
func BenchmarkEviction(b *testing.B) {
	// Create table with custom eviction settings
	table := &SyncPrefixHashTable{
		seed:                  12345,
		maxContexts:           maxContexts,
		maxPrefixesPerContext: maxPrefixesPerContext,
		blockSize:             prefixCacheBlockSize,
		evictionInterval:      1 * time.Hour, // Disable automatic eviction
		evictionDuration:      1 * time.Minute,
		stopCh:                make(chan struct{}),
	}

	// Start eviction worker
	table.wg.Add(1)
	go table.evictionWorker()
	defer table.Close()

	// Pre-populate with many contexts
	numContexts := 1000
	for i := 0; i < numContexts; i++ {
		modelName := fmt.Sprintf("model-%d", i)
		loraID := int64(i)
		podName := fmt.Sprintf("pod-%d", i)

		tokens := make([]byte, 1024)
		_, _ = crand.Read(tokens)
		hashes := table.GetPrefixHashes(tokens)

		_ = table.AddPrefix(modelName, loraID, podName, hashes)
	}

	// Make half of them expired
	halfContexts := numContexts / 2
	table.contextMap.Range(func(key, value interface{}) bool {
		if halfContexts <= 0 {
			return false
		}
		contextData := value.(*ContextData)
		contextData.prefixStore.lastAccess.Store(time.Now().Add(-2 * time.Minute).Unix())
		halfContexts--
		return true
	})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		table.performEviction()
	}
}
