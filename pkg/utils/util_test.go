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
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestLoadEnvDuration(t *testing.T) {
	const key = "AIBRIX_TEST_LOAD_ENV_DURATION"
	const def = 10 * time.Second

	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "unset uses default", value: "", want: def},
		{name: "seconds", value: "45s", want: 45 * time.Second},
		{name: "compound duration", value: "1m30s", want: 90 * time.Second},
		{name: "sub-second", value: "250ms", want: 250 * time.Millisecond},
		{name: "unparsable uses default", value: "soon", want: def},
		{name: "missing unit uses default", value: "30", want: def},
		// time.ParseDuration accepts a bare "0" without a unit, so it has to be
		// rejected by the positivity check rather than by the parser.
		{name: "bare zero uses default", value: "0", want: def},
		{name: "zero uses default", value: "0s", want: def},
		{name: "negative uses default", value: "-1s", want: def},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(key, tt.value)
			assert.Equal(t, tt.want, LoadEnvDuration(key, def))
		})
	}
}

// TestLoadEnvNonNegativeInt pins the one way this loader differs from
// LoadEnvInt: an explicit 0 reaches the caller, because for the knobs using it
// 0 is the documented off switch. Negative and unparseable values still fall
// back, as there they mean a typo.
func TestLoadEnvNonNegativeInt(t *testing.T) {
	const key = "AIBRIX_TEST_NON_NEGATIVE_INT"
	const defaultValue = 7

	cases := []struct {
		name     string
		raw      string
		set      bool
		expected int
	}{
		{name: "unset", set: false, expected: defaultValue},
		{name: "empty", raw: "", set: true, expected: defaultValue},
		{name: "zero", raw: "0", set: true, expected: 0},
		{name: "positive", raw: "5", set: true, expected: 5},
		{name: "negative", raw: "-1", set: true, expected: defaultValue},
		{name: "unparseable", raw: "abc", set: true, expected: defaultValue},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv first either way, so the variable is restored after the
			// subtest even when the case under test is "unset".
			t.Setenv(key, tc.raw)
			if !tc.set {
				require.NoError(t, os.Unsetenv(key))
			}
			assert.Equal(t, tc.expected, LoadEnvNonNegativeInt(key, defaultValue))
		})
	}

	// LoadEnvInt keeps rejecting 0: other callers depend on that.
	t.Run("LoadEnvInt still rejects zero", func(t *testing.T) {
		t.Setenv(key, "0")
		assert.Equal(t, defaultValue, LoadEnvInt(key, defaultValue))
	})
}

func TestPathWithoutQuery(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "empty", path: "", want: ""},
		{name: "no query string", path: "/v1/chat/completions", want: "/v1/chat/completions"},
		{name: "query string is stripped", path: "/v1/chat/completions?beta=true", want: "/v1/chat/completions"},
		{name: "query string after nested path segments", path: "/v1/videos/abc/content?download=1", want: "/v1/videos/abc/content"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PathWithoutQuery(tc.path))
		})
	}
}
