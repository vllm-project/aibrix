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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_LPRadixCacheE2E(t *testing.T) {
	t.SkipNow()
	cache := NewLPRadixCache(2) // assuming 2 GPUs
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
	}

	inputText := "Hello World! What a Good Day! Good Morning! 你好世界！多么美好的一天啊！早上好！"
	tokens, err := utils.TokenizeInputText(inputText)
	assert.Equal(t, nil, err)

	// Initial match should return empty as cache is empty
	matchedTokens, unMatchedTokens, matchPods := cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t, 0, len(matchedTokens))
	assert.Equal(t,
		[]int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 7839, 29084, 0, 220, 57668, 53901, 3574, 244, 98220, 6447, 43240, 82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447, 6079, 102, 17905, 53901, 6447},
		unMatchedTokens)
	assert.Equal(t, 0, len(matchPods))

	// Add prefix and verify it can be matched
	cache.AddPrefix(unMatchedTokens, "m1", "p1")
	matchedTokens, unMatchedTokens, matchPods = cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t,
		[]int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 7839, 29084, 0, 220, 57668, 53901, 3574, 244, 98220, 6447, 43240, 82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447, 6079, 102, 17905, 53901, 6447},
		matchedTokens)
	assert.Equal(t, 0, len(unMatchedTokens))
	assert.Equal(t, "p1", matchPods[0].Name)

	// Test eviction
	cache.Evict(time.Now().Add(60 * time.Minute))
	_, unMatchedTokens, matchPods = cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t,
		[]int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 7839, 29084, 0, 220, 57668, 53901, 3574, 244, 98220, 6447, 43240, 82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447, 6079, 102, 17905, 53901, 6447},
		unMatchedTokens)
	assert.Equal(t, 0, len(matchPods))
}

func Test_RadixMatchPrefix(t *testing.T) {
	t.SkipNow()
	tests := []*struct {
		name          string
		inputText     string
		setupCache    func() *LPRadixCache
		model         string
		pods          []*v1.Pod
		matchTokens   []int
		unMatchTokens []int
		matchPods     []*v1.Pod
	}{
		{
			name:      "empty cache - no match",
			inputText: "Hello World! What a Good Day! 你好世界！多么美好的一天啊！",
			setupCache: func() *LPRadixCache {
				return NewLPRadixCache(2)
			},
			model: "m1",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
			},
			matchTokens:   []int{},
			unMatchTokens: []int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 220, 57668, 53901, 3574, 244, 98220, 6447, 43240, 82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447},
			matchPods:     nil,
		},
		{
			name:      "partial match - one node exists",
			inputText: "Hello World! What a Good Day! 你好世界！多么美好的一天啊！",
			setupCache: func() *LPRadixCache {
				cache := NewLPRadixCache(2)
				tokens := []int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 220}
				cache.AddPrefix(tokens, "m1", "p1")
				return cache
			},
			model: "m1",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
			},
			matchTokens:   []int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 220},
			unMatchTokens: []int{57668, 53901, 3574, 244, 98220, 6447, 43240, 82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447},
			matchPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, err := utils.TokenizeInputText(tt.inputText)
			assert.Equal(t, nil, err)

			cache := tt.setupCache()
			matchTokens, unMatchTokens, matchPods := cache.MatchPrefix(tokens, tt.model, tt.pods)

			assert.Equal(t, tt.matchTokens, matchTokens)
			assert.Equal(t, tt.unMatchTokens, unMatchTokens)
			assert.Equal(t, tt.matchPods, matchPods)
		})
	}
}

func Test_LPRadixCacheConcurrency(t *testing.T) {
	cache := NewLPRadixCache(2) // assuming 2 GPUs
	model := "m1"
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p3"}},
	}

	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Generate different tokens for each goroutine
			inputTokens := []int{1, 2, 3, i % 10, 5, 6}

			// Match prefix and add to cache
			_, unMatchedTokens, _ := cache.MatchPrefix(inputTokens, model, pods)
			if len(unMatchedTokens) > 0 {
				cache.AddPrefix(unMatchedTokens, model, "p1")
			}
		}(i)
	}
	wg.Wait()

	// Verify the cache has been populated by checking a specific token sequence
	testTokens := []int{1, 2, 3, 5, 5, 6}
	matchedTokens, _, matchPods := cache.MatchPrefix(testTokens, model, pods)

	// We should have at least a partial match after adding 1000 patterns
	assert.True(t, len(matchedTokens) > 0, "Expected to find matches in cache after concurrent population")

	// One of our 1000 patterns should have matched this test pattern completely
	foundMatchingPod := false
	for _, pod := range matchPods {
		if pod.Name == "p1" {
			foundMatchingPod = true
			break
		}
	}
	assert.True(t, foundMatchingPod, "Expected to find pod p1 in matched pods")
}
