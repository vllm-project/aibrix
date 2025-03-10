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

// func Test_PrefixHashTableE2E(t *testing.T) {
// 	r := rand.New(rand.NewSource(time.Now().Unix()))
// 	seed := r.Uint64()
// 	cache := PrefixHashTable{
// 		blocks: map[uint64]Block{},
// 		hash:   xxhash.NewWithSeed(seed),
// 		seed:   seed,
// 	}
// 	pods := []*v1.Pod{
// 		{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
// 		{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
// 	}

// 	// inputText := "Hello World! What a Good Day! Good day to code and learn new things in LLM!! 你好世界！ 多么美好的一天啊！"
// 	// / tokens := strings.Split(inputText, " ")
// 	tokens := []byte{}

// 	matchedTokens, unMatchedTokens, matchPods := cache.MatchPrefix(tokens, "m1", pods)
// 	assert.Equal(t, 0, len(matchedTokens))
// 	assert.Equal(t,
// 		[]string{"Hello", "World!", "What", "a", "Good", "Day!", "Good", "day", "to", "code", "and", "learn", "new", "things", "in", "LLM!!", "你好世界！", "多么美好的一天啊！"},
// 		unMatchedTokens)
// 	assert.Equal(t, 0, len(matchPods))

// 	cache.AddPrefix(unMatchedTokens, "m1", "p1")
// 	matchedTokens, unMatchedTokens, matchPods = cache.MatchPrefix(tokens, "m1", pods)
// 	assert.Equal(t,
// 		[]string{"Hello", "World!", "What", "a", "Good", "Day!", "Good", "day", "to", "code", "and", "learn", "new", "things", "in", "LLM!!", "你好世界！", "多么美好的一天啊！"},
// 		matchedTokens)
// 	assert.Equal(t, 0, len(unMatchedTokens))
// 	assert.Equal(t, "p1", matchPods[0].Name)

// 	cache.Evict(time.Now().Add(60 * time.Minute))
// 	_, unMatchedTokens, matchPods = cache.MatchPrefix(tokens, "m1", pods)
// 	assert.Equal(t,
// 		[]string{"Hello", "World!", "What", "a", "Good", "Day!", "Good", "day", "to", "code", "and", "learn", "new", "things", "in", "LLM!!", "你好世界！", "多么美好的一天啊！"},
// 		unMatchedTokens)
// 	assert.Equal(t, 0, len(matchPods))
// }

// func Test_MatchPrefix(t *testing.T) {
// 	r := rand.New(rand.NewSource(time.Now().Unix()))
// 	seed := r.Uint64()
// 	tests := []*struct {
// 		name          string
// 		inputText     string
// 		cache         PrefixHashTable
// 		model         string
// 		pods          []*v1.Pod
// 		matchTokens   []string
// 		unMatchTokens []string
// 		matchPods     []*v1.Pod
// 	}{
// 		{
// 			name:      "token length more than prefix block size, no prefix blocks exist in the cache",
// 			inputText: "Hello World! What a Good Day! 你好世界！多么美好的一天啊！",
// 			cache: PrefixHashTable{
// 				blocks: map[uint64]Block{},
// 				hash:   xxhash.NewWithSeed(seed),
// 				seed:   seed,
// 			},
// 			model: "m1",
// 			pods: []*v1.Pod{
// 				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
// 				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
// 			},
// 			matchTokens:   []string{},
// 			unMatchTokens: []string{"Hello", "World!", "What", "a", "Good", "Day!", "你好世界！多么美好的一天啊！"},
// 			matchPods:     nil,
// 		},
// 		{
// 			name:      "token length more than prefix block size, one prefix block exist in the cache",
// 			inputText: "Hello World! What a Good Day! Good day to code and learn new things in LLM!! 你好世界！多么美好的一天啊！",
// 			cache: PrefixHashTable{
// 				blocks: map[uint64]Block{
// 					8439316938363978324: {
// 						modelToPods: map[string]map[string]time.Time{
// 							"m1": {
// 								"p1": time.Now(),
// 							},
// 						},
// 						lastAccessTime: time.Now(),
// 					},
// 				},
// 				hash: xxhash.NewWithSeed(0),
// 				seed: 0,
// 			},
// 			model: "m1",
// 			pods: []*v1.Pod{
// 				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
// 				{ObjectMeta: metav1.ObjectMeta{Name: "p2"}},
// 			},
// 			matchTokens:   []string{"Hello", "World!", "What", "a", "Good", "Day!", "Good", "day", "to", "code", "and", "learn", "new", "things", "in", "LLM!!"},
// 			unMatchTokens: []string{"你好世界！多么美好的一天啊！"},
// 			matchPods: []*v1.Pod{
// 				{ObjectMeta: metav1.ObjectMeta{Name: "p1"}},
// 			},
// 		},
// 	}

// 	for _, tt := range tests {
// 		matchTokens, unMatchTokens, matchPods := tt.cache.MatchPrefix(strings.Split(tt.inputText, " "), tt.model, tt.pods)

// 		assert.Equal(t, tt.matchTokens, matchTokens, tt.name)
// 		assert.Equal(t, tt.unMatchTokens, unMatchTokens, tt.name)
// 		assert.Equal(t, tt.matchPods, matchPods, tt.name)
// 	}
// }
