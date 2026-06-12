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

package prefill

import (
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// LogContext carries router-resolved metadata included in prefill structured logs.
type LogContext struct {
	PrefillScorePolicy string
	DecodeScorePolicy  string
}

// PrefillExecutor executes the prefill phase of a disaggregated-inference request.
// Implementations are responsible for payload preparation, HTTP dispatch (async or
// sync depending on the engine), and tracker lifecycle.
type PrefillExecutor interface {
	Execute(routingCtx *types.RoutingContext, prefillPod *v1.Pod, llmEngine string, logCtx LogContext) error
}
