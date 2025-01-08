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
	"sync"
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func newTraceCache() *Cache {
	return &Cache{
		initialized:     true,
		requestTrace:    map[string]*RequestTrace{},
		pendingRequests: sync.Map{},
	}
}

func TestCache(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cache Suite")
}

var _ = Describe("Cache", func() {
	It("should basic add request count, add request trace no err", func() {
		modelName := "llama-7b"
		cache := newTraceCache()
		cache.AddRequestCount("no use now", modelName)
		Expect(len(cache.requestTrace)).To(Equal(1))
		trace := cache.getRequestTrace(modelName)
		Expect(trace).ToNot(BeNil())
		Expect(trace.numKeys).To(Equal(int32(0)))
		Expect(trace.numRequests).To(Equal(int32(1)))
		Expect(trace.pendingRequests).To(Equal(int32(1)))
		pPendingCounter, exist := cache.pendingRequests.Load(modelName)
		Expect(exist).To(BeTrue())
		Expect(*pPendingCounter.(*int32)).To(Equal(int32(1)))

		cache.AddRequestTrace("no use now", modelName, 1, 1)
		Expect(len(cache.requestTrace)).To(Equal(1))
		trace = cache.getRequestTrace(modelName)
		Expect(trace).ToNot(BeNil())
		Expect(trace.numKeys).To(Equal(int32(1)))
		pProfileCounter, exist := trace.trace.Load("0:0") // log2(1)
		Expect(exist).To(BeTrue())
		Expect(*pProfileCounter.(*int32)).To(Equal(int32(1)))
		Expect(trace.numRequests).To(Equal(int32(1)))
		Expect(trace.pendingRequests).To(Equal(int32(0)))
		pPendingCounter, exist = cache.pendingRequests.Load(modelName)
		Expect(exist).To(BeTrue())
		Expect(*pPendingCounter.(*int32)).To(Equal(int32(0)))
	})
})
