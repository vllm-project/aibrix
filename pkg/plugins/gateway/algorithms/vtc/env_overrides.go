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

package vtc

import "github.com/vllm-project/aibrix/pkg/types"

// EnvOverrides returns the VTC routing knob defaults the environment
// configures. The routing algorithm package folds them into the process default
// table at startup.
//
// The token tracker's window, time unit and token floors are absent on purpose:
// they configure one tracker per gateway process rather than a routing
// decision, so they stay environment-only.
func EnvOverrides() types.VTCOverrides {
	return types.VTCOverrides{
		MaxPodLoad:        maxPodLoad,
		FairnessWeight:    fairnessWeight,
		UtilizationWeight: utilizationWeight,
	}
}
