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
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/prefixcacheindexer/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_PrefixHashTableE2E(t *testing.T) {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	seed := r.Uint64()
	cache := PrefixHashTable{
		store: cache.NewLRUStore[uint64, Block](defaultPrefixCacheBlockNumber, 20*time.Second, 2*time.Second),
		hash:  xxhash.NewWithSeed(seed),
		seed:  seed,
	}
	pods := []*v1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
	}

	inputText := "Hello World! What a Good Day! Good day to code and learn new things in LLM!! 你好世界！ 多么美好的一天啊！"
	tokens, err := tokenizer.NewStringTokenizer().TokenizeInputText(inputText)
	assert.NoError(t, err)

	matchedTokens, unMatchedTokens, matchPods := cache.MatchPrefix(tokens, "m1", pods)
	fmt.Println(matchedTokens)
	fmt.Println(unMatchedTokens)
	assert.Equal(t, 0, len(matchedTokens))
	assert.Equal(t,
		[]byte{72, 101, 108, 108, 111, 87, 111, 114, 108, 100, 33, 87, 104, 97, 116, 97, 71, 111, 111, 100, 68, 97, 121, 33, 71, 111, 111, 100, 100, 97, 121, 116, 111, 99, 111, 100, 101, 97, 110, 100, 108, 101, 97, 114, 110, 110, 101, 119, 116, 104, 105, 110, 103, 115, 105, 110, 76, 76, 77, 33, 33, 228, 189, 160, 229, 165, 189, 228, 184, 150, 231, 149, 140, 239, 188, 129, 229, 164, 154, 228, 185, 136, 231, 190, 142, 229, 165, 189, 231, 154, 132, 228, 184, 128, 229, 164, 169, 229, 149, 138, 239, 188, 129},
		unMatchedTokens)
	assert.Equal(t, 0, len(matchPods))

	cache.AddPrefix(unMatchedTokens, "m1", "p1")
	matchedTokens, unMatchedTokens, matchPods = cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t,
		[]byte{72, 101, 108, 108, 111, 87, 111, 114, 108, 100, 33, 87, 104, 97, 116, 97, 71, 111, 111, 100, 68, 97, 121, 33, 71, 111, 111, 100, 100, 97, 121, 116, 111, 99, 111, 100, 101, 97, 110, 100, 108, 101, 97, 114, 110, 110, 101, 119, 116, 104, 105, 110, 103, 115, 105, 110, 76, 76, 77, 33, 33, 228, 189, 160, 229, 165, 189, 228, 184, 150, 231, 149, 140, 239, 188, 129, 229, 164, 154, 228, 185, 136, 231, 190, 142, 229, 165, 189, 231, 154, 132, 228, 184, 128, 229, 164, 169, 229, 149, 138, 239, 188, 129},
		matchedTokens)
	assert.Equal(t, 0, len(unMatchedTokens))
	assert.Equal(t, "p1", matchPods[0].Name)

	time.Sleep(20 * time.Second)
	_, unMatchedTokens, matchPods = cache.MatchPrefix(tokens, "m1", pods)
	assert.Equal(t,
		[]byte{72, 101, 108, 108, 111, 87, 111, 114, 108, 100, 33, 87, 104, 97, 116, 97, 71, 111, 111, 100, 68, 97, 121, 33, 71, 111, 111, 100, 100, 97, 121, 116, 111, 99, 111, 100, 101, 97, 110, 100, 108, 101, 97, 114, 110, 110, 101, 119, 116, 104, 105, 110, 103, 115, 105, 110, 76, 76, 77, 33, 33, 228, 189, 160, 229, 165, 189, 228, 184, 150, 231, 149, 140, 239, 188, 129, 229, 164, 154, 228, 185, 136, 231, 190, 142, 229, 165, 189, 231, 154, 132, 228, 184, 128, 229, 164, 169, 229, 149, 138, 239, 188, 129},
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
		matchTokens   []byte
		unMatchTokens []byte
		matchPods     []*v1.Pod
		blocks        map[uint64]Block
	}{
		{
			name:      "token length more than prefix block size, no prefix blocks exist in the cache",
			inputText: "Hello World! What a Good Day! 你好世界！多么美好的一天啊！",
			cache: PrefixHashTable{
				store: cache.NewLRUStore[uint64, Block](defaultPrefixCacheBlockNumber, 20*time.Second, 10*time.Second),
				hash:  xxhash.NewWithSeed(seed),
				seed:  seed,
			},
			model: "m1",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
			},
			matchTokens:   []byte{},
			unMatchTokens: []byte{72, 101, 108, 108, 111, 87, 111, 114, 108, 100, 33, 87, 104, 97, 116, 97, 71, 111, 111, 100, 68, 97, 121, 33, 228, 189, 160, 229, 165, 189, 228, 184, 150, 231, 149, 140, 239, 188, 129, 229, 164, 154, 228, 185, 136, 231, 190, 142, 229, 165, 189, 231, 154, 132, 228, 184, 128, 229, 164, 169, 229, 149, 138, 239, 188, 129},
			matchPods:     nil,
			blocks: map[uint64]Block{
				14691102025703449771: {
					modelToPods: map[string]map[string]time.Time{
						"m1": {
							"p1": time.Now(),
						},
					},
				},
				8713185073040773989: {
					modelToPods: map[string]map[string]time.Time{
						"m1": {
							"p1": time.Now(),
						},
					},
				},
			},
		},
		{
			name:      "token length more than prefix block size, one prefix block exist in the cache",
			inputText: "Hello World! What a Good Day! Good day to code and learn new things in LLM!! 你好世界！多么美好的一天啊！",
			cache: PrefixHashTable{
				store: cache.NewLRUStore[uint64, Block](defaultPrefixCacheBlockNumber, 20*time.Second, 10*time.Second),
				hash:  xxhash.NewWithSeed(0),
				seed:  0,
			},
			model: "m1",
			pods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
			},
			matchTokens:   []byte{72, 101, 108, 108, 111, 87, 111, 114, 108, 100, 33, 87, 104, 97, 116, 97, 71, 111, 111, 100, 68, 97, 121, 33, 71, 111, 111, 100, 100, 97, 121, 116},
			unMatchTokens: []byte{111, 99, 111, 100, 101, 97, 110, 100, 108, 101, 97, 114, 110, 110, 101, 119, 116, 104, 105, 110, 103, 115, 105, 110, 76, 76, 77, 33, 33, 228, 189, 160, 229, 165, 189, 228, 184, 150, 231, 149, 140, 239, 188, 129, 229, 164, 154, 228, 185, 136, 231, 190, 142, 229, 165, 189, 231, 154, 132, 228, 184, 128, 229, 164, 169, 229, 149, 138, 239, 188, 129},
			matchPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
			},
			blocks: map[uint64]Block{
				14691102025703449771: {
					modelToPods: map[string]map[string]time.Time{
						"m1": {
							"p1": time.Now(),
						},
					},
				},
				8713185073040773989: {
					modelToPods: map[string]map[string]time.Time{
						"m1": {
							"p1": time.Now(),
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		for hash, block := range tt.blocks {
			tt.cache.store.Put(hash, block)
		}

		tokens, err := tokenizer.NewStringTokenizer().TokenizeInputText(tt.inputText)
		assert.NoError(t, err)
		matchTokens, unMatchTokens, matchPods := tt.cache.MatchPrefix(tokens, tt.model, tt.pods)

		assert.Equal(t, tt.matchTokens, matchTokens, tt.name)
		assert.Equal(t, tt.unMatchTokens, unMatchTokens, tt.name)
		assert.Equal(t, tt.matchPods, matchPods, tt.name)
	}
}
