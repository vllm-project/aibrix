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

package tokenizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHelloWorld = "Hello, world!"

func TestNewTokenizer(t *testing.T) {
	tests := []struct {
		name          string
		tokenizerType string
		config        interface{}
		wantErr       bool
		errContains   string
	}{
		{
			name:          "tiktoken tokenizer",
			tokenizerType: "tiktoken",
			config:        nil,
			wantErr:       false,
		},
		{
			name:          "character tokenizer",
			tokenizerType: "character",
			config:        nil,
			wantErr:       false,
		},
		{
			name:          "unsupported tokenizer",
			tokenizerType: "unknown",
			config:        nil,
			wantErr:       true,
			errContains:   "unsupported tokenizer type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokenizer, err := NewTokenizer(tt.tokenizerType, tt.config)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, tokenizer)

			// Test basic functionality
			result, err := tokenizer.TokenizeInputText(testHelloWorld)
			require.NoError(t, err)
			require.NotEmpty(t, result)
		})
	}
}

func TestFactoryCompatibility(t *testing.T) {
	// Test that factory method produces same results as direct constructors

	// Test tiktoken
	t.Run("tiktoken compatibility", func(t *testing.T) {
		direct := NewTiktokenTokenizer()
		factory, err := NewTokenizer("tiktoken", nil)
		require.NoError(t, err)

		testText := testHelloWorld
		directResult, err1 := direct.TokenizeInputText(testText)
		factoryResult, err2 := factory.TokenizeInputText(testText)

		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.Equal(t, directResult, factoryResult)
	})

	// Test character
	t.Run("character compatibility", func(t *testing.T) {
		direct := NewCharacterTokenizer()
		factory, err := NewTokenizer("character", nil)
		require.NoError(t, err)

		testText := testHelloWorld
		directResult, err1 := direct.TokenizeInputText(testText)
		factoryResult, err2 := factory.TokenizeInputText(testText)

		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.Equal(t, directResult, factoryResult)
	})
}
