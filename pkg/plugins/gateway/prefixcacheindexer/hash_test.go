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
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_PrefixHashTableE2E(t *testing.T) {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	seed := r.Uint64()
	cache := PrefixHashTable{
		blocks: map[uint64]Block{},
		hash:   xxhash.NewWithSeed(seed),
		seed:   seed,
	}
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
	}

	inputText := "Hello World! What a Good Day! Good Morning! 你好世界！多么美好的一天啊！早上好！"
	tokens, err := utils.TokenizeInputText(inputText)
	assert.Equal(t, nil, err)

	matchedTokens, unMatchedTokens, matchPods := cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t, 0, len(matchedTokens))
	assert.Equal(t,
		[]int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 7839, 29084, 0, 220, 57668, 53901, 3574, 244, 98220, 6447, 43240, 82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447, 6079, 102, 17905, 53901, 6447},
		unMatchedTokens)
	assert.Equal(t, 0, len(matchPods))

	cache.AddPrefix(unMatchedTokens, "m1", "p1")
	matchedTokens, unMatchedTokens, matchPods = cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t,
		[]int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 7839, 29084, 0, 220, 57668, 53901, 3574, 244, 98220, 6447, 43240, 82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447, 6079, 102, 17905, 53901, 6447},
		matchedTokens)
	assert.Equal(t, 0, len(unMatchedTokens))
	assert.Equal(t, "p1", matchPods[0].Name)

	cache.Evict(time.Now().Add(60 * time.Minute))
	_, unMatchedTokens, matchPods = cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t,
		[]int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 7839, 29084, 0, 220, 57668, 53901, 3574, 244, 98220, 6447, 43240, 82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447, 6079, 102, 17905, 53901, 6447},
		unMatchedTokens)
	assert.Equal(t, 0, len(matchPods))
}

func Test_MatchPrefix(t *testing.T) {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	seed := r.Uint64()
	tests := []*struct {
		name          string
		inputText     string
		cache         PrefixHashTable
		model         string
		pods          []*v1.Pod
		matchTokens   []int
		unMatchTokens []int
		matchPods     []*v1.Pod
	}{
		{
			name:      "token length more than prefix block size, no prefix blocks exist in the cache",
			inputText: "Hello World! What a Good Day! 你好世界！多么美好的一天啊！",
			cache: PrefixHashTable{
				blocks: map[uint64]Block{},
				hash:   xxhash.NewWithSeed(seed),
				seed:   seed,
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
			name:      "token length more than prefix block size, one prefix block exist in the cache",
			inputText: "Hello World! What a Good Day! 你好世界！多么美好的一天啊！",
			cache: PrefixHashTable{
				blocks: map[uint64]Block{
					8954089069687757318: {
						modelToPods: map[string]map[string]time.Time{
							"m1": {
								"p1": time.Now(),
							},
						},
						lastAccessTime: time.Now(),
					},
				},
				hash: xxhash.NewWithSeed(0),
				seed: 0,
			},
			model: "m1",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
			},
			matchTokens:   []int{9906, 4435, 0, 3639, 264, 7839, 6187, 0, 220, 57668, 53901, 3574, 244, 98220, 6447, 43240},
			unMatchTokens: []int{82696, 58666, 53901, 9554, 15120, 36827, 28308, 232, 6447},
			matchPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
			},
		},
	}

	for _, tt := range tests {
		tokens, err := utils.TokenizeInputText(tt.inputText)
		assert.Equal(t, nil, err)
		fmt.Println(len(tokens))
		fmt.Println(tokens)

		matchTokens, unMatchTokens, matchPods := tt.cache.MatchPrefix(tokens, tt.model, tt.pods)

		assert.Equal(t, tt.matchTokens, matchTokens)
		assert.Equal(t, tt.unMatchTokens, unMatchTokens)
		assert.Equal(t, tt.matchPods, matchPods)
	}
}
