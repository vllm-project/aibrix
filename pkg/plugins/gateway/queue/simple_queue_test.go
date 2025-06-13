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
package queue

import (
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/vllm-project/aibrix/pkg/types"
)

var _ = Describe("SimpleQueue", func() {
	var (
		queue *SimpleQueue[int]
		cap   int = 10
	)

	BeforeEach(func() {
		queue = NewSimpleQueue[int](cap)
	})

	Describe("Basic Operations", func() {
		It("should enqueue and dequeue items", func() {
			// nolint:errcheck
			queue.Enqueue(1, time.Now())
			// nolint:errcheck
			queue.Enqueue(2, time.Now())

			val, err := queue.Dequeue(time.Now())
			Expect(err).ToNot(HaveOccurred())
			Expect(val).To(Equal(1))

			val, err = queue.Dequeue(time.Now())
			Expect(err).ToNot(HaveOccurred())
			Expect(val).To(Equal(2))

			Expect(queue.Len()).To(Equal(0))
		})

		It("should peek without removal", func() {
			// nolint:errcheck
			queue.Enqueue(42, time.Now())
			val, err := queue.Peek(time.Now(), nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(val).To(Equal(42))
			Expect(queue.Len()).To(Equal(1))
		})

		It("should handle empty queue", func() {
			val, err := queue.Dequeue(time.Now())
			Expect(err).To(BeIdenticalTo(types.ErrQueueEmpty))
			Expect(val).To(BeZero())

			val, err = queue.Peek(time.Now(), nil)
			Expect(err).To(BeIdenticalTo(types.ErrQueueEmpty))
			Expect(val).To(BeZero())
		})
	})

	Describe("Expansion", func() {
		BeforeEach(func() {
			// Fill first
			for i := 0; i < queue.Cap(); i++ {
				// nolint: errcheck
				queue.Enqueue(i+1, time.Now())
			}
		})

		It("should expand when capacity is exceeded", func() {
			initialCap := queue.Cap()
			// nolint: errcheck
			queue.Enqueue(initialCap, time.Now()) // Trigger expansion

			Eventually(queue.Cap()).Should(Equal(initialCap * 2))
			Expect(queue.Len()).To(Equal(initialCap + 1))
		})

		It("should pack elements when utilization is low", func() {
			initialCap := queue.Cap()
			// Dequeue to reduce utilization
			for i := 0; i < initialCap/2; i++ {
				// nolint:errcheck
				queue.Dequeue(time.Now())
			}

			// nolint:errcheck
			queue.Enqueue(initialCap, time.Now()) // Should trigger packing

			Expect(queue.Cap()).To(Equal(initialCap))
			Expect(queue.Len()).To(Equal(initialCap/2 + 1))
		})
	})

	Describe("Concurrency", func() {
		It("should handle concurrent enqueues/dequeues", func() {
			var wg sync.WaitGroup
			const numWorkers = 10
			const perWorker = 100

			// queue = NewSimpleQueue[int](numWorkers * perWorker)

			// Concurrent enqueues
			enqueued := make(chan int, numWorkers*perWorker)
			wg.Add(numWorkers)
			for i := 0; i < numWorkers; i++ {
				go func(n int) {
					defer wg.Done()
					for j := 0; j < perWorker; j++ {
						// nolint: errcheck
						queue.Enqueue(n*perWorker+j+1, time.Now()) // start with 1
						runtime.Gosched()
						enqueued <- n*perWorker + j + 1
					}
				}(i)
			}

			// Concurrent dequeues
			dequeued := make(chan int, numWorkers*perWorker)
			numErrs := int32(0)
			wg.Add(numWorkers)
			for i := 0; i < numWorkers; i++ {
				go func(n int) {
					defer wg.Done()
					for j := 0; j < perWorker; j++ {
						val, err := queue.Dequeue(time.Now())
						if err != nil {
							// This is possible because there may be more dequeue calls than enqueue calls at some time.
							atomic.AddInt32(&numErrs, 1)
							continue
						}
						runtime.Gosched()
						dequeued <- val
					}
				}(i)
			}

			wg.Wait()
			close(enqueued)
			close(dequeued)

			// Mark enqueued values
			received := make(map[int]bool, numWorkers*perWorker+1)
			for val := range enqueued {
				received[val] = false
			}
			// Mark dequeued values
			for val := range dequeued {
				if received[val] == true {
					log.Printf("detected duplicated %d", val)
				}
				received[val] = true
			}
			enqueues := 0
			dequeues := 0
			for i := 0; i < numWorkers*perWorker; i++ {
				if rec, ok := received[i+1]; !ok {
					// Unlikely
					log.Printf("missing value: %d", i+1)
				} else {
					enqueues++
					if rec {
						dequeues++
					}
				}
			}
			// Verify all enqueued
			Expect(enqueues).To(Equal(numWorkers * perWorker))
			expectedErrs := enqueues - dequeues
			// Verify object in queue matches the number of errors.
			Expect(queue.Len()).To(Equal(int(numErrs)))
			// Verify non duplicated dequeue object.
			Expect(int(numErrs)).To(Equal(expectedErrs))
		})

		It("should peek() consistent with len() result", func() {
			var wg sync.WaitGroup
			numWorkers := 10
			const perWorker = 100

			// queue = NewSimpleQueue[int](numWorkers * perWorker)

			// Concurrent enqueues
			wg.Add(numWorkers)
			for i := 0; i < numWorkers; i++ {
				go func(n int) {
					defer wg.Done()
					for j := 0; j < perWorker; j++ {
						// nolint: errcheck
						queue.Enqueue(n*perWorker+j+1, time.Now()) // start with 1
						runtime.Gosched()
					}
				}(i)
			}

			// Concurrent dequeues
			seen := int32(0)
			numErrs := int32(0)
			wg.Add(numWorkers)
			for i := 0; i < numWorkers; i++ {
				go func(n int) {
					defer wg.Done()
					for j := 0; j < perWorker; j++ {
						if queue.Len() > 0 {
							_, err := queue.Peek(time.Now(), nil)
							if err != nil {
								// This is possible because there may be more dequeue calls than enqueue calls at some time.
								atomic.AddInt32(&numErrs, 1)
								continue
							}
							runtime.Gosched()
							atomic.AddInt32(&seen, 1)
						}
					}
				}(i)
			}

			wg.Wait()

			// Verify all enqueued
			Expect(queue.Len()).To(Equal(numWorkers * perWorker))
			// Verify non duplicated dequeue object.
			Expect(seen > 0).To(BeTrue())
			Expect(numErrs).To(Equal(int32(0)))
		})

		It("should maintain consistency under load", func() {
			const total = 1000
			capacity := queue.Cap()
			var wg sync.WaitGroup

			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < total; i++ {
					// nolint: errcheck
					queue.Enqueue(i, time.Now())
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < total; i++ {
					// nolint: errcheck
					queue.Dequeue(time.Now())
				}
			}()

			Consistently(queue.Cap(), 1*time.Second).Should(Equal(capacity))
			wg.Wait()
		})
	})

	Describe("Edge Cases", func() {
		It("should not handle nil values", func() {
			q := NewSimpleQueue[*int](2)
			// nolint: errcheck
			err := q.Enqueue(nil, time.Now())
			Expect(err).To(BeIdenticalTo(ErrZeroValueNotSupported))
		})

		It("should initialize with correct capacity", func() {
			q := NewSimpleQueue[string](0)
			Expect(q.Cap()).To(Equal(types.DefaultQueueCapacity))
		})
	})
})
