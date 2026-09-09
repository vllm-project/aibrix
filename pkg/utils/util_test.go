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

package utils

import (
	"slices"
	"sync"
	"testing"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"github.com/stretchr/testify/assert"
)

func TestTokenizeInputText(t *testing.T) {
	inputStr := "Hello World, 你好世界"
	tokens, err := TokenizeInputText(inputStr)
	assert.Equal(t, nil, err)

	outputStr, err := DetokenizeText(tokens)
	assert.NoError(t, err)
	assert.Equal(t, inputStr, outputStr)

	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	tke, _ := tiktoken.GetEncoding(encoding)
	outputStr = tke.Decode(tokens)
	assert.Equal(t, inputStr, outputStr)
}

func TestShuffle(t *testing.T) {
	t.Run("preserves the multiset of elements", func(t *testing.T) {
		original := []int{3, 1, 4, 1, 5, 9, 2, 6, 5, 3, 5}
		shuffled := slices.Clone(original)
		Shuffle(shuffled)

		want, got := slices.Clone(original), slices.Clone(shuffled)
		slices.Sort(want)
		slices.Sort(got)
		assert.Equal(t, want, got, "Shuffle must not add, drop or alter elements")
	})

	t.Run("handles empty and single-element slices", func(t *testing.T) {
		var nilSlice []int
		assert.NotPanics(t, func() { Shuffle(nilSlice) })
		assert.NotPanics(t, func() { Shuffle([]int{}) })

		single := []string{"only"}
		assert.NotPanics(t, func() { Shuffle(single) })
		assert.Equal(t, []string{"only"}, single)
	})

	t.Run("permutes the order", func(t *testing.T) {
		// A shuffle that never reorders would silently defeat the tie-breaking
		// this function exists for. Returning the identity 100 times in a row
		// has probability (1/8!)^100, so this cannot fail by chance.
		identity := []int{0, 1, 2, 3, 4, 5, 6, 7}
		permuted := false
		for i := 0; i < 100 && !permuted; i++ {
			candidate := slices.Clone(identity)
			Shuffle(candidate)
			permuted = !slices.Equal(candidate, identity)
		}
		assert.True(t, permuted, "Shuffle never changed the order in 100 attempts")
	})

	t.Run("is safe for concurrent use", func(t *testing.T) {
		// Every goroutine owns its slice, so the only shared state is the
		// process-wide math/rand/v2 source Shuffle draws from. Run under -race.
		const goroutines, iterations, length = 8, 500, 10
		sorted := make([]int, length)
		for i := range sorted {
			sorted[i] = i
		}

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func() {
				defer wg.Done()
				own := slices.Clone(sorted)
				for i := 0; i < iterations; i++ {
					Shuffle(own)
				}
				slices.Sort(own)
				assert.Equal(t, sorted, own)
			}()
		}
		wg.Wait()
	})
}
