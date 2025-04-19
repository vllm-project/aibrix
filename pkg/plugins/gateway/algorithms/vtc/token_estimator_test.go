1/*
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

package vtc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSimpleTokenEstimator_EstimateInputTokens(t *testing.T) {
	estimator := NewSimpleTokenEstimator()

	tests := []struct {
		name     string
		message  string
		expected float64
	}{
		{"Empty message", "", 0},
		{"Short message", "Hello", 2},                   // 5 chars / 4 = 1.25 -> ceil = 2
		{"Divisible message", "abcd", 1},                // 4 chars / 4 = 1 -> ceil = 1
		{"Non-divisible message", "abcde", 2},           // 5 chars / 4 = 1.25 -> ceil = 2
		{"Long message", "This is a longer message", 6}, // 24 chars / 4 = 6 -> ceil = 6
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := estimator.EstimateInputTokens(tt.message)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestSimpleTokenEstimator_EstimateOutputTokens(t *testing.T) {
	estimator := NewSimpleTokenEstimator()

	tests := []struct {
		name     string
		message  string
		expected float64
	}{
		{"Empty message", "", 0},                        // input 0 * 1.5 = 0 -> ceil = 0
		{"Short message", "Hello", 3},                   // input 2 * 1.5 = 3 -> ceil = 3
		{"Divisible message", "abcd", 2},                // input 1 * 1.5 = 1.5 -> ceil = 2
		{"Non-divisible message", "abcde", 3},           // input 2 * 1.5 = 3 -> ceil = 3
		{"Long message", "This is a longer message", 9}, // input 6 * 1.5 = 9 -> ceil = 9
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := estimator.EstimateOutputTokens(tt.message)
			assert.Equal(t, tt.expected, actual)
		})
	}
}
