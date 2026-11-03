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
	"testing"
	"time"
)

// Routing can refresh an idle engine block without another BlockStored event.
func TestExpiredPodRouterRefreshStillAcceptsBlockRemoved(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "refresh_without_removal"
		if remove {
			name = "refresh_then_removal"
		}
		t.Run(name, func(t *testing.T) {
			table := newEvictionStoreTestTable()
			defer table.Close()
			tokens := makeTokens(table.blockSize)
			ctx := ModelContext{ModelName: testModelName, LoraID: -1}
			if err := table.ProcessBlockStored(BlockStored{BlockHashes: []int64{701}, Tokens: [][]byte{tokens}, ModelName: ctx.ModelName, LoraID: ctx.LoraID, SourcePod: testPod1Name}); err != nil {
				t.Fatal(err)
			}
			value, _ := table.contextMap.Load(ctx)
			data := value.(*ContextData)
			data.prefixMu.Lock()
			for _, pods := range data.prefixStore.prefixMap {
				pods[testPod1Name].LastAccessTime.Store(time.Now().Add(-2 * table.evictionDuration).Unix())
			}
			data.prefixMu.Unlock()
			// Keep the context active: exercise Phase 3, not whole-context expiry.
			table.performEviction()
			matches, hashes := table.MatchPrefix(ctx.ModelName, ctx.LoraID, tokens, map[string]struct{}{testPod1Name: {}})
			if len(matches) != 0 {
				t.Fatalf("expired pod still matches: %v", matches)
			}
			// This is the same public API used by the router after dispatch.
			if err := table.AddPrefix(ctx.ModelName, ctx.LoraID, testPod1Name, hashes); err != nil {
				t.Fatal(err)
			}
			if remove {
				if err := table.ProcessBlockRemoved(BlockRemoved{BlockHashes: []int64{701}, ModelName: ctx.ModelName, SourcePod: testPod1Name}); err != nil {
					t.Fatal(err)
				}
			}
			matches, _ = table.MatchPrefix(ctx.ModelName, ctx.LoraID, tokens, map[string]struct{}{testPod1Name: {}})
			if remove && len(matches) != 0 {
				t.Fatalf("late engine removal left router-refreshed prefix: %v", matches)
			}
			if !remove && matches[testPod1Name] != 100 {
				t.Fatalf("refresh should remain until removal: %v", matches)
			}
			if remove && len(table.blockIndex) != 0 {
				t.Fatalf("confirmed removal left index: %v", table.blockIndex)
			}
		})
	}
}

func TestRemovePrefixPreservesSharedPodMapping(t *testing.T) {
	table := newEvictionStoreTestTable()
	defer table.Close()
	tokens := makeTokens(table.blockSize)
	for _, pod := range []string{testPod1Name, "pod2"} {
		if err := table.ProcessBlockStored(BlockStored{BlockHashes: []int64{702}, Tokens: [][]byte{tokens}, ModelName: testModelName, LoraID: -1, SourcePod: pod}); err != nil {
			t.Fatal(err)
		}
	}
	if err := table.RemovePrefix(testModelName, -1, testPod1Name); err != nil {
		t.Fatal(err)
	}
	matches, _ := table.MatchPrefix(testModelName, -1, tokens, map[string]struct{}{testPod1Name: {}, "pod2": {}})
	if len(matches) != 1 || matches["pod2"] != 100 {
		t.Fatalf("wrong remaining holder: %v", matches)
	}
	if len(table.blockIndex[702]) != 1 {
		t.Fatalf("shared block index lost: %v", table.blockIndex)
	}
	if err := table.ProcessBlockRemoved(BlockRemoved{BlockHashes: []int64{702}, ModelName: testModelName, SourcePod: "pod2"}); err != nil {
		t.Fatal(err)
	}
	matches, _ = table.MatchPrefix(testModelName, -1, tokens, map[string]struct{}{"pod2": {}})
	if len(matches) != 0 || len(table.blockIndex) != 0 {
		t.Fatalf("last holder removal failed: matches=%v index=%v", matches, table.blockIndex)
	}
}

func TestRemovePrefixPreservesOtherAdapterBlockIndex(t *testing.T) {
	table := newEvictionStoreTestTable()
	defer table.Close()
	tokens := makeTokens(table.blockSize)
	for i, adapter := range []string{"adapter-a", "adapter-b"} {
		pod := []string{testPod1Name, "pod2"}[i]
		if err := table.ProcessBlockStored(BlockStored{BlockHashes: []int64{703}, Tokens: [][]byte{tokens}, ModelName: testModelName, LoraName: adapter, SourcePod: pod}); err != nil {
			t.Fatal(err)
		}
	}
	if err := table.RemovePrefix(testModelName, -1, testPod1Name); err != nil {
		t.Fatal(err)
	}
	remaining := ModelContext{ModelName: testModelName, LoraID: LoraIDFromName("adapter-b")}
	if contexts := table.blockIndex[703]; len(contexts) != 1 || contexts[0] != remaining {
		t.Fatalf("other adapter index damaged: %v", contexts)
	}
	matches, _ := table.MatchPrefix(testModelName, remaining.LoraID, tokens, map[string]struct{}{"pod2": {}})
	if matches["pod2"] != 100 {
		t.Fatalf("other adapter no longer matches: %v", matches)
	}
	if err := table.ProcessBlockRemoved(BlockRemoved{BlockHashes: []int64{703}, ModelName: testModelName, SourcePod: "pod2"}); err != nil {
		t.Fatal(err)
	}
	matches, _ = table.MatchPrefix(testModelName, remaining.LoraID, tokens, map[string]struct{}{"pod2": {}})
	if len(matches) != 0 || len(table.blockIndex) != 0 {
		t.Fatalf("other adapter removal failed: matches=%v index=%v", matches, table.blockIndex)
	}
}

func TestExpiredPodMappingsRetireWithContext(t *testing.T) {
	table := newEvictionStoreTestTable()
	defer table.Close()
	ctx := ModelContext{ModelName: testModelName, LoraID: -1}
	if err := table.ProcessBlockStored(BlockStored{BlockHashes: []int64{704}, Tokens: [][]byte{makeTokens(table.blockSize)}, ModelName: ctx.ModelName, LoraID: ctx.LoraID, SourcePod: testPod1Name}); err != nil {
		t.Fatal(err)
	}
	value, _ := table.contextMap.Load(ctx)
	data := value.(*ContextData)
	data.prefixMu.Lock()
	for _, pods := range data.prefixStore.prefixMap {
		pods[testPod1Name].LastAccessTime.Store(time.Now().Add(-2 * table.evictionDuration).Unix())
	}
	data.prefixMu.Unlock()
	table.performEviction()
	if len(table.blockIndex[704]) != 1 {
		t.Fatal("idle pod must retain the engine lookup until confirmed removal or context retirement")
	}
	data.prefixStore.lastAccess.Store(time.Now().Add(-2 * table.evictionDuration).Unix())
	table.performEviction()
	if _, exists := table.contextMap.Load(ctx); exists {
		t.Fatal("idle context was not retired")
	}
	if len(table.blockIndex) != 0 {
		t.Fatalf("retired context retained reverse index: %v", table.blockIndex)
	}
}
