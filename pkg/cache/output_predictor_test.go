//go:build !race

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
package cache

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// var _ = Describe("OutputPredictor", func() {
// 	var predictor *SimpleOutputPredictor

// 	BeforeEach(func() {
// 		predictor = NewSimpleOutputPredictor(32, 32, 60*time.Second)
// 	})

// 	Context("when initializing OutputPredictor", func() {
// 		It("should create a new instance", func() {
// 			Expect(predictor).ToNot(BeNil())
// 		})
// 	})

// 	// Add more test cases here based on the actual methods of OutputPredictor
// })

func getWeightedDistribution(fn func() int, weights []int) (distribution []int) {
	distribution = make([]int, len(weights)+1)
	total := 1000
	unit_weights := 0
	for i := 0; i < total; i++ {
		distribution[fn()+1]++
	}
	for i := 0; i < len(weights); i++ {
		unit_weights += weights[i]
	}
	distribution[0] = total / unit_weights
	return distribution
}

var _ = Describe("SimpleOutputPredictor", func() {
	var (
		p   *SimpleOutputPredictor
		now time.Time
	)

	Describe("Initialization", func() {
		It("should initialize with correct buckets and history size", func() {
			p = NewSimpleOutputPredictor(8, 16, 60*time.Second)
			Expect(len(p.history.window)).To(Equal(7)) // 60/10 + 1
			Expect(len(p.history.Head())).To(Equal(4*5 + 1))
			Expect(p.history.size).To(Equal(int32(0)))
			Expect(len(p.inputs)).To(Equal(4 * 5))
			Expect(len(p.inputsSums)).To(Equal(4))
			Expect(p.inputBuckets).To(Equal(4))
			Expect(p.outputBuckets).To(Equal(5))
		})

		It("should initialize with correct buckets size with non-aligned window size", func() {
			p = NewSimpleOutputPredictor(8, 16, 65*time.Second)
			Expect(len(p.history.window)).To(Equal(8)) // cel(65/10) + 1
		})
	})

	// Helper methods to access private functions for testing
	Context("Test Helpers", func() {
		BeforeEach(func() {
			p = NewSimpleOutputPredictor(8, 16, 60*time.Second)
		})

		It("bucket2idx should compute index correctly", func() {
			// First
			Expect(p.bucket2idx(0, 0)).To(Equal(0))
			// Last
			Expect(p.bucket2idx(3, 4)).To(Equal(19))
			// Middle
			Expect(p.bucket2idx(1, 2)).To(Equal(1*5 + 2))
		})

		It("token2bucket should round bucket correctly", func() {
			bucket := p.token2bucket(4, p.inputBuckets)
			Expect(bucket).To(Equal(2))

			bucket = p.token2bucket(5, p.inputBuckets)
			Expect(bucket).To(Equal(2))

			bucket = p.token2bucket(6, p.inputBuckets)
			Expect(bucket).To(Equal(3))

			bucket = p.token2bucket(7, p.inputBuckets)
			Expect(bucket).To(Equal(3))
		})

		It("token2bucket should handle zero input tokens", func() {
			bucket := p.token2bucket(0, p.inputBuckets)
			Expect(bucket).To(Equal(0))
		})

		It("token2bucket should clamp large input tokens to max bucket", func() {
			p = NewSimpleOutputPredictor(8, 16, 30*time.Second)
			bucket := p.token2bucket(1000, p.inputBuckets)
			maxBucket := p.inputBuckets - 1
			Expect(bucket).To(Equal(maxBucket))
		})
	})

	Describe("Adding Traces", func() {

		BeforeEach(func() {
			p = NewSimpleOutputPredictor(8, 16, 60*time.Second)
		})

		It("should update the correct distribution index", func() {
			p.AddTrace(1, 1, 1) // inputBucket 0, outputBucket 0

			idx := p.bucket2idx(0, 0)
			Expect(atomic.LoadInt32(&p.inputs[idx])).To(Equal(int32(1)))

			inputBucket := p.token2bucket(1, p.inputBuckets)
			Expect(atomic.LoadInt32(&p.inputsSums[inputBucket])).To(Equal(int32(1)))
		})

		It("should handle multiple traces for same input", func() {
			p.AddTrace(1, 1, 2)
			p.AddTrace(1, 2, 3)

			idx1 := p.bucket2idx(0, 0)
			idx2 := p.bucket2idx(0, 1)
			Expect(atomic.LoadInt32(&p.inputs[idx1])).To(Equal(int32(2)))
			Expect(atomic.LoadInt32(&p.inputs[idx2])).To(Equal(int32(3)))

			inputBucket := p.token2bucket(1, p.inputBuckets)
			Expect(atomic.LoadInt32(&p.inputsSums[inputBucket])).To(Equal(int32(5)))
		})
	})

	Describe("Prediction", func() {
		BeforeEach(func() {
			p = NewSimpleOutputPredictor(8, 16, 60*time.Second)
			now = p.history.headTimestamp
		})

		It("should not panic on predict with no history", func() {
			var output int
			Expect(func() {
				output = p.Predict(16)
			}).ShouldNot(Panic())

			Expect(output).To(Equal(1)) // Default to optimistic result
		})

		It("should predict the only output", func() {
			p.rand = rand.New(rand.NewSource(1)).Int31n // Seed for deterministic test
			p.AddTrace(2, 3, 1)

			result := p.Predict(2)
			Expect(result).To(Equal(4)) // Output round to 2^N

			p.AddTraceWithTimestamp(2, 4, 1, now.Add(12*time.Second)) // Trigger rotation

			Expect(p.Predict(2)).To(Equal(4))
		})

		It("should handle weighted random selection", func() {
			p.rand = rand.New(rand.NewSource(42)).Int31n // Seed for deterministic test
			p.AddTrace(1, 1, 2)                          // Weight 2
			p.AddTrace(1, 2, 1)                          // Weight 1

			// With seed 42, the cursor should pick the second option
			weights := []int{2, 1}
			distributions := getWeightedDistribution(func() int { return p.token2bucket(p.Predict(1), p.outputBuckets) }, weights)
			baseline := float64(distributions[0])
			distributions = distributions[1:]
			for i := 0; i < len(distributions); i++ {
				Expect((float64(distributions[i])/float64(weights[i])-baseline)/baseline < 0.1).To(BeTrue())
			}
		})
	})

	Describe("Rotation", func() {
		BeforeEach(func() {
			p = NewSimpleOutputPredictor(8, 16, 30*time.Second)
			p.testing = true // Enable testWait
			now = p.history.headTimestamp
		})

		It("should not remove data from summary within window", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			Consistently(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(5)))
			Expect(p.history.size).To(Equal(int32(0)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(0))) // First bucket have no skipped set

			p.AddTraceWithTimestamp(1, 1, 5, now.Add(12*time.Second))
			Consistently(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))
			Expect(p.history.size).To(Equal(int32(1)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(1)))

			p.AddTraceWithTimestamp(1, 1, 5, now.Add(21*time.Second))
			Consistently(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(15)))
			Expect(p.history.size).To(Equal(int32(2)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(1)))
		})

		It("should remove old data from summary: exist 1, pass window", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(35*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(5)))
			Expect(p.history.size).To(Equal(int32(0)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(3)))
		})

		It("should remove old data from summary: exist 2, pass window - 1", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(11*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(35*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))
			Expect(p.history.size).To(Equal(int32(2)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(2)))
		})

		It("should remove old data from summary: exist window, pass 1", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(11*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(21*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(35*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(15)))
			Expect(p.history.size).To(Equal(int32(2)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(1)))
		})

		It("should remove old data from summary: exist window, pass window - 1", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(11*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(21*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(45*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))
			Expect(p.history.size).To(Equal(int32(2)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(2)))
		})

		It("should remove old data from summary: exist window, pass window", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(11*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(21*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(55*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(5)))
			Expect(p.history.size).To(Equal(int32(0)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(3)))
		})

		It("should remove old data from summary: exist window, pass window + 1", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(11*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(21*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(65*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(5)))
			Expect(p.history.size).To(Equal(int32(0)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(4)))
		})

		It("should remove old data from summary: exist window + 1, pass window - 1", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(11*time.Second))

			// Wait for exeuction
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(1)))
			oldHead := p.history.head

			p.AddTraceWithTimestamp(1, 1, 5, now.Add(21*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(31*time.Second))

			// Wait for exeuction
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(15)))

			p.AddTraceWithTimestamp(1, 1, 5, now.Add(55*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))
			Expect(p.history.size).To(Equal(int32(2)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(2)))
			Expect(p.history.window[oldHead].getSkipped()).To(Equal(int32(0))) // Ensure skip flag cleared.
		})

		It("should remove old data from summary: exist window + 2, pass window - 1", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(11*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(21*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(31*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(41*time.Second))
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(65*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))
			Expect(p.history.size).To(Equal(int32(2)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(2)))
		})

		It("should remove old data from summary: exist 1, 0, 1, pass 1", func() {
			// log.Println("should remove old data from summary: exist 1, 0, 1, pass 1")
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(22*time.Second))

			// Wait for exeuction
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))

			p.AddTraceWithTimestamp(1, 1, 5, now.Add(35*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))
			Expect(p.history.size).To(Equal(int32(1)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(1)))
		})

		It("should remove old data from summary: exist 1, 0, 1, pass window - 1", func() {
			p.AddTraceWithTimestamp(1, 1, 5, now)
			p.AddTraceWithTimestamp(1, 1, 5, now.Add(22*time.Second))

			// Wait for exeuction
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))

			p.AddTraceWithTimestamp(1, 1, 5, now.Add(45*time.Second)) // Rotate

			// After rotation, initial data should be removed
			// Verify sum for inputBucket 0 is updated correctly
			Eventually(func() int32 {
				return atomic.LoadInt32(&p.inputsSums[0])
			}, 100*time.Millisecond).Should(Equal(int32(10)))
			Expect(p.history.size).To(Equal(int32(2)))
			Expect(p.history.Head().getSkipped()).To(Equal(int32(2)))
		})
	})

	Describe("Concurrency", func() {
		It("should handle concurrent AddTrace and Predict calls", func() {
			p = NewSimpleOutputPredictor(1024, 2048, time.Minute)
			var wg sync.WaitGroup
			wg.Add(2)

			go func() {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					p.AddTrace(i%256+1, i%512+1, 1)
				}
			}()

			go func() {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					p.Predict(i%256 + 1)
				}
			}()

			wg.Wait()
		})
	})
})
