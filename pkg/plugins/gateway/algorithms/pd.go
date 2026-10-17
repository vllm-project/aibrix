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

package routingalgorithms

import (
	v1 "k8s.io/api/core/v1"

	pdrouter "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/router"
)

// The PD router lives in the pd/router package. Its algorithm name stays re-exported here so
// callers keep referring to it as routingalgorithms.RouterPD.
const RouterPD = pdrouter.RouterPD

func init() {
	Register(RouterPD, pdrouter.NewPDRouter)
}

// ValidateAndGetLLMEngine validates that all pods use the same LLM engine label and returns it.
// It forwards to the PD router package, where the PD path uses it.
func ValidateAndGetLLMEngine(pods []*v1.Pod) (string, error) {
	return pdrouter.ValidateAndGetLLMEngine(pods)
}
