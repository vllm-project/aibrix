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

package engine

import (
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// DefaultHandler is a no-op handler used for unknown or future engines.
// It performs a synchronous prefill with no request augmentation or response
// processing, which matches the original "default" branch behaviour.
type DefaultHandler struct{}

func (h *DefaultHandler) Name() string  { return "default" }
func (h *DefaultHandler) IsAsync() bool { return false }
func (h *DefaultHandler) AugmentPrefillRequest(_ *types.RoutingContext, _ *v1.Pod, _ map[string]any) error {
	return nil
}
func (h *DefaultHandler) MergePrefillResponse(_ *types.RoutingContext, _ map[string]any, _ *v1.Pod) error {
	return nil
}
