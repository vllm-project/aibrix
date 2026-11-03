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
package queue

import (
	"context"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
)

type alternatingOutputPredictor struct{ calls int }

func (p *alternatingOutputPredictor) AddTrace(int, int, int32) {}

func (p *alternatingOutputPredictor) Predict(int) int {
	p.calls++
	if p.calls%2 == 1 {
		return 16
	}
	return 1024
}

func TestSLORankUsesSamePredictionForIdenticalProfiles(t *testing.T) {
	now := time.Now()
	req := types.NewRoutingContext(context.Background(), "slo", "model", "hello", "prediction", "")
	defer req.Delete()
	req.RequestTime = now
	predictor := &alternatingOutputPredictor{}
	req.SetOutputPredictor(predictor)
	firstProfile := &cache.ModelGPUProfile{
		Deployment: "first",
		Indexes:    [][]float64{{4, 10}, {0}},
		E2E:        [][]float64{{0.05}, {1.2}},
		SLOs:       cache.ModelSLOs{E2E: 0.5},
	}
	secondProfile := *firstProfile
	secondProfile.Deployment = "second"
	queue := &SLOQueue{}
	first, err := queue.rank(now, req, firstProfile)
	if err != nil {
		t.Fatal(err)
	}
	second, err := queue.rank(now, req, &secondProfile)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || predictor.calls != 1 {
		t.Fatalf("identical profiles ranked differently for one request: first=%g second=%g prediction calls=%d", first, second, predictor.calls)
	}
}
