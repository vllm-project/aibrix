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
package types

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
)

func getBlockChannel(cb func(), duration time.Duration) chan bool {
	block := make(chan bool)
	go func() {
		cb()
		block <- false
	}()
	return block
}

func shouldBlock(cb func(), duration time.Duration) {
	block := getBlockChannel(cb, duration)
	Consistently(block, duration).ShouldNot(Receive(BeFalse()))
}

func shouldNotBlock(cb func(), duration time.Duration) {
	block := getBlockChannel(cb, duration)
	Eventually(block, duration).Should(Receive(BeFalse()))
}

type testOutputPredictor struct {
}

func (p *testOutputPredictor) AddTrace(inputTokens, outputTokens int, cnt int32) {
	// Do nothing
}

func (p *testOutputPredictor) Predict(promptLen int) (outputLen int) {
	return promptLen
}

var _ = Describe("RouterContext", func() {
	It("should initialize correctly", func() {
		predictor := &testOutputPredictor{}
		ctx := context.Background()
		rctx := NewRoutingContext(ctx, "algorithm", "model", "message", "r1", "")
		rctx.SetOutputPreditor(predictor)
		Expect(rctx.Context).To(BeIdenticalTo(ctx))
		Expect(rctx.Algorithm).To(Equal(RoutingAlgorithm("algorithm")))
		Expect(rctx.RequestID).To(Equal("r1"))
		Expect(rctx.Model).To(Equal("model"))
		Expect(rctx.Message).To(Equal("message"))
		Expect(rctx.predictor).To(BeIdenticalTo(predictor))
		shouldBlock(func() { rctx.TargetPod() }, 100*time.Millisecond)
		Expect(rctx.targetPod.Load()).To(BeIdenticalTo(nilPod))
		Expect(rctx.getError()).To(BeNil())

		rctx.SetError(fmt.Errorf("error"))
		Expect(rctx.targetPod.Load()).To(BeNil()) // target pod set
		Expect(rctx.getError()).ToNot(BeNil())    // No blocking

		rctx.Delete()
		ctx2 := context.Background()
		rctx2 := NewRoutingContext(ctx2, "algorithm2", "model2", "message2", "r2", "")
		Expect(rctx2).To(BeIdenticalTo(rctx)) // routing context reused
		Expect(rctx2.Context).To(BeIdenticalTo(ctx2))
		Expect(rctx2.Algorithm).To(Equal(RoutingAlgorithm("algorithm2")))
		Expect(rctx.RequestID).To(Equal("r2"))
		Expect(rctx2.Model).To(Equal("model2"))
		Expect(rctx2.Message).To(Equal("message2"))
		Expect(rctx.predictor).To(BeNil())
		shouldBlock(func() { rctx.TargetPod() }, 100*time.Millisecond)
		Expect(rctx.targetPod.Load()).To(BeIdenticalTo(nilPod))
		Expect(rctx.getError()).To(BeNil()) // No blocking

		rctx.Delete()
	})

	It("should SetTargetPod accept nil", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		ctx.SetTargetPod(nil)
		shouldNotBlock(func() { ctx.TargetPod() }, 100*time.Millisecond)
		Expect(ctx.TargetPod()).To(BeNil())
	})

	It("should HasRouted() indicate correctly", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		Expect(ctx.HasRouted()).To(BeFalse())

		ctx.SetTargetPod(&v1.Pod{})
		Expect(ctx.HasRouted()).To(BeTrue())
	})

	It("should reset without SetTargetPod()", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		ctx.Delete()
		shouldNotBlock(func() { ctx.TargetPod() }, 100*time.Millisecond)
		Expect(ctx.TargetPod()).To(BeNil())
	})

	It("should SetTargetPod twice ok but will not change original value", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		pod := &v1.Pod{}
		ctx.SetTargetPod(pod)
		Expect(ctx.TargetPod()).To(BeIdenticalTo(pod))

		Expect(func() {
			ctx.SetTargetPod(&v1.Pod{})
		}).ToNot(Panic())
		Expect(ctx.TargetPod()).To(BeIdenticalTo(pod))
	})

	It("should SetError also SetTargetPod", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		err := fmt.Errorf("test error")
		ctx.SetError(err)
		shouldNotBlock(func() { ctx.TargetPod() }, 100*time.Millisecond)
		Expect(ctx.TargetPod()).To(BeNil())
		Expect(ctx.GetError()).To(BeIdenticalTo(err))
	})

	It("should HasRouted() indicate correctly", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		Expect(ctx.HasRouted()).To(BeFalse())

		ctx.SetTargetPod(&v1.Pod{})
		Expect(ctx.HasRouted()).To(BeTrue())

		ctx = NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		ctx.SetTargetPod(nil)
		Expect(ctx.HasRouted()).To(BeFalse())
	})

	It("should reset without SetTargetPod()", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		ctx.Delete()
		shouldNotBlock(func() { ctx.TargetPod() }, 100*time.Millisecond)
	})

	It("should TargetPod block and unblock successfully", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		ctx.debugDelay = 100 * time.Millisecond
		shouldBlock(func() { ctx.TargetPod() }, 30*time.Millisecond)

		// Set targetPod before blocking()
		ctx.SetTargetPod(&v1.Pod{})

		shouldNotBlock(func() { ctx.TargetPod() }, 100*time.Millisecond)
	})

	It("should GetError block and unblock successfully", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		ctx.debugDelay = 100 * time.Millisecond
		// nolint: errcheck
		shouldBlock(func() { ctx.GetError() }, 30*time.Millisecond)

		// Set targetPod before blocking()
		ctx.SetError(fmt.Errorf("test error"))

		// nolint: errcheck
		shouldNotBlock(func() { ctx.GetError() }, 100*time.Millisecond)
	})
})
