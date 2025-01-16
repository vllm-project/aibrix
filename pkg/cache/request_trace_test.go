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
	"encoding/json"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("reqeustTrace", func() {
	It("should NewRequestTrace return recycled RequestTrace with value reset.", func() {
		// Ensure a independent pool
		newRequestTrace := newRequestTraceGen(&sync.Pool{})

		expectedEmptyTrace := newRequestTrace(0)

		ts := time.Now().UnixNano()
		oldTrace := newRequestTrace(ts)
		term, _ := oldTrace.AddRequest("no use now", "no use now")
		Expect(term).To(Equal(ts))
		oldTrace.AddRequest("no use now", "no use now")
		oldTrace.DoneRequest("no use now", term)
		oldTrace.AddRequestTrace("no use now", "1:1")
		Expect(oldTrace.numRequests).ToNot(Equal(int32(0)))
		oldTraceMap := oldTrace.trace
		oldTrace.Recycle()

		newTrace := newRequestTrace(0)
		Expect(newTrace).To(BeIdenticalTo(oldTrace))             // Address should equal
		Expect(newTrace.trace).ToNot(BeIdenticalTo(oldTraceMap)) // trace should be reset
		// function cannot be compared, so set them to nil
		newTrace.recycler = nil
		expectedEmptyTrace.recycler = nil
		Expect(newTrace).To(Equal(expectedEmptyTrace)) // Values should equal
	})

	It("should ToMap return expected record.", func() {
		trace := NewRequestTrace(0)
		trace.AddRequest("no use now", "no use now")
		trace.DoneRequest("no use now", 0)
		trace.AddRequestTrace("no use now", "1:1")
		traceMap := trace.ToMap(2)
		expected := []byte("{\"1:1\":1,\"meta_interval_sec\":10,\"meta_pending_reqs\":2,\"meta_precision\":10,\"meta_total_reqs\":1,\"meta_v\":3}")
		marshaled, err := json.Marshal(traceMap)
		Expect(err).To(BeNil())
		Expect(marshaled).To(Equal(expected))
	})

	It("should pending requests should not negative.", func() {
		trace := NewRequestTrace(0)
		trace.AddRequest("no use now", "no use now")
		trace.DoneRequest("no use now", 0)
		trace.DoneRequest("no use now", 0)
		// TODO: Since in window pending requests are not used in this version, this test will never fail.
		traceMap := trace.ToMap(0)
		Expect(traceMap[MetaKeyPendingRequests.ToString()]).To(Equal(0))
	})

	It("during RequestTrace switch, no trace should lost.", func() {
		for i := 0; i < 10; i++ { // Repeat N times to increase problem rate
			total := 1000000
			trace := NewRequestTrace(time.Now().UnixNano())
			traces := make([]*RequestTrace, 0, 10)
			traces = append(traces, trace)
			done := make(chan struct{})
			var lastTrace *RequestTrace
			tracesSeen := 0
			// start := time.Now()
			go func() {
				for j := 0; j < total; j++ {
					if trace != lastTrace {
						lastTrace = trace
						tracesSeen++
					}
					// Retry until success
					term, success := trace.AddRequest("no use now", "no use now")
					for !success {
						term, success = trace.AddRequest("no use now", "no use now")
					}
					// Retry until success
					for !trace.DoneRequestTrace("no use now", "1:1", term) {
					}
				}
				close(done)
			}()
			go func() {
				for {
					time.Sleep(2 * time.Millisecond)
					select {
					case <-done:
						return
					default:
						oldTrace := trace
						trace = NewRequestTrace(time.Now().UnixNano())
						oldTrace.Lock()
						oldTrace.recycler = nil // simulate recycling.
						oldTrace.Unlock()
						traces = append(traces, trace)
					}
				}
			}()
			<-done
			// duration := time.Since(start)
			// print(duration)
			Expect(tracesSeen > 1).To(BeTrue())

			requests := int32(0)
			profiles := int32(0)
			for _, trace := range traces {
				requests += trace.numRequests
				Expect(trace.completedRequests <= trace.numRequests).To(BeTrue())
				trace.trace.Range(func(_, num any) bool {
					profiles += *num.(*int32)
					return true
				})
			}
			Expect(requests).To(Equal(int32(total)))
			Expect(profiles).To(Equal(int32(total)))
		}
	})
})
