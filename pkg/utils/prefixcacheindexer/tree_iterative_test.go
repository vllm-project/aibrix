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

package prefixcacheindexer

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// makeSequentialTokens returns []int{start, start+1, ..., start+length-1}
func makeSequentialTokens(start, length int) []int {
	tokens := make([]int, length)
	for i := range tokens {
		tokens[i] = start + i
	}
	return tokens
}

// buildDeepTree creates a tree with the given depth by inserting
// incrementally longer sequences [1], [1,2], [1,2,3], ..., [1,...,depth].
// Each node in the resulting chain stores a single token.
func buildDeepTree(depth int) *LPRadixCache {
	cache := NewLPRadixCache(2)
	for i := 1; i <= depth; i++ {
		tokens := makeSequentialTokens(1, i)
		cache.AddPrefix(tokens, "m1", "p1")
	}
	return cache
}

// Test_InsertHelperEmptyInput verifies that AddPrefix handles empty token input.
func Test_InsertHelperEmptyInput(t *testing.T) {
	cache := NewLPRadixCache(2)
	node, matched, unmatched := cache.AddPrefix([]int{}, "m1", "p1")
	assert.NotNil(t, node)
	assert.Nil(t, matched)
	assert.Nil(t, unmatched)
}

// Test_InsertHelperSingleToken verifies that AddPrefix handles a single token.
func Test_InsertHelperSingleToken(t *testing.T) {
	cache := NewLPRadixCache(2)

	// Insert single token
	node, matched, unmatched := cache.AddPrefix([]int{42}, "m1", "p1")
	assert.NotNil(t, node)
	assert.Nil(t, matched)
	assert.Equal(t, []int{42}, unmatched)

	// Insert same single token again — should be exact match
	node2, matched2, unmatched2 := cache.AddPrefix([]int{42}, "m1", "p1")
	assert.NotNil(t, node2)
	assert.Equal(t, []int{42}, matched2)
	assert.Nil(t, unmatched2)

	// Insert different single token
	node3, matched3, unmatched3 := cache.AddPrefix([]int{99}, "m1", "p1")
	assert.NotNil(t, node3)
	assert.Nil(t, matched3)
	assert.Equal(t, []int{99}, unmatched3)
}

// Test_MatchPrefixEmptyInput verifies that MatchPrefix handles empty token input.
func Test_MatchPrefixEmptyInput(t *testing.T) {
	cache := NewLPRadixCache(2)
	cache.AddPrefix([]int{1, 2, 3}, "m1", "p1")

	pods := []*v1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "p1"}}}
	matchedTokens, unmatchedTokens, _ := cache.MatchPrefix([]int{}, "m1", pods)
	assert.Equal(t, 0, len(matchedTokens))
	assert.Equal(t, 0, len(unmatchedTokens))
}

// Test_MatchPrefixSingleToken verifies that MatchPrefix handles single token input.
func Test_MatchPrefixSingleToken(t *testing.T) {
	cache := NewLPRadixCache(2)
	cache.AddPrefix([]int{42}, "m1", "p1")

	pods := []*v1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "p1"}}}

	// Exact single-token match
	matchedTokens, unmatchedTokens, matchedPods := cache.MatchPrefix([]int{42}, "m1", pods)
	assert.Equal(t, []int{42}, matchedTokens)
	assert.Equal(t, 0, len(unmatchedTokens))
	assert.Equal(t, 1, len(matchedPods))

	// No match
	matchedTokens, unmatchedTokens, _ = cache.MatchPrefix([]int{99}, "m1", pods)
	assert.Equal(t, 0, len(matchedTokens))
	assert.Equal(t, []int{99}, unmatchedTokens)
}

// Test_InsertHelperMultipleSuccessiveSplits verifies correctness when multiple
// splits occur on the same branch at different depths.
func Test_InsertHelperMultipleSuccessiveSplits(t *testing.T) {
	cache := NewLPRadixCache(2)

	// Insert [1,2,3,4,5] — creates one node
	cache.AddPrefix([]int{1, 2, 3, 4, 5}, "m1", "p1")

	// Insert [1,2,X,Y,Z] — forces split at position 2
	node1, matched1, unmatched1 := cache.AddPrefix([]int{1, 2, 100, 101, 102}, "m1", "p1")
	assert.NotNil(t, node1)
	assert.Equal(t, []int{1, 2}, matched1)
	assert.Equal(t, []int{100, 101, 102}, unmatched1)

	// Insert [1,A,B,C,D] — forces split at position 1
	node2, matched2, unmatched2 := cache.AddPrefix([]int{1, 200, 201, 202, 203}, "m1", "p1")
	assert.NotNil(t, node2)
	assert.Equal(t, []int{1}, matched2)
	assert.Equal(t, []int{200, 201, 202, 203}, unmatched2)

	// Verify all three original sequences are still matchable
	pods := []*v1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "p1"}}}

	m1, u1, _ := cache.MatchPrefix([]int{1, 2, 3, 4, 5}, "m1", pods)
	assert.Equal(t, 5, len(m1))
	assert.Equal(t, 0, len(u1))

	m2, u2, _ := cache.MatchPrefix([]int{1, 2, 100, 101, 102}, "m1", pods)
	assert.Equal(t, 5, len(m2))
	assert.Equal(t, 0, len(u2))

	m3, u3, _ := cache.MatchPrefix([]int{1, 200, 201, 202, 203}, "m1", pods)
	assert.Equal(t, 5, len(m3))
	assert.Equal(t, 0, len(u3))
}

// Test_InsertHelperDeepTree verifies that AddPrefix handles deep trees
// without stack overflow. The old recursive implementation would use O(depth)
// stack frames; the iterative version uses O(1).
func Test_InsertHelperDeepTree(t *testing.T) {
	depth := 1000
	cache := buildDeepTree(depth)

	// Insert a sequence that traverses the full depth, then extends beyond
	tokens := makeSequentialTokens(1, depth+10)
	node, matched, unmatched := cache.AddPrefix(tokens, "m1", "p1")

	assert.NotNil(t, node)
	assert.Equal(t, depth, len(matched))
	assert.Equal(t, 10, len(unmatched))
	assert.Equal(t, makeSequentialTokens(1, depth), matched)
	assert.Equal(t, makeSequentialTokens(depth+1, 10), unmatched)
}

// Test_MatchPrefixDeepTree verifies that MatchPrefix handles deep trees
// without stack overflow.
func Test_MatchPrefixDeepTree(t *testing.T) {
	depth := 1000
	cache := buildDeepTree(depth)
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
	}

	// Exact match
	tokens := makeSequentialTokens(1, depth)
	matchedTokens, unmatchedTokens, matchedPods := cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t, depth, len(matchedTokens))
	assert.Equal(t, 0, len(unmatchedTokens))
	assert.Equal(t, 1, len(matchedPods))
	assert.Equal(t, "p1", matchedPods[0].Name)

	// Prefix match (input longer than tree)
	longerTokens := makeSequentialTokens(1, depth+5)
	matchedTokens, unmatchedTokens, _ = cache.MatchPrefix(longerTokens, "m1", pods)
	assert.Equal(t, depth, len(matchedTokens))
	assert.Equal(t, 5, len(unmatchedTokens))
	assert.Equal(t, makeSequentialTokens(depth+1, 5), unmatchedTokens)

	// Partial match (input diverges mid-tree)
	partialTokens := makeSequentialTokens(1, 500)
	partialTokens = append(partialTokens, 99999) // diverge at position 500
	matchedTokens, unmatchedTokens, _ = cache.MatchPrefix(partialTokens, "m1", pods)
	assert.Equal(t, 500, len(matchedTokens))
	assert.Equal(t, 1, len(unmatchedTokens))
}

// Test_InsertHelperSplitDeepTree verifies that insertHelper handles splits
// correctly in a deep tree.
func Test_InsertHelperSplitDeepTree(t *testing.T) {
	cache := NewLPRadixCache(2)

	// Insert a long sequence to create a single deep node
	original := makeSequentialTokens(1, 500)
	cache.AddPrefix(original, "m1", "p1")

	// Insert a diverging sequence that forces a split at position 250
	diverging := make([]int, 300)
	copy(diverging, original[:250])
	for i := 250; i < 300; i++ {
		diverging[i] = 10000 + i
	}
	node, matched, unmatched := cache.AddPrefix(diverging, "m1", "p1")
	assert.NotNil(t, node)
	assert.Equal(t, 250, len(matched))
	assert.Equal(t, 50, len(unmatched))

	// The original sequence should still be fully matchable
	matchNode := cache.GetNode(original)
	assert.NotNil(t, matchNode)
}

// Test_InsertHelperLargeTokenInput simulates the production scenario from
// issue #2049: high-concurrency 30K+ token requests with overlapping prefixes.
func Test_InsertHelperLargeTokenInput(t *testing.T) {
	cache := NewLPRadixCache(4)
	largeSize := 30000

	// Insert a large token sequence
	tokens1 := makeSequentialTokens(0, largeSize)
	node1, _, _ := cache.AddPrefix(tokens1, "m1", "p1")
	assert.NotNil(t, node1)

	// Insert an overlapping sequence that shares 29000 tokens then diverges
	tokens2 := make([]int, largeSize)
	copy(tokens2, tokens1[:29000])
	for i := 29000; i < largeSize; i++ {
		tokens2[i] = largeSize + i // diverge
	}
	node2, matched, unmatched := cache.AddPrefix(tokens2, "m1", "p2")
	assert.NotNil(t, node2)
	assert.Equal(t, 29000, len(matched))
	assert.Equal(t, 1000, len(unmatched))

	// Insert another overlapping sequence (shares 15000 tokens)
	tokens3 := make([]int, largeSize)
	copy(tokens3, tokens1[:15000])
	for i := 15000; i < largeSize; i++ {
		tokens3[i] = 2*largeSize + i
	}
	node3, matched, unmatched := cache.AddPrefix(tokens3, "m1", "p3")
	assert.NotNil(t, node3)
	assert.Equal(t, 15000, len(matched))
	assert.Equal(t, 15000, len(unmatched))

	// Verify original sequence is still fully matchable
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
	}
	matchedTokens, unmatchedTokens, _ := cache.MatchPrefix(tokens1, "m1", pods)
	assert.Equal(t, largeSize, len(matchedTokens))
	assert.Equal(t, 0, len(unmatchedTokens))
}

// Test_ConcurrentDeepTreeAccess simulates concurrent access to a deep tree,
// reproducing the conditions from issue #2049 where multiple goroutines
// traverse deep prefix trees simultaneously.
func Test_ConcurrentDeepTreeAccess(t *testing.T) {
	cache := NewLPRadixCache(4)
	model := "m1"
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p3"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p4"}},
	}

	// Build a moderately deep tree
	depth := 200
	baseTokens := makeSequentialTokens(1, depth)
	cache.AddPrefix(baseTokens, model, "p1")

	// Create depth by inserting incrementally
	for i := 1; i < depth; i++ {
		tokens := makeSequentialTokens(1, i)
		cache.AddPrefix(tokens, model, "p1")
	}

	// Concurrent inserts and reads with large-ish inputs
	var wg sync.WaitGroup
	numGoroutines := 100

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Each goroutine inserts a variation that shares the prefix
			tokens := make([]int, depth+50)
			copy(tokens, baseTokens)
			for j := depth; j < depth+50; j++ {
				tokens[j] = 10000*id + j
			}
			cache.AddPrefix(tokens, model, pods[id%len(pods)].Name)

			// Also do a MatchPrefix
			cache.MatchPrefix(tokens[:depth], model, pods)
		}(i)
	}
	wg.Wait()

	// Verify tree is still consistent
	matchedTokens, _, matchedPods := cache.MatchPrefix(baseTokens, model, pods)
	assert.Equal(t, depth, len(matchedTokens))
	assert.True(t, len(matchedPods) > 0, "Expected matched pods after concurrent operations")
}
