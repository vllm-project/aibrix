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

package config

type RuntimeConfig struct {
	EnableRuntimeSidecar bool
	DebugMode            bool
	ModelAdapterOpt      ModelAdapterOpt
}

// ModelAdapterOpt contains options for model adapter controller.
type ModelAdapterOpt struct {
	// SchedulerPolicyName is the name of the scheduler policy
	// to use for model adapter controller.
	SchedulerPolicyName string
}

// NewRuntimeConfig creates a new RuntimeConfig with specified settings.
func NewRuntimeConfig(enableRuntimeSidecar, debugMode bool, schedulerPolicyName string) RuntimeConfig {
	return RuntimeConfig{
		EnableRuntimeSidecar: enableRuntimeSidecar,
		DebugMode:            debugMode,
		ModelAdapterOpt: ModelAdapterOpt{
			SchedulerPolicyName: schedulerPolicyName,
		},
	}
}
