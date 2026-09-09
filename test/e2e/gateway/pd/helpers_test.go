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

package e2e

import framework "github.com/vllm-project/aibrix/test/e2e/framework"

var e2eConfig = framework.LoadConfig()

var (
	gatewayURL                            = e2eConfig.GatewayURL
	apiKey                                = e2eConfig.APIKey
	initializeClient                      = framework.InitializeClient
	createOpenAIClientWithRoutingStrategy = framework.NewOpenAIClientWithRoutingStrategy
	validateAllPodsAreReady               = framework.ValidateAllPodsAreReady
	waitForPDDisaggregationRouting        = framework.WaitForPDDisaggregationRouting
	waitForPDCombinedRouting              = framework.WaitForPDCombinedRouting
)

const (
	modelNameVLLM       = framework.ModelNameVLLM
	modelNameVLLMBucket = framework.ModelNameVLLMBucket
	modelNameSGLang     = framework.ModelNameSGLang
	modelNameTRTLLM     = framework.ModelNameTRTLLM
)
