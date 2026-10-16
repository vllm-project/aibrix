/*
Copyright 2026 The Aibrix Team.

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
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// No background worker: the tests drive the production eviction methods.
func newEvictionStoreTestTable() *SyncPrefixHashTable {
	return &SyncPrefixHashTable{
		seed:                  12345,
		maxContexts:           2,
		maxPrefixesPerContext: maxPrefixesPerContext,
		blockSize:             16,
		evictionInterval:      time.Hour,
		evictionDuration:      20 * time.Minute,
		stopCh:                make(chan struct{}),
		blockIndex:            make(map[int64][]ModelContext),
	}
}

func TestBlockStoredAfterContextIndexCleanup(t *testing.T) {
	for _, loraName := range []string{"", "adapter"} {
		t.Run(fmt.Sprintf("adapter=%s", loraName), func(t *testing.T) {
			table := newEvictionStoreTestTable()
			defer table.Close()
			ctx := ModelContext{ModelName: testModelName, LoraID: LoraIDFromName(loraName)}
			event := BlockStored{
				BlockHashes: []int64{101}, Tokens: [][]byte{makeTokens(16)},
				ModelName: ctx.ModelName, LoraID: -1, LoraName: loraName, SourcePod: testPod1Name,
			}
			if err := table.ProcessBlockStored(event); err != nil {
				t.Fatal(err)
			}
			value, _ := table.contextMap.Load(ctx)
			retired := value.(*ContextData)

			// Both eviction paths mark, clean the reverse index, then delete
			// the context. Deliver an event in that last window without sleeps.
			retired.markedForEviction.Store(true)
			table.dropContextBlockIndex(ctx, retired)
			event.BlockHashes = []int64{102}
			if err := table.ProcessBlockStored(event); err != nil {
				t.Fatal(err)
			}
			table.contextMap.Delete(ctx)
			table.contextCount.Add(-1)
			if len(table.blockIndex) != 0 {
				t.Errorf("evicted context left reverse-index entries: %v", table.blockIndex)
			}

			// The next event must be able to rebuild a usable context.
			if err := table.ProcessBlockStored(event); err != nil {
				t.Fatal(err)
			}
			matches, _ := table.MatchPrefix(ctx.ModelName, ctx.LoraID, event.Tokens[0], map[string]struct{}{testPod1Name: {}})
			if matches[testPod1Name] != 100 {
				t.Errorf("fresh context should match the rebuilt block: %v", matches)
			}
			if err := table.ProcessBlockRemoved(BlockRemoved{
				BlockHashes: event.BlockHashes, ModelName: ctx.ModelName,
				LoraID: -1, SourcePod: testPod1Name,
			}); err != nil {
				t.Fatal(err)
			}
			if len(table.blockIndex) != 0 {
				t.Errorf("removing the rebuilt block should clear its reverse index: %v", table.blockIndex)
			}
		})
	}
}

func TestConcurrentContextEvictionLeavesNoOrphanedBlocks(t *testing.T) {
	table := newEvictionStoreTestTable()
	defer table.Close()
	var writers sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 4; worker++ {
		writers.Add(1)
		go func(worker int) {
			defer writers.Done()
			<-start
			for i := 0; i < 500; i++ {
				if err := table.ProcessBlockStored(BlockStored{
					BlockHashes: []int64{int64(worker*500 + i)},
					Tokens:      [][]byte{makeTokens(16)}, ModelName: fmt.Sprintf("model-%d", worker),
					LoraID: -1, SourcePod: testPod1Name,
				}); err != nil {
					t.Error(err)
					return
				}
				runtime.Gosched()
			}
		}(worker)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for i := 0; i < 500; i++ {
			table.performEviction()
			runtime.Gosched()
		}
	}()
	close(start)
	writers.Wait()
	<-done
	// No writers remain. Retire all surviving contexts to expose any
	// reverse entries that escaped an earlier concurrent eviction.
	table.enforceContextLimit(int(table.contextCount.Load()))
	if len(table.blockIndex) != 0 {
		t.Errorf("all contexts were evicted, but %d block hashes remain indexed", len(table.blockIndex))
	}
}
