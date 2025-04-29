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

package vtc

import (
	"math"
)

type SimpleTokenEstimator struct {
	CharactersPerToken float64
	OutputRatio        float64
}

// TODO: advanced token estimators
func NewSimpleTokenEstimator() TokenEstimator {
	return &SimpleTokenEstimator{
		CharactersPerToken: 4.0, // Rough approximation: 4 chars per token
		OutputRatio:        1.5, // Default output/input ratio
	}
}

func (e *SimpleTokenEstimator) EstimateInputTokens(message string) float64 {
	if message == "" {
		return 0
	}

	return math.Ceil(float64(len(message)) / e.CharactersPerToken)
}

func (e *SimpleTokenEstimator) EstimateOutputTokens(message string) float64 {
	inputTokens := e.EstimateInputTokens(message)
	return math.Ceil(inputTokens * e.OutputRatio)
}
