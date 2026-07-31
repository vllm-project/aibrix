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
package utils

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("SyncMap", func() {
	var syncMap *SyncMap[string, int] // Assume SyncMap is the type in sync_map.go

	BeforeEach(func() {
		syncMap = &SyncMap[string, int]{} // Reset sync map
	})

	It("should Store set a value and update length if necessary", func() {
		key := "testKey"

		syncMap.Store(key, 1)

		val, loaded := syncMap.Load(key)
		Expect(loaded).To(BeTrue())
		Expect(val).To(Equal(1))
		Expect(syncMap.Len()).To(Equal(1))

		syncMap.Store(key, 2)

		val, loaded = syncMap.Load(key)
		Expect(loaded).To(BeTrue())
		Expect(val).To(Equal(2))
		Expect(syncMap.Len()).To(Equal(1))
	})

	It("should Delete remove a value and update length correctly", func() {
		key := "testKey"

		syncMap.Delete(key)

		_, loaded := syncMap.Load(key)
		Expect(loaded).To(BeFalse())
		Expect(syncMap.Len()).To(Equal(0))

		syncMap.Store(key, 1)
		syncMap.Delete(key)

		_, loaded = syncMap.Load(key)
		Expect(loaded).To(BeFalse())
		Expect(syncMap.Len()).To(Equal(0))
	})
})
