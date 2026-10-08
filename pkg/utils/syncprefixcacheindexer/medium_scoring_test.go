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
