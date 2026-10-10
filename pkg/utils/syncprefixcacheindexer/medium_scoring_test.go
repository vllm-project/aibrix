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
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMediumWeight verifies the tier weights used to score non-GPU prefixes.
func TestMediumWeight(t *testing.T) {
	assert.Equal(t, 1.0, mediumWeight(MediumGPU))
	assert.Equal(t, 0.5, mediumWeight(MediumCPU))
	assert.Equal(t, 0.25, mediumWeight(MediumStorage))
	// Unspecified tier keeps full strength for backward compatibility.
	assert.Equal(t, 1.0, mediumWeight(""))
	assert.Equal(t, 1.0, mediumWeight("unknown-tier"))
}

// TestMatchPrefixMediumWeighting verifies that pods holding a prefix only on a
// slower tier are scored lower than pods holding it on GPU, while pods with an
// unspecified tier keep the legacy full score.
func TestMatchPrefixMediumWeighting(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	model := "medium-test-model"
	tokens := make([]byte, 32) // 2 blocks of default block size 16
	for i := range tokens {
		tokens[i] = byte(i)
	}
	hashes := table.GetPrefixHashes(tokens)
	require.Len(t, hashes, 2)

	readyPods := map[string]struct{}{"p-gpu": {}, "p-cpu": {}, "p-storage": {}, "p-unspecified": {}}

	require.NoError(t, table.AddPrefixWithMedium(model, -1, "p-gpu", MediumGPU, hashes))
	require.NoError(t, table.AddPrefixWithMedium(model, -1, "p-cpu", MediumCPU, hashes))
	require.NoError(t, table.AddPrefixWithMedium(model, -1, "p-storage", MediumStorage, hashes))
	require.NoError(t, table.AddPrefixWithMedium(model, -1, "p-unspecified", "", hashes))

	matched, _ := table.MatchPrefix(model, -1, tokens, readyPods)

	assert.Equal(t, 100, matched["p-gpu"], "GPU tier must keep full score")
	assert.Equal(t, 50, matched["p-cpu"], "CPU tier must be scored lower")
	assert.Equal(t, 25, matched["p-storage"], "STORAGE tier must be scored lowest")
	assert.Equal(t, 100, matched["p-unspecified"], "unspecified tier keeps legacy full score")
}

// TestProcessBlockStoredMediumScoring verifies the event ingestion path records
// the tier carried by the BlockStored event and that MatchPrefix weights it.
// vLLM emits one BlockStored event per block, chained via ParentBlockHash.
func TestProcessBlockStoredMediumScoring(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	model := "event-medium-model"
	block1 := make([]byte, 16)
	block2 := make([]byte, 16)
	for i := range block2 {
		block2[i] = byte(16 + i)
	}

	storeOnPod := func(pod, medium string) {
		// Block 1: parent is the engine-side NONE (nil).
		err := table.ProcessBlockStored(BlockStored{
			BlockHashes: []int64{9001},
			Tokens:      [][]byte{block1},
			ModelName:   model,
			LoraID:      -1,
			SourcePod:   pod,
			Medium:      medium,
		})
		require.NoError(t, err)
		// Block 2: parent is block 1's engine hash.
		parent := int64(9001)
		err = table.ProcessBlockStored(BlockStored{
			BlockHashes:     []int64{9002},
			ParentBlockHash: &parent,
			Tokens:          [][]byte{block2},
			ModelName:       model,
			LoraID:          -1,
			SourcePod:       pod,
			Medium:          medium,
		})
		require.NoError(t, err)
	}

	storeOnPod("p-gpu", MediumGPU)
	storeOnPod("p-cpu", MediumCPU)

	tokens := append(append([]byte{}, block1...), block2...)
	readyPods := map[string]struct{}{"p-gpu": {}, "p-cpu": {}}
	matched, _ := table.MatchPrefix(model, -1, tokens, readyPods)

	assert.Equal(t, 100, matched["p-gpu"], "GPU tier must keep full score")
	assert.Equal(t, 50, matched["p-cpu"], "CPU tier must be scored lower")
}

// TestAddPrefixWithoutMediumKeepsLegacyBehavior covers the plain AddPrefix path
// (no tier information): entries score at full strength.
func TestAddPrefixWithoutMediumKeepsLegacyBehavior(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	model := "legacy-medium-model"
	tokens := make([]byte, 16)
	hashes := table.GetPrefixHashes(tokens)
	require.Len(t, hashes, 1)

	require.NoError(t, table.AddPrefix(model, -1, "p1", hashes))

	matched, _ := table.MatchPrefix(model, -1, tokens, map[string]struct{}{"p1": {}})
	assert.Equal(t, 100, matched["p1"])
}

const tierTestModel = "tier-test-model"

// tierTestBlock returns the tokens of block i, one default-size block
func tierTestBlock(i int) []byte {
	return bytes.Repeat([]byte{byte(i + 1)}, defaultPrefixCacheBlockSize)
}

// storeTierBlock processes the BlockStored event vLLM publishes for block i
// of a chained prompt, with engine block hash 1000+i
func storeTierBlock(t *testing.T, table *SyncPrefixHashTable, pod, medium string, group int64, i int) {
	t.Helper()
	event := BlockStored{
		BlockHashes: []int64{int64(1000 + i)},
		Tokens:      [][]byte{tierTestBlock(i)},
		ModelName:   tierTestModel,
		LoraID:      -1,
		SourcePod:   pod,
		Medium:      medium,
		GroupIdx:    group,
	}
	if i > 0 {
		parent := int64(1000 + i - 1)
		event.ParentBlockHash = &parent
	}
	require.NoError(t, table.ProcessBlockStored(event))
}

func removeTierBlock(t *testing.T, table *SyncPrefixHashTable, pod, medium string, group int64, i int) {
	t.Helper()
	require.NoError(t, table.ProcessBlockRemoved(BlockRemoved{
		BlockHashes: []int64{int64(1000 + i)},
		ModelName:   tierTestModel,
		LoraID:      -1,
		SourcePod:   pod,
		Medium:      medium,
		GroupIdx:    group,
	}))
}

// matchTierBlocks matches a prompt of the first n test blocks on p1 and p2
func matchTierBlocks(table *SyncPrefixHashTable, n int) map[string]int {
	var tokens []byte
	for i := 0; i < n; i++ {
		tokens = append(tokens, tierTestBlock(i)...)
	}
	matched, _ := table.MatchPrefix(tierTestModel, -1, tokens, map[string]struct{}{"p1": {}, "p2": {}})
	return matched
}

// TestTierPresenceFollowsOffload replays the events vLLM publishes for a block
// with CPU offloading: the block is copied to CPU while the GPU copy is still
// cached, then each copy is evicted on its own.
func TestTierPresenceFollowsOffload(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	storeTierBlock(t, table, "p1", MediumGPU, 0, 0)
	assert.Equal(t, 100, matchTierBlocks(table, 1)["p1"])

	storeTierBlock(t, table, "p1", MediumCPU, -1, 0)
	assert.Equal(t, 100, matchTierBlocks(table, 1)["p1"], "the GPU copy is still cached")

	removeTierBlock(t, table, "p1", MediumGPU, 0, 0)
	assert.Equal(t, 50, matchTierBlocks(table, 1)["p1"], "only the CPU copy is left")

	removeTierBlock(t, table, "p1", MediumCPU, -1, 0)
	assert.NotContains(t, matchTierBlocks(table, 1), "p1")
}

// TestTierPresencePromotesToGPU tests that a block first reported on CPU
// scores fully once it is also on GPU, and that removing the CPU copy keeps it
func TestTierPresencePromotesToGPU(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	storeTierBlock(t, table, "p1", MediumCPU, -1, 0)
	assert.Equal(t, 50, matchTierBlocks(table, 1)["p1"])

	storeTierBlock(t, table, "p1", MediumGPU, 0, 0)
	assert.Equal(t, 100, matchTierBlocks(table, 1)["p1"])

	removeTierBlock(t, table, "p1", MediumCPU, -1, 0)
	assert.Equal(t, 100, matchTierBlocks(table, 1)["p1"], "the GPU copy is still cached")
}

// TestTierPresenceScopedToGroup tests that a hybrid-attention model, which
// publishes events per KV-cache group, keeps the prefix until the last group
// holding it evicts it
func TestTierPresenceScopedToGroup(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	storeTierBlock(t, table, "p1", MediumGPU, 0, 0)
	storeTierBlock(t, table, "p1", MediumGPU, 1, 0)

	removeTierBlock(t, table, "p1", MediumGPU, 1, 0)
	assert.Equal(t, 100, matchTierBlocks(table, 1)["p1"], "group 0 still holds the block")

	removeTierBlock(t, table, "p1", MediumGPU, 0, 0)
	assert.NotContains(t, matchTierBlocks(table, 1), "p1")
}

// TestMatchPrefixSumsBlockWeights tests that each matched block counts with
// the weight of the fastest tier holding it
func TestMatchPrefixSumsBlockWeights(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	// p1: blocks 0-1 on GPU and CPU, blocks 2-3 only on CPU. p2: all on GPU.
	for i := 0; i < 4; i++ {
		storeTierBlock(t, table, "p1", MediumCPU, -1, i)
		storeTierBlock(t, table, "p2", MediumGPU, 0, i)
	}
	storeTierBlock(t, table, "p1", MediumGPU, 0, 0)
	storeTierBlock(t, table, "p1", MediumGPU, 0, 1)

	matched := matchTierBlocks(table, 4)
	assert.Equal(t, 75, matched["p1"], "(1 + 1 + 0.5 + 0.5) / 4")
	assert.Equal(t, 100, matched["p2"])
}

// TestMatchPrefixStopsAtFirstMissingBlock tests that a pod is not credited
// for blocks after one it lacks, since the engine can only reuse a prefix
func TestMatchPrefixStopsAtFirstMissingBlock(t *testing.T) {
	table := NewSyncPrefixHashTable()
	defer table.Close()

	for i := 0; i < 3; i++ {
		storeTierBlock(t, table, "p1", MediumGPU, 0, i)
	}
	storeTierBlock(t, table, "p2", MediumGPU, 0, 0)
	storeTierBlock(t, table, "p2", MediumGPU, 0, 2)

	matched := matchTierBlocks(table, 3)
	assert.Equal(t, 100, matched["p1"])
	assert.Equal(t, 33, matched["p2"], "only block 0 counts")
}

// TestPodInfoWeight tests the weight of tier combinations
func TestPodInfoWeight(t *testing.T) {
	tests := []struct {
		name  string
		tiers uint64
		want  float64
	}{
		{"none", 0, 0},
		{"GPU", tierBit(MediumGPU, 0), 1.0},
		{"unspecified medium", tierBit("", -1), 1.0},
		{"unknown medium", tierBit("NVME", 0), 1.0},
		{"CPU", tierBit(MediumCPU, 0), 0.5},
		{"CPU and STORAGE", tierBit(MediumCPU, 0) | tierBit(MediumStorage, 0), 0.5},
		{"STORAGE in group 3", tierBit(MediumStorage, 3), 0.25},
		{"GPU in a group past the tracked ones", tierBit(MediumGPU, maxTrackedGroups+2), 1.0},
		{"CPU in group 1, GPU in group 2", tierBit(MediumCPU, 1) | tierBit(MediumGPU, 2), 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PodInfo{tiers: tt.tiers}
			assert.Equal(t, tt.want, p.weight())
		})
	}
}
