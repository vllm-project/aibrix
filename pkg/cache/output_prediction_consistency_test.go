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
package cache

import (
	"context"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

func TestPendingLoadUsesOneSampleFromOutputHistory(t *testing.T) {
	req := types.NewRoutingContext(context.Background(), "slo", "model", "hello", "history", "")
	defer req.Delete()
	input, err := req.PromptLength()
	if err != nil {
		t.Fatal(err)
	}
	predictor := NewSimpleOutputPredictor(1024, 2048, time.Minute)
	predictor.AddTrace(input, 16, 1)
	predictor.AddTrace(input, 1024, 1)
	calls := 0
	predictor.rand = func(n int32) int32 {
		choice := int32(calls) % n
		calls++
		return choice
	}
	req.SetOutputPredictor(predictor)
	profile := &ModelGPUProfile{
		Indexes: [][]float64{{4, 10}, {0}},
		Tputs:   [][]float64{{10}, {1}},
		E2E:     [][]float64{{0.5}, {2}},
	}
	provider := newPendingLoadProvider(profileCache{profile: profile})
	first, err := provider.GetConsumption(req, &v1.Pod{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.GetConsumption(req, &v1.Pod{})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || calls != 1 {
		t.Fatalf("same request/profile changed pending load: first=%g second=%g samples=%d", first, second, calls)
	}
}
