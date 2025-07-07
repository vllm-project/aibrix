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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_PrefixHashTableE2E(t *testing.T) {
	cache := NewPrefixHashTable()
	model := "m1"
	model2 := "m2"
	targetPod := "p1"
	targetPod2 := "p2"
	prefixCacheBlockSize = 4

	matchedPods, prefixHashes := cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9}, model, getReadyPods())
	assert.Equal(t, 0, len(matchedPods))
	assert.Equal(t, 2, len(prefixHashes))

	// add unmatched prefix hashes
	cache.AddPrefix(prefixHashes, model, targetPod)

	// run match prefix will different combinations
	matchedPods, prefixHashes = cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7}, model, getReadyPods())
	assert.Equal(t, 1, len(matchedPods))
	assert.Equal(t, targetPod, getFirstKey(matchedPods))
	assert.Equal(t, 1, len(prefixHashes))

	matchedPods, prefixHashes = cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8}, model, getReadyPods())
	assert.Equal(t, 1, len(matchedPods))
	assert.Equal(t, targetPod, getFirstKey(matchedPods))
	assert.Equal(t, 2, len(prefixHashes))

	matchedPods, prefixHashes = cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9}, model, getReadyPods())
	assert.Equal(t, 1, len(matchedPods))
	assert.Equal(t, targetPod, getFirstKey(matchedPods))
	assert.Equal(t, 2, len(prefixHashes))

	matchedPods, prefixHashes = cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}, model, getReadyPods())
	assert.Equal(t, 1, len(matchedPods))
	assert.Equal(t, targetPod, getFirstKey(matchedPods))
	assert.Equal(t, 3, len(prefixHashes))

	// there is no way to match prefix, even same block has been added
	matchedPods, prefixHashes = cache.MatchPrefix([]byte{5, 6, 7, 8, 9, 10, 11, 12, 13}, model, getReadyPods())
	assert.Equal(t, 0, len(matchedPods))
	assert.Equal(t, 2, len(prefixHashes))

	// different model sharing same prefix
	matchedPods, prefixHashes = cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}, model2, getReadyPods())
	assert.Equal(t, 0, len(matchedPods))
	assert.Equal(t, 3, len(prefixHashes))

	cache.AddPrefix(prefixHashes, model2, targetPod)
	cache.AddPrefix(prefixHashes[0:2], model2, targetPod2)

	matchedPods, prefixHashes = cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8}, model2, getReadyPods())
	assert.Equal(t, 2, len(matchedPods))
	assert.Equal(t, 2, len(prefixHashes))

	matchedPods, prefixHashes = cache.MatchPrefix([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}, model2, getReadyPods())
	assert.Equal(t, 2, len(matchedPods))
	assert.Equal(t, 100, matchedPods[targetPod])
	assert.Equal(t, 66, matchedPods[targetPod2])
	assert.Equal(t, 3, len(prefixHashes))
}

func Test_PrefixHashTableConcurrency(t *testing.T) {
	prefixHashTable := NewPrefixHashTable()
	model := "m1"
	targetPod := "p1"
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens := []byte(fmt.Sprintf("this is %v message", rand.Intn(10)))
			_, prefixHashes := prefixHashTable.MatchPrefix(tokens, model, getReadyPods())
			prefixHashTable.AddPrefix(prefixHashes, model, targetPod)
		}()
	}
	wg.Wait()
}

func getFirstKey(matchedPods map[string]int) string {
	var targetPod string
	for pod := range matchedPods {
		targetPod = pod
		break
	}
	return targetPod
}

func getReadyPods() map[string]struct{} {
	return map[string]struct{}{
		"p1": {},
		"p2": {},
		"p3": {},
	}
}
