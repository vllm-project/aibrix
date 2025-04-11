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

var _ = Describe("RouterContext", func() {
	It("should initialize correctly", func() {
		ctx := context.Background()
		rctx := NewRoutingContext(ctx, "algorithm", "model", "message", "r1", "")
		Expect(rctx.Context).To(BeIdenticalTo(ctx))
		Expect(rctx.Algorithm).To(Equal(RoutingAlgorithm("algorithm")))
		Expect(rctx.Model).To(Equal("model"))
		Expect(rctx.Message).To(Equal("message"))
		shouldBlock(func() { rctx.TargetPod() }, 100*time.Millisecond)
		Expect(rctx.targetPod.Load()).To(BeIdenticalTo(nilPod))

		pod := &v1.Pod{}
		rctx.SetTargetPod(pod)
		Expect(rctx.targetPod.Load()).To(BeIdenticalTo(pod)) // target pod set
		Expect(rctx.TargetPod()).To(BeIdenticalTo(pod))      // No blocking

		rctx.Delete()
		ctx2 := context.Background()
		rctx2 := NewRoutingContext(ctx2, "algorithm2", "model2", "message2", "r1", "")
		Expect(rctx2).To(BeIdenticalTo(rctx)) // routing context reused
		Expect(rctx2.Context).To(BeIdenticalTo(ctx2))
		Expect(rctx2.Algorithm).To(Equal(RoutingAlgorithm("algorithm2")))
		Expect(rctx2.Model).To(Equal("model2"))
		Expect(rctx2.Message).To(Equal("message2"))
		shouldBlock(func() { rctx.TargetPod() }, 100*time.Millisecond)
		Expect(rctx.targetPod.Load()).To(BeIdenticalTo(nilPod))

		rctx2.SetTargetPod(pod) // unblock
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

	It("should TargetPod unblock successfully", func() {
		ctx := NewRoutingContext(context.Background(), "algorithm", "model", "message", "r1", "")
		ctx.debugDelay = 100 * time.Millisecond
		done := make(chan bool)
		go func() {
			ctx.TargetPod()
			done <- true
		}()

		// Yield to allow TargetPod() pass targetPod nil check.
		time.Sleep(30 * time.Millisecond)

		// Set targetPod before blocking()
		ctx.SetTargetPod(&v1.Pod{})

		// Use Eventually to check if the function completes within a timeout
		Eventually(done, 1*time.Second).Should(Receive(BeTrue()))
	})
})
