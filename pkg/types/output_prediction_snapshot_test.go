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
package types

import (
	"context"
	"testing"
)

type changingOutputPredictor struct {
	calls int
	first int
}

func (p *changingOutputPredictor) AddTrace(int, int, int32) {}

func (p *changingOutputPredictor) Predict(int) int {
	p.calls++
	if p.calls == 1 {
		if p.first != 0 {
			return p.first
		}
		return 16
	}
	return 1024
}

type zeroOutputPredictor struct{ calls int }

func (p *zeroOutputPredictor) AddTrace(int, int, int32) {}

func (p *zeroOutputPredictor) Predict(int) int {
	p.calls++
	return 0
}

func TestOutputPredictionCachesZero(t *testing.T) {
	predictor := &zeroOutputPredictor{}
	ctx := &RoutingContext{tokens: []int{1}}
	ctx.SetOutputPredictor(predictor)
	for i := 0; i < 3; i++ {
		got, err := ctx.TokenLength()
		if err != nil || got != 0 {
			t.Fatalf("zero prediction: got=%d err=%v", got, err)
		}
	}
	if predictor.calls != 1 {
		t.Fatalf("zero prediction was sampled %d times", predictor.calls)
	}
}

func TestOutputPredictionChangesWithPredictor(t *testing.T) {
	ctx := &RoutingContext{tokens: []int{1}}
	first := &changingOutputPredictor{first: 8}
	second := &changingOutputPredictor{first: 64}
	ctx.SetOutputPredictor(first)
	if got, err := ctx.TokenLength(); err != nil || got != 8 {
		t.Fatalf("first prediction: got=%d err=%v", got, err)
	}
	if old := ctx.SetOutputPredictor(second); old != first {
		t.Fatal("setter did not return previous predictor")
	}
	if got, err := ctx.TokenLength(); err != nil || got != 64 {
		t.Fatalf("replacement prediction: got=%d err=%v", got, err)
	}
	ctx.SetOutputPredictor(nil)
	if _, err := ctx.TokenLength(); err == nil {
		t.Fatal("detached predictor should report an error")
	}
	ctx.SetOutputPredictor(second)
	if got, err := ctx.TokenLength(); err != nil || got != 1024 {
		t.Fatalf("reattached predictor: got=%d err=%v", got, err)
	}
}

func TestOutputPredictionIsRequestScoped(t *testing.T) {
	predictor := &changingOutputPredictor{}
	first := &RoutingContext{tokens: []int{1}}
	second := &RoutingContext{tokens: []int{1}}
	first.SetOutputPredictor(predictor)
	second.SetOutputPredictor(predictor)
	a, err := first.TokenLength()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.TokenLength()
	if err != nil {
		t.Fatal(err)
	}
	if a != 16 || b != 1024 || predictor.calls != 2 {
		t.Fatalf("requests should sample independently: first=%d second=%d calls=%d", a, b, predictor.calls)
	}
}

func TestOutputPredictionResetsWhenContextReused(t *testing.T) {
	predictor := &changingOutputPredictor{}
	ctx := &RoutingContext{tokens: []int{1}}
	ctx.SetOutputPredictor(predictor)
	if got, err := ctx.TokenLength(); err != nil || got != 16 {
		t.Fatalf("original request: got=%d err=%v", got, err)
	}
	ctx.reset(context.Background(), "slo", "model", "hello", "next", "")
	ctx.tokens = []int{1}
	if _, err := ctx.TokenLength(); err == nil {
		t.Fatal("reused context must require a predictor")
	}
	ctx.SetOutputPredictor(predictor)
	if got, err := ctx.TokenLength(); err != nil || got != 1024 {
		t.Fatalf("next request: got=%d err=%v", got, err)
	}
}

func TestOutputPredictionStableAcrossFeatureReads(t *testing.T) {
	predictor := &changingOutputPredictor{}
	ctx := &RoutingContext{tokens: []int{1, 2, 3}}
	ctx.SetOutputPredictor(predictor)
	first, err := ctx.Features()
	if err != nil {
		t.Fatal(err)
	}
	second, err := ctx.Features()
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != second[0] {
		t.Fatalf("same request changed predicted output: first=%v second=%v; predictor calls=%d", first, second, predictor.calls)
	}
	length, err := ctx.TokenLength()
	if err != nil {
		t.Fatal(err)
	}
	if float64(length) != first[0] || predictor.calls != 1 {
		t.Fatalf("output prediction resampled: length=%d first=%v calls=%d", length, first, predictor.calls)
	}
}
