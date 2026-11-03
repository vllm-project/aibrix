/*
Copyright 2026 The Aibrix Team.

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

package router

import (
	"time"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
)

// EnvOverrides returns the PD routing knob defaults the environment configures: the ones the pd
// package owns (pd.EnvOverrides) plus the ones the router reads from its own package variables.
// The routing algorithm package folds them into the process default table at startup.
func EnvOverrides() types.PDOverrides {
	defaults := pd.EnvOverrides()
	defaults.Spreads = types.PDSpreadOverrides{
		PrefillLoadImbalanceMinSpread:      aibrixPrefillLoadImbalanceMinSpread,
		DecodeLoadImbalanceMinSpread:       aibrixDecodeLoadImbalanceMinSpread,
		DecodeThroughputImbalanceMinSpread: aibrixDecodeThroughputImbalanceMinSpread,
		DecodeScoreRatioThreshold:          aibrixDecodeScoreRatioThreshold,
	}
	defaults.PromptLengthBucketing = aibrixPromptLengthBucketing
	defaults.PrefillRequestTimeout = time.Duration(prefillRequestTimeout) * time.Second
	return defaults
}
