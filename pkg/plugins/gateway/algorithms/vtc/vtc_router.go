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

// Package vtc implements the Virtual Token Credit routing algorithm
package vtc

import (
	"context"

	"github.com/vllm-project/aibrix/pkg/types"
)

// Router constants
const RouterVTCBasic types.RoutingAlgorithm = "vtc-basic"

// TODO: add other variants - "vtc-fair", "vtc-max-fair", "vtc-pred-50", "vtc-oracle"

// TokenTracker tracks token usage per user
type TokenTracker interface {
	GetTokenCount(ctx context.Context, user string) (float64, error)

	UpdateTokenCount(ctx context.Context, user string, inputTokens, outputTokens float64) error
}

// TokenEstimator estimates token counts for messages
type TokenEstimator interface {
	EstimateInputTokens(message string) float64

	EstimateOutputTokens(message string) float64
}

type VTCConfig struct {
	Variant           types.RoutingAlgorithm
	InputTokenWeight  float64
	OutputTokenWeight float64
}

func DefaultVTCConfig() VTCConfig {
	return VTCConfig{
		Variant:           RouterVTCBasic,
		InputTokenWeight:  1.0,
		OutputTokenWeight: 1.5,
	}
}

func NewVTCBasicRouter() (types.Router, error) {
	config := DefaultVTCConfig()
	configPtr := &config
	tokenEstimator := NewSimpleTokenEstimator()
	tokenTracker := NewInMemoryTokenTracker(configPtr)
	return NewBasicVTCRouter(tokenTracker, tokenEstimator, configPtr)
}
